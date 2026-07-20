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

func TestDetectInFlightWorkflow(t *testing.T) {
	cases := []struct {
		name       string
		lines      []string
		wantRunID  string // "" means: want nil (no in-flight workflow)
		wantScript string
	}{
		{
			name:       "in-flight launch is detected with runId and scriptPath",
			lines:      []string{plainLine, launchA, plainLine},
			wantRunID:  wfFixtureRunA,
			wantScript: wfFixtureScriptA,
		},
		{
			name:  "no workflow in transcript",
			lines: []string{plainLine, plainLine},
		},
		{
			name:  "completed workflow is not resumed",
			lines: []string{plainLine, launchA, plainLine, terminalA},
		},
		{
			name:  "queue-operation notification also counts as terminal",
			lines: []string{launchB, terminalB},
		},
		{
			name:       "latest completed but earlier still in flight resumes the earlier one",
			lines:      []string{launchA, launchB, terminalB},
			wantRunID:  wfFixtureRunA,
			wantScript: wfFixtureScriptA,
		},
		{
			name:       "most recent launch wins when several are in flight",
			lines:      []string{launchA, launchB},
			wantRunID:  "wf_bbbb2222-bbb",
			wantScript: `C:\store\projects\D--proj\11111111-1111-1111-1111-111111111111\workflows\scripts\lane-b-wf_bbbb2222-bbb.js`,
		},
		{
			name:  "quoted launch text without coordinates is ignored",
			lines: []string{quotedLaunch},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectInFlightWorkflow(writeTranscript(t, tc.lines...))
			if tc.wantRunID == "" {
				if got != nil {
					t.Fatalf("expected no in-flight workflow, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected in-flight workflow %s, got nil", tc.wantRunID)
			}
			if got.runID != tc.wantRunID {
				t.Fatalf("runID = %q, want %q", got.runID, tc.wantRunID)
			}
			if got.scriptPath != tc.wantScript {
				t.Fatalf("scriptPath = %q, want %q", got.scriptPath, tc.wantScript)
			}
		})
	}

	t.Run("unreadable transcript is best-effort nil", func(t *testing.T) {
		if got := detectInFlightWorkflow(filepath.Join(t.TempDir(), "missing.jsonl")); got != nil {
			t.Fatalf("expected nil for missing transcript, got %+v", got)
		}
	})
}

func TestAugmentPromptForWorkflow(t *testing.T) {
	const generic = "Continue the previous task from exactly where it left off. Ignore hook housekeeping notices."

	t.Run("nil workflow leaves the prompt unmodified", func(t *testing.T) {
		if got := augmentPromptForWorkflow(generic, nil); got != generic {
			t.Fatalf("prompt modified without a workflow: %q", got)
		}
	})

	t.Run("in-flight workflow appends the resume instruction", func(t *testing.T) {
		wf := detectInFlightWorkflow(writeTranscript(t, launchA))
		if wf == nil {
			t.Fatal("fixture launch not detected")
		}
		got := augmentPromptForWorkflow(generic, wf)
		if !strings.HasPrefix(got, generic) {
			t.Fatalf("augmented prompt does not keep the original prompt as prefix: %q", got)
		}
		for _, want := range []string{
			"A Workflow was in progress",
			"runId " + wfFixtureRunA,
			"scriptPath " + wfFixtureScriptA,
			`resumeFromRunId: "` + wfFixtureRunA + `"`,
			"Resume it FIRST",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("augmented prompt missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("completed workflow yields the generic prompt", func(t *testing.T) {
		wf := detectInFlightWorkflow(writeTranscript(t, launchA, terminalA))
		if got := augmentPromptForWorkflow(generic, wf); got != generic {
			t.Fatalf("prompt augmented for a completed workflow: %q", got)
		}
	})
}
