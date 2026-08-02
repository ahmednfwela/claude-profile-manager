package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture lines mirror the REAL transcript shapes (verified against a live
// session jsonl): the launch is a user record whose tool_result content is the
// Workflow tool's text ("Workflow launched in background. Task ID: ...\n...
// Script file: <path>\n...Run ID: wf_...\n..."), and the terminal signal is a
// later record carrying "<task-id>...</task-id>" inside a <task-notification>.
const (
	wfFixtureRunA    = "wf_aaaa1111-aaa"
	wfFixtureScriptA = `C:\store\projects\D--proj\11111111-1111-1111-1111-111111111111\workflows\scripts\lane-a-wf_aaaa1111-aaa.js`

	launchA = `{"parentUuid":"p1","type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01A","type":"tool_result","content":"Workflow launched in background. Task ID: wtaskaaa1\nSummary: fixture lane A\nTranscript dir: C:\\store\\projects\\D--proj\\11111111-1111-1111-1111-111111111111\\subagents\\workflows\\wf_aaaa1111-aaa\nScript file: C:\\store\\projects\\D--proj\\11111111-1111-1111-1111-111111111111\\workflows\\scripts\\lane-a-wf_aaaa1111-aaa.js\n(Edit this file with Write/Edit and re-invoke Workflow with {scriptPath: \"...\"} to iterate without resending the script.)\nRun ID: wf_aaaa1111-aaa\nTo resume after editing the script: Workflow({scriptPath: \"...\"})"}]}}`

	launchB = `{"parentUuid":"p2","type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01B","type":"tool_result","content":"Workflow launched in background. Task ID: wtaskbbb2\nSummary: fixture lane B\nTranscript dir: C:\\store\\projects\\D--proj\\11111111-1111-1111-1111-111111111111\\subagents\\workflows\\wf_bbbb2222-bbb\nScript file: C:\\store\\projects\\D--proj\\11111111-1111-1111-1111-111111111111\\workflows\\scripts\\lane-b-wf_bbbb2222-bbb.js\n(Edit this file with Write/Edit and re-invoke Workflow with {scriptPath: \"...\"} to iterate without resending the script.)\nRun ID: wf_bbbb2222-bbb\nTo resume after editing the script: Workflow({scriptPath: \"...\"})"}]}}`

	terminalA = `{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>wtaskaaa1</task-id>\n<tool-use-id>toolu_01A</tool-use-id>\n<status>completed</status>\n<summary>Dynamic workflow \"fixture lane A\" completed</summary>\n</task-notification>"}}`

	terminalB = `{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>wtaskbbb2</task-id>\n<tool-use-id>toolu_01B</tool-use-id>\n<status>completed</status>\n<summary>Dynamic workflow \"fixture lane B\" completed</summary>\n</task-notification>"}`

	plainLine = `{"type":"assistant","cwd":"D:\\projects\\devops-aggregate","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}`

	// A compaction-style quote of the launch text WITHOUT its coordinates must
	// not be mistaken for a launch.
	quotedLaunch = `{"type":"user","message":{"role":"user","content":"Earlier: Workflow launched in background. Task ID: wtaskccc3 (details elided)"}}`
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A session can have several Workflows running at once, so detection returns
// every in-flight launch, oldest first — an earlier "newest launch only" form
// silently dropped the others from the resume instruction.
func TestDetectInFlightWorkflows(t *testing.T) {
	const (
		runB    = "wf_bbbb2222-bbb"
		scriptB = `C:\store\projects\D--proj\11111111-1111-1111-1111-111111111111\workflows\scripts\lane-b-wf_bbbb2222-bbb.js`
	)

	cases := []struct {
		name  string
		lines []string
		want  []string // runIDs, in the order returned
	}{
		{
			name:  "two simultaneously in-flight launches are both returned, oldest first",
			lines: []string{launchA, launchB},
			want:  []string{wfFixtureRunA, runB},
		},
		{
			name:  "a completed launch is filtered out, the in-flight one survives",
			lines: []string{launchA, launchB, terminalB},
			want:  []string{wfFixtureRunA},
		},
		{
			name:  "all completed yields nothing",
			lines: []string{launchA, terminalA, launchB, terminalB},
		},
		{
			name:  "a completed workflow is not resumed",
			lines: []string{plainLine, launchA, plainLine, terminalA},
		},
		{
			name:  "a queue-operation notification also counts as terminal",
			lines: []string{launchB, terminalB},
		},
		{
			name:  "a terminal signal BEFORE the launch does not close it",
			lines: []string{terminalA, launchA},
			want:  []string{wfFixtureRunA},
		},
		{
			name:  "a lone in-flight launch is returned with its coordinates",
			lines: []string{plainLine, launchA, plainLine},
			want:  []string{wfFixtureRunA},
		},
		{
			name:  "no workflow in transcript",
			lines: []string{plainLine, plainLine},
		},
		{
			name:  "quoted launch text without coordinates is ignored",
			lines: []string{quotedLaunch},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectInFlightWorkflows(writeTranscript(t, tc.lines...))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d in-flight workflows, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if got[i].runID != want {
					t.Fatalf("run %d = %q, want %q", i, got[i].runID, want)
				}
			}
		})
	}

	t.Run("scriptPath is carried for every launch", func(t *testing.T) {
		got := detectInFlightWorkflows(writeTranscript(t, launchA, launchB))
		if got[0].scriptPath != wfFixtureScriptA || got[1].scriptPath != scriptB {
			t.Fatalf("scriptPaths not carried: %q / %q", got[0].scriptPath, got[1].scriptPath)
		}
	})

	t.Run("unreadable transcript is best-effort empty", func(t *testing.T) {
		if got := detectInFlightWorkflows(filepath.Join(t.TempDir(), "missing.jsonl")); len(got) != 0 {
			t.Fatalf("expected none for a missing transcript, got %+v", got)
		}
	})
}

func TestAugmentPromptForWorkflows(t *testing.T) {
	const generic = "Continue the previous task from exactly where it left off. Ignore hook housekeeping notices."

	t.Run("no launches leaves the prompt unmodified", func(t *testing.T) {
		if got := augmentPromptForWorkflows(generic, nil); got != generic {
			t.Fatalf("prompt modified without a workflow: %q", got)
		}
		if got := augmentPromptForWorkflows(generic, []workflowLaunch{}); got != generic {
			t.Fatalf("prompt modified for an empty slice: %q", got)
		}
	})

	t.Run("every in-flight run is named with its resume call", func(t *testing.T) {
		launches := detectInFlightWorkflows(writeTranscript(t, launchA, launchB))
		got := augmentPromptForWorkflows(generic, launches)
		if !strings.HasPrefix(got, generic) {
			t.Fatalf("augmented prompt does not keep the original prompt as prefix: %q", got)
		}
		for _, want := range []string{
			"2 Workflows were in progress",
			"runId " + wfFixtureRunA,
			"runId wf_bbbb2222-bbb",
			`resumeFromRunId: "` + wfFixtureRunA + `"`,
			`resumeFromRunId: "wf_bbbb2222-bbb"`,
			`scriptPath: "` + strings.ReplaceAll(wfFixtureScriptA, `\`, `\\`) + `"`,
			"Resume them FIRST",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("augmented prompt missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("states cpm already copied the run dirs rather than asking for self-repair", func(t *testing.T) {
		launches := detectInFlightWorkflows(writeTranscript(t, launchA))
		got := augmentPromptForWorkflows(generic, launches)
		if !strings.Contains(got, "cpm has already copied its run directory") {
			t.Fatalf("prompt does not tell the agent the transplant already happened:\n%s", got)
		}
		// The fallback stays: the transplant lands after the re-dispatch, so a
		// very fast first turn can still beat it.
		if !strings.Contains(got, "If agents start re-running from scratch") {
			t.Fatalf("prompt dropped the cache-miss fallback:\n%s", got)
		}
		if strings.Contains(got, "Resume them FIRST") {
			t.Fatalf("singular case used plural phrasing:\n%s", got)
		}
	})
}
