package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fixture shapes mirror the live estate (read-only):
//   - daemon/roster.json workers.<short>.dispatch.launch =
//     {"mode":"resume","sessionId":<origin uuid>,"fork":true} while
//     workers.<short>.sessionId is the NEW uuid.
//   - a run dir is <session>/subagents/workflows/<runId>/ holding journal.jsonl
//     plus an agent-<id>.jsonl / agent-<id>.meta.json pair per agent call.

const (
	txOriginID = "c22695de-96b4-4bab-9a61-c26d8cd822b2"
	txNewID    = "5faacda6-5395-4fee-99d2-b915db6f7022"
	txOtherID  = "817c16cf-1c8b-414b-8071-5d46a1f2c1d5"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRunDir fabricates one workflow run directory with the real file set.
func writeRunDir(t *testing.T, sessionDir, runID string) string {
	t.Helper()
	dir := filepath.Join(sessionDir, "subagents", "workflows", runID)
	writeFile(t, filepath.Join(dir, "journal.jsonl"),
		`{"type":"started","key":"v2:abc","agentId":"a1"}`+"\n"+
			`{"type":"result","key":"v2:abc","agentId":"a1","result":{"lane":"`+runID+`","status":"PUSHED"}}`+"\n")
	writeFile(t, filepath.Join(dir, "agent-a1.jsonl"), `{"type":"assistant"}`+"\n")
	writeFile(t, filepath.Join(dir, "agent-a1.meta.json"),
		`{"agentType":"workflow-subagent","spawnDepth":1,"model":"sonnet"}`+"\n")
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestTransplantWorkflowRuns(t *testing.T) {
	t.Run("every run dir lands byte-for-byte", func(t *testing.T) {
		base := t.TempDir()
		oldDir := filepath.Join(base, "old-slug", txOriginID)
		newDir := filepath.Join(base, "new-slug", txNewID)
		writeRunDir(t, oldDir, "wf_aaaa1111-aaa")
		writeRunDir(t, oldDir, "wf_bbbb2222-bbb")

		runs, err := transplantWorkflowRuns(oldDir, newDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(runs) != 2 {
			t.Fatalf("copied %d runs, want 2: %+v", len(runs), runs)
		}
		for _, run := range []string{"wf_aaaa1111-aaa", "wf_bbbb2222-bbb"} {
			for _, f := range []string{"journal.jsonl", "agent-a1.jsonl", "agent-a1.meta.json"} {
				src := filepath.Join(oldDir, "subagents", "workflows", run, f)
				dst := filepath.Join(newDir, "subagents", "workflows", run, f)
				if got, want := readFile(t, dst), readFile(t, src); got != want {
					t.Fatalf("%s/%s: got %q, want %q", run, f, got, want)
				}
			}
		}
	})

	t.Run("existing destination run dir is never clobbered", func(t *testing.T) {
		base := t.TempDir()
		oldDir := filepath.Join(base, "old", txOriginID)
		newDir := filepath.Join(base, "new", txNewID)
		writeRunDir(t, oldDir, "wf_collide-111")
		writeRunDir(t, oldDir, "wf_fresh-2222")

		// The resumed session already started its own run under that id.
		live := filepath.Join(newDir, "subagents", "workflows", "wf_collide-111", "journal.jsonl")
		writeFile(t, live, "LIVE-RUN-DO-NOT-TOUCH\n")

		runs, err := transplantWorkflowRuns(oldDir, newDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := readFile(t, live); got != "LIVE-RUN-DO-NOT-TOUCH\n" {
			t.Fatalf("collision clobbered live state: %q", got)
		}
		// A collision on one run must not abort the others.
		if len(runs) != 1 || runs[0].runID != "wf_fresh-2222" {
			t.Fatalf("want only wf_fresh-2222 copied, got %+v", runs)
		}
		if _, err := os.Stat(filepath.Join(newDir, "subagents", "workflows", "wf_fresh-2222", "journal.jsonl")); err != nil {
			t.Fatalf("non-colliding run did not land: %v", err)
		}
	})

	t.Run("no subagents/workflows at all is a silent no-op", func(t *testing.T) {
		base := t.TempDir()
		oldDir := filepath.Join(base, "old", txOriginID)
		newDir := filepath.Join(base, "new", txNewID)
		writeFile(t, filepath.Join(oldDir, "tool-results", "x.json"), "{}\n")

		runs, err := transplantWorkflowRuns(oldDir, newDir)
		if err != nil || runs != nil {
			t.Fatalf("want (nil, nil) for a session that never ran a Workflow, got (%+v, %v)", runs, err)
		}
		if _, err := os.Stat(filepath.Join(newDir, "subagents")); !os.IsNotExist(err) {
			t.Fatalf("no-op created %s/subagents", newDir)
		}
	})

	t.Run("empty subagents/workflows is a no-op", func(t *testing.T) {
		base := t.TempDir()
		oldDir := filepath.Join(base, "old", txOriginID)
		newDir := filepath.Join(base, "new", txNewID)
		if err := os.MkdirAll(filepath.Join(oldDir, "subagents", "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		runs, err := transplantWorkflowRuns(oldDir, newDir)
		if err != nil || len(runs) != 0 {
			t.Fatalf("want no copies, got (%+v, %v)", runs, err)
		}
	})

	t.Run("nested subdirectories inside a run dir are copied", func(t *testing.T) {
		base := t.TempDir()
		oldDir := filepath.Join(base, "old", txOriginID)
		newDir := filepath.Join(base, "new", txNewID)
		writeRunDir(t, oldDir, "wf_nested-111")
		writeFile(t, filepath.Join(oldDir, "subagents", "workflows", "wf_nested-111", "deep", "extra.json"), "{}\n")

		if _, err := transplantWorkflowRuns(oldDir, newDir); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, filepath.Join(newDir, "subagents", "workflows", "wf_nested-111", "deep", "extra.json")); got != "{}\n" {
			t.Fatalf("nested file not copied: %q", got)
		}
	})
}

func TestHasWorkflowRuns(t *testing.T) {
	base := t.TempDir()

	if hasWorkflowRuns(filepath.Join(base, "missing")) {
		t.Fatal("a session dir that does not exist has no workflow runs")
	}

	empty := filepath.Join(base, "empty")
	if err := os.MkdirAll(filepath.Join(empty, "subagents", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if hasWorkflowRuns(empty) {
		t.Fatal("an empty workflows dir has no runs")
	}

	full := filepath.Join(base, "full")
	writeRunDir(t, full, "wf_aaaa1111-aaa")
	if !hasWorkflowRuns(full) {
		t.Fatal("a session with a run dir must report true")
	}
}

// roster renders a daemon/roster.json with the live worker shape.
func writeRoster(t *testing.T, dir string, workers string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "daemon", "roster.json"),
		`{"proto":1,"supervisorPid":1234,"workers":{`+workers+`}}`)
}

func rosterWorker(short, sessionID, mode, resumeOf string, createdAt int64) string {
	return fmt.Sprintf(`"%s":{"pid":1,"sessionId":%q,"cwd":"D:\\projects\\devops-aggregate",`+
		`"dispatch":{"short":%q,"sessionId":%q,"createdAt":%d,"source":"shell",`+
		`"launch":{"mode":%q,"sessionId":%q,"fork":true}}}`,
		short, sessionID, short, sessionID, createdAt, mode, resumeOf)
}

func TestFindResumedWorker(t *testing.T) {
	const now = int64(1785665714305)

	t.Run("picks the worker whose dispatch resumed our session", func(t *testing.T) {
		dir := t.TempDir()
		writeRoster(t, dir, rosterWorker("5faacda6", txNewID, "resume", txOriginID, now)+","+
			rosterWorker("c839111d", txOtherID, "prompt", "", now))
		if got := findResumedWorker(dir, txOriginID, now-1000); got != txNewID {
			t.Fatalf("got %q, want %q", got, txNewID)
		}
	})

	t.Run("a stale entry from an earlier handoff of the same session is excluded", func(t *testing.T) {
		dir := t.TempDir()
		writeRoster(t, dir, rosterWorker("aaaaaaaa", txOtherID, "resume", txOriginID, now-60_000))
		if got := findResumedWorker(dir, txOriginID, now); got != "" {
			t.Fatalf("stale worker returned: %q", got)
		}
	})

	t.Run("the newest qualifying entry wins", func(t *testing.T) {
		dir := t.TempDir()
		writeRoster(t, dir, rosterWorker("aaaaaaaa", txOtherID, "resume", txOriginID, now)+","+
			rosterWorker("5faacda6", txNewID, "resume", txOriginID, now+5000))
		if got := findResumedWorker(dir, txOriginID, now-1000); got != txNewID {
			t.Fatalf("got %q, want the newer %q", got, txNewID)
		}
	})

	t.Run("a resume of a DIFFERENT session is not ours", func(t *testing.T) {
		dir := t.TempDir()
		writeRoster(t, dir, rosterWorker("5faacda6", txNewID, "resume", txOtherID, now))
		if got := findResumedWorker(dir, txOriginID, now-1000); got != "" {
			t.Fatalf("matched an unrelated resume: %q", got)
		}
	})

	t.Run("missing or malformed roster is best-effort empty", func(t *testing.T) {
		if got := findResumedWorker(t.TempDir(), txOriginID, 0); got != "" {
			t.Fatalf("missing roster should yield \"\", got %q", got)
		}
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "daemon", "roster.json"), "{not json")
		if got := findResumedWorker(dir, txOriginID, 0); got != "" {
			t.Fatalf("malformed roster should yield \"\", got %q", got)
		}
	})
}

func TestFindSessionProjectDir(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := findSessionProjectDir(dir, txNewID); err == nil {
		t.Fatal("expected an error when the transcript is absent")
	}

	// The slug is never derived from cwd — here it deliberately does not match
	// the cwd recorded inside the transcript (the worktree-launched case).
	slug := filepath.Join(dir, "projects", "D--projects-devops-aggregate--claude-worktrees-lane-1")
	writeFile(t, filepath.Join(slug, txNewID+".jsonl"),
		`{"type":"attachment","cwd":"D:\\projects\\devops-aggregate"}`+"\n")

	gotDir, gotTranscript, err := findSessionProjectDir(dir, txNewID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDir != slug {
		t.Fatalf("slug dir = %q, want %q", gotDir, slug)
	}
	if want := filepath.Join(slug, txNewID+".jsonl"); gotTranscript != want {
		t.Fatalf("transcript = %q, want %q", gotTranscript, want)
	}
}

func TestWaitForNewSession(t *testing.T) {
	const now = int64(1785665714305)

	t.Run("returns once BOTH the roster entry and the transcript exist", func(t *testing.T) {
		dir := t.TempDir()
		slug := filepath.Join(dir, "projects", "D--projects-devops-aggregate")

		// Roster first, transcript a few polls later — the real ordering.
		writeRoster(t, dir, rosterWorker("5faacda6", txNewID, "resume", txOriginID, now))
		go func() {
			time.Sleep(20 * time.Millisecond)
			os.MkdirAll(slug, 0o755)
			os.WriteFile(filepath.Join(slug, txNewID+".jsonl"), []byte("{}\n"), 0o644)
		}()

		gotID, gotDir, err := waitForNewSession(dir, txOriginID, now-1000, 2*time.Millisecond, 5*time.Second)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotID != txNewID {
			t.Fatalf("session id = %q, want %q", gotID, txNewID)
		}
		if want := filepath.Join(slug, txNewID); gotDir != want {
			t.Fatalf("session dir = %q, want %q", gotDir, want)
		}
	})

	t.Run("a roster entry alone is not enough", func(t *testing.T) {
		dir := t.TempDir()
		writeRoster(t, dir, rosterWorker("5faacda6", txNewID, "resume", txOriginID, now))
		if _, _, err := waitForNewSession(dir, txOriginID, now-1000, 2*time.Millisecond, 30*time.Millisecond); err == nil {
			t.Fatal("expected a timeout while the transcript is still missing")
		}
	})

	t.Run("times out inside its budget instead of hanging", func(t *testing.T) {
		start := time.Now()
		_, _, err := waitForNewSession(t.TempDir(), txOriginID, 0, 2*time.Millisecond, 50*time.Millisecond)
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("wait overran its budget: %s", elapsed)
		}
	})
}
