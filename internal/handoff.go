package internal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	shortIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)
	fullIDRe  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// rosterFile mirrors the fields of <profile>/daemon/roster.json we need to
// resolve a short worker id to its full session UUID.
type rosterFile struct {
	Workers map[string]struct {
		SessionID string `json:"sessionId"`
	} `json:"workers"`
}

// ResolveSessionID accepts either a full session UUID or the 8-char short id
// shown by `claude agents`, and returns (fullID, shortID). Short ids are
// resolved against the source profile's daemon roster.
func ResolveSessionID(id, fromDir string) (string, string, error) {
	if fullIDRe.MatchString(id) {
		return id, id[:8], nil
	}
	if !shortIDRe.MatchString(id) {
		return "", "", fmt.Errorf("session id %q is neither a full UUID nor an 8-char short id", id)
	}
	data, err := os.ReadFile(filepath.Join(fromDir, "daemon", "roster.json"))
	if err != nil {
		return "", "", fmt.Errorf("short id given but source roster unreadable (%v) — pass the full session UUID", err)
	}
	var roster rosterFile
	if err := json.Unmarshal(data, &roster); err != nil {
		return "", "", fmt.Errorf("parse roster.json: %v", err)
	}
	w, ok := roster.Workers[id]
	if !ok || w.SessionID == "" {
		return "", "", fmt.Errorf("short id %q not in source profile's roster — pass the full session UUID", id)
	}
	return w.SessionID, id, nil
}

// runClaudeInline runs a claude invocation and returns instead of exiting the
// process (unlike RunClaude), so HandoffSession can chain two invocations.
// In headless contexts (scheduled tasks, hooks) children are spawned without
// a console window — see hideIfHeadless — so automated handoffs never pop
// terminal windows on Windows.
//
// dir sets the child's working directory. It MUST be the session's original
// project directory for the `--resume` re-dispatch: claude scopes session-id
// lookup to the current directory and its worktrees ("session ID lookup is
// scoped to the current project directory and its git worktrees, so a session
// created elsewhere reports No conversation found" —
// https://code.claude.com/docs/en/sessions). Launched from any other cwd,
// `--bg --resume <id>` fails to find the id and forks a blank session. Pass ""
// to inherit cpm's cwd (used only where scope is irrelevant).
func runClaudeInline(path string, argv, env []string, dir string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	hideIfHeadless(cmd)
	return cmd.Run()
}

// verifyTranscriptVisible checks that the target profile can actually see the
// session transcript before re-dispatching. All profiles are expected to share
// one projects store (junction/symlink); when that wiring is broken, `claude
// --bg --resume` on the target would silently start an EMPTY session instead
// of continuing the conversation — the classic "handoff didn't route" symptom.
func verifyTranscriptVisible(toDir, fullID string) error {
	matches, err := filepath.Glob(filepath.Join(toDir, "projects", "*", fullID+".jsonl"))
	if err == nil && len(matches) > 0 {
		return nil
	}
	return fmt.Errorf("transcript %s.jsonl not visible under %s\\projects — the profiles' projects stores are not shared (junction missing?); refusing to dispatch a blank session", fullID, toDir)
}

// sessionProjectDir returns the working directory the session was created in, by
// reading the `cwd` field from its transcript. Store visibility (a shared
// projects junction) is necessary but NOT sufficient to re-dispatch: because
// `--resume` resolves the id relative to the launch cwd, the re-dispatch must
// run in this directory or claude forks a blank session even though the
// transcript is right there in the store. The first transcript line is a
// `custom-title` record with a null cwd, so scan for the first line that
// actually carries a non-empty cwd. Reversing the project slug is not an option
// — slugging replaces every non-alphanumeric byte with '-' and is lossy — so
// the transcript is the only authoritative source; refuse rather than guess.
func sessionProjectDir(toDir, fullID string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(toDir, "projects", "*", fullID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("transcript %s.jsonl not visible under %s\\projects — the profiles' projects stores are not shared (junction missing?); refusing to dispatch a blank session", fullID, toDir)
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return "", fmt.Errorf("open transcript %s: %w", matches[0], err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024) // transcript lines can be large
	for sc.Scan() {
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue // skip any non-JSON / partial line
		}
		if rec.Cwd != "" {
			return rec.Cwd, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read transcript %s: %w", matches[0], err)
	}
	return "", fmt.Errorf("no cwd found in transcript %s — cannot determine the session's project directory; refusing to re-dispatch where --resume would fork a blank session", matches[0])
}

// ResolveHandoffName picks the name for the re-dispatched session. An explicit
// --name override wins; otherwise the origin session's own name carries over
// verbatim, read from the daemon job state <fromDir>/jobs/<shortID>/state.json
// ("name" is the field `claude agents` shows). Only when the origin has no
// recorded name does it fall back to the "handoff-<short>" slug -- a handoff
// is the same task continuing on another account, so it should keep its name.
func ResolveHandoffName(override, fromDir, shortID string) string {
	if override != "" {
		return override
	}
	if data, err := os.ReadFile(filepath.Join(fromDir, "jobs", shortID, "state.json")); err == nil {
		var st struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &st) == nil && st.Name != "" {
			return st.Name
		}
	}
	return "handoff-" + shortID
}

// workflowLaunch is one Workflow-tool launch found in a session transcript.
// The launch tool_result prints (verified against real transcripts):
//
//	Workflow launched in background. Task ID: <taskID>
//	...
//	Script file: <scriptPath>
//	...
//	Run ID: wf_<...>
//
// and the workflow's terminal signal is a later <task-notification> line
// carrying the same task id in a <task-id> tag.
type workflowLaunch struct {
	line       int
	taskID     string
	runID      string
	scriptPath string
}

var (
	wfLaunchTaskRe = regexp.MustCompile(`Workflow launched in background\. Task ID: ([A-Za-z0-9_-]+)`)
	wfRunIDRe      = regexp.MustCompile(`Run ID: (wf_[a-z0-9-]+)`)
	wfScriptRe     = regexp.MustCompile(`(?m)^Script file: (.+?)\r?$`)
)

// collectStrings appends every string value reachable in a decoded JSON value,
// so launch text is found whether the tool_result content is a plain string, a
// list of content blocks, or echoed in a toolUseResult / queue-operation record.
func collectStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			collectStrings(e, out)
		}
	case map[string]any:
		for _, e := range t {
			collectStrings(e, out)
		}
	}
}

// detectInFlightWorkflow scans a session transcript for the most recent
// Workflow-tool launch that has no later terminal notification. A Workflow runs
// as a detached process that dies when the session is stopped, and --resume
// alone restores only the transcript -- so the handoff prompt must carry the
// resume coordinates or the run is silently dropped.
//
// Best-effort by design: any read/parse problem returns nil and the handoff
// proceeds unaugmented. On a partial read the launches seen so far still count:
// resuming an already-finished workflow is a cheap cache no-op, while dropping
// a live one is the data loss this exists to prevent.
func detectInFlightWorkflow(transcriptPath string) *workflowLaunch {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	const launchMarker = "Workflow launched in background. Task ID: "
	var launches []workflowLaunch
	seen := map[string]bool{}    // launch taskID -> already recorded
	terminal := map[string]int{} // taskID -> line its terminal notification was seen on

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024) // transcript lines can be large
	for i := 0; sc.Scan(); i++ {
		raw := sc.Bytes()
		if bytes.Contains(raw, []byte(launchMarker)) {
			var rec any
			if json.Unmarshal(raw, &rec) == nil {
				var texts []string
				collectStrings(rec, &texts)
				for _, txt := range texts {
					if !strings.Contains(txt, launchMarker) {
						continue
					}
					task := wfLaunchTaskRe.FindStringSubmatch(txt)
					run := wfRunIDRe.FindStringSubmatch(txt)
					script := wfScriptRe.FindStringSubmatch(txt)
					if task == nil || run == nil || script == nil {
						continue // e.g. a compaction summary quoting the launch text without its coordinates
					}
					if seen[task[1]] {
						continue
					}
					seen[task[1]] = true
					launches = append(launches, workflowLaunch{
						line: i, taskID: task[1], runID: run[1],
						scriptPath: strings.TrimSpace(script[1]),
					})
				}
			}
		}
		// Terminal signal: the workflow's <task-notification> carries the same
		// task id its launch printed. Task ids are alphanumeric, so JSON string
		// escaping cannot split them -- raw containment is safe.
		for _, l := range launches {
			needle := "<task-id>" + l.taskID + "</task-id>"
			if bytes.Contains(raw, []byte(needle)) {
				terminal[l.taskID] = i
			}
		}
	}
	for i := len(launches) - 1; i >= 0; i-- {
		if t, ok := terminal[launches[i].taskID]; !ok || t <= launches[i].line {
			return &launches[i]
		}
	}
	return nil
}

// augmentPromptForWorkflow appends the resume instruction for an in-flight
// Workflow to the re-dispatch prompt. The journal caution matters: Workflow
// resume looks the run's journal.jsonl up in the CURRENT session's transcript
// directory, and a cache miss silently re-runs every agent from scratch
// (observed: a naive handoff re-ran a 29/31-agent run in full).
func augmentPromptForWorkflow(prompt string, wf *workflowLaunch) string {
	if wf == nil {
		return prompt
	}
	return fmt.Sprintf("%s A Workflow was in progress (runId %s, scriptPath %s). "+
		"Resume it FIRST via Workflow({scriptPath: %q, resumeFromRunId: %q}) before anything else. "+
		"Then verify the resume hit the run's cache: its journal.jsonl must list the previously "+
		"completed agents as cached results. If agents start re-running from scratch, the journal "+
		"was not found in this session's transcript directory - stop the run, copy the prior run's "+
		"journal.jsonl and agent transcripts into the new run directory, and resume again.",
		prompt, wf.runID, wf.scriptPath, wf.scriptPath, wf.runID)
}

// HandoffSession moves a background session between profiles/accounts: it
// stops the worker on the source profile, then re-dispatches the conversation
// as a new background session on the target profile via `claude --bg --resume`.
//
// This relies on the shared projects store (all profiles junction projects/ to
// one directory), so the transcript is already visible to the target profile.
// The stop MUST happen first: two profiles resuming one session means two
// processes appending the same transcript file.
//
// An empty name resolves via ResolveHandoffName (origin session's own name,
// else "handoff-<short>"); a non-empty name is the --name override and wins.
func HandoffSession(fromName, fromDir string, fromProfile *Profile, toName, toDir string, toProfile *Profile, sessionID, prompt, name string) error {
	fullID, shortID, err := ResolveSessionID(sessionID, fromDir)
	if err != nil {
		return err
	}
	name = ResolveHandoffName(name, fromDir, shortID)

	// Resolve the session's project directory up front and fail fast: this both
	// proves the transcript is visible (shared projects store) AND yields the
	// cwd the --resume re-dispatch must run in. Doing it before the stop means a
	// mis-wired store aborts the handoff without needlessly killing the source
	// worker.
	projectDir, err := sessionProjectDir(toDir, fullID)
	if err != nil {
		return err
	}

	// Stop on the source profile. Best-effort: the session may already be
	// stopped (that's the typical rate-limited state) or finished. Run it in the
	// session's project dir so the daemon subcommand resolves the same scope.
	stopPath, stopArgv, stopEnv, err := BuildRunInvocation(fromName, fromDir, fromProfile, []string{"stop", shortID})
	if err != nil {
		return err
	}
	fmt.Printf("stopping %s on profile %q...\n", shortID, fromName)
	if err := runClaudeInline(stopPath, stopArgv, stopEnv, projectDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: stop on %q failed (%v) — continuing; verify no live worker still owns the session\n", fromName, err)
	}

	// A Workflow the origin session had in flight died with the stop above;
	// --resume restores the transcript only, so the prompt must instruct the
	// resumed session to pick the run back up. Best-effort: a failed scan
	// changes nothing and the handoff proceeds with the prompt as given.
	if matches, err := filepath.Glob(filepath.Join(toDir, "projects", "*", fullID+".jsonl")); err == nil && len(matches) > 0 {
		if wf := detectInFlightWorkflow(matches[0]); wf != nil {
			fmt.Printf("in-flight workflow %s detected - augmenting the re-dispatch prompt to resume it\n", wf.runID)
			prompt = augmentPromptForWorkflow(prompt, wf)
		}
	}

	// Re-dispatch on the target profile. Decoration (profile Args like
	// --dangerously-skip-permissions) is wanted here: --bg forwards it into
	// the worker's launch args and respawnFlags. The re-dispatch MUST run in the
	// session's project dir — claude scopes --resume id lookup to the launch cwd,
	// so any other directory forks a blank session (see runClaudeInline).
	bgArgs := []string{"--bg", prompt, "--resume", fullID, "--name", name}
	bgPath, bgArgv, bgEnv, err := BuildRunInvocation(toName, toDir, toProfile, bgArgs)
	if err != nil {
		return err
	}
	fmt.Printf("re-dispatching %s on profile %q as %q (cwd %s)...\n", fullID, toName, name, projectDir)
	return runClaudeInline(bgPath, bgArgv, bgEnv, projectDir)
}
