package internal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	shortIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)
	fullIDRe  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// How long to wait for the re-dispatched session to appear on disk before
// giving up on the workflow transplant. `claude --bg` returns as soon as the
// supervisor accepts the dispatch, so the roster entry and the new transcript
// land shortly AFTER cpm's child exits — hence a poll rather than a one-shot
// read. Only paid when the origin session actually has workflow runs.
const (
	newSessionPollInterval = 250 * time.Millisecond
	newSessionPollTimeout  = 30 * time.Second
)

// rosterFile mirrors the fields of <profile>/daemon/roster.json we need to
// resolve a short worker id to its full session UUID, and to identify the
// worker a `--bg --resume` re-dispatch created.
//
// Shapes verified against a live roster: a resume dispatch records
//
//	workers.<short>.sessionId          -> the NEW session's UUID
//	workers.<short>.dispatch.createdAt -> epoch ms
//	workers.<short>.dispatch.launch    -> {"mode":"resume","sessionId":<ORIGIN uuid>,"fork":true}
//
// This pair is the only place on disk that names both the old and the new
// session, so it is the ground truth for "which session did my handoff make?".
// jobs/<short>/state.json cannot answer it: there both sessionId and
// resumeSessionId hold the worker's OWN id, not the one it resumed from.
type rosterFile struct {
	Workers map[string]struct {
		SessionID string `json:"sessionId"`
		Dispatch  struct {
			CreatedAt int64 `json:"createdAt"`
			Launch    struct {
				Mode      string `json:"mode"`
				SessionID string `json:"sessionId"`
			} `json:"launch"`
		} `json:"dispatch"`
	} `json:"workers"`
}

// findResumedWorker returns the full session id of the worker that `claude --bg
// --resume fromFullID` created on the target profile, or "" if it has not
// appeared yet.
//
// notBeforeMs excludes roster entries left over from an EARLIER handoff of the
// same origin session (workers linger until the daemon prunes them), so a
// re-handoff never transplants into a stale session. When several entries
// qualify the newest wins.
//
// Best-effort: an unreadable or unparsable roster returns "" so the caller
// keeps polling rather than failing the handoff, which has already succeeded by
// this point.
func findResumedWorker(toDir, fromFullID string, notBeforeMs int64) string {
	data, err := os.ReadFile(filepath.Join(toDir, "daemon", "roster.json"))
	if err != nil {
		return ""
	}
	var roster rosterFile
	if json.Unmarshal(data, &roster) != nil {
		return ""
	}
	best, bestAt := "", int64(-1)
	for _, w := range roster.Workers {
		d := w.Dispatch
		if d.Launch.Mode != "resume" || d.Launch.SessionID != fromFullID {
			continue
		}
		if d.CreatedAt < notBeforeMs || w.SessionID == "" {
			continue
		}
		if d.CreatedAt > bestAt {
			best, bestAt = w.SessionID, d.CreatedAt
		}
	}
	return best
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
	_, _, err := findSessionProjectDir(toDir, fullID)
	return err
}

// findSessionProjectDir locates a session's own subtree in a profile's projects
// store by globbing for its id-named transcript, and returns
// (<store>/<slug>, <store>/<slug>/<fullID>.jsonl).
//
// The project slug is NEVER computed here. Claude Code slugs the session's
// LAUNCH cwd (every non-alphanumeric byte -> '-'), which is both lossy and not
// recoverable from the transcript: a session started inside a git worktree
// keeps the worktree's slug for life while its records carry whatever cwd was
// current when they were written. Measured on the live store, slugging the
// first cwd in the transcript reproduced the directory name for only 41 of 55
// sessions — the 14 misses were all worktree-launched or case-shifted drive
// letters. Globbing for the id is exact; guessing the slug is not.
func findSessionProjectDir(toDir, fullID string) (string, string, error) {
	matches, err := filepath.Glob(filepath.Join(toDir, "projects", "*", fullID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return "", "", fmt.Errorf("transcript %s.jsonl not visible under %s\\projects — the profiles' projects stores are not shared (junction missing?); refusing to dispatch a blank session", fullID, toDir)
	}
	return filepath.Dir(matches[0]), matches[0], nil
}

// waitForNewSession blocks until the session created by the `--bg --resume`
// re-dispatch is fully on disk, and returns its id and its session directory
// (<store>/<slug>/<newFullID> — which may sit under a DIFFERENT slug than the
// origin, because the re-dispatch cwd need not equal the origin's launch cwd).
//
// Two independent things must appear — the roster entry that names the new id,
// and that id's transcript file — so both are required before returning.
// pollInterval/timeout are parameters so tests can drive this in milliseconds.
func waitForNewSession(toDir, fromFullID string, notBeforeMs int64, pollInterval, timeout time.Duration) (string, string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if newFullID := findResumedWorker(toDir, fromFullID, notBeforeMs); newFullID != "" {
			if slugDir, _, err := findSessionProjectDir(toDir, newFullID); err == nil {
				return newFullID, filepath.Join(slugDir, newFullID), nil
			}
		}
		if !time.Now().Before(deadline) {
			return "", "", fmt.Errorf("no resumed session for %s appeared under %s within %s (roster entry and/or transcript missing)", fromFullID, toDir, timeout)
		}
		time.Sleep(pollInterval)
	}
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
	_, transcript, err := findSessionProjectDir(toDir, fullID)
	if err != nil {
		return "", err
	}
	return sessionCwdFromTranscript(transcript)
}

// sessionCwdFromTranscript is sessionProjectDir's second half, split out so
// HandoffSession — which already holds the transcript path — does not glob the
// projects store twice.
func sessionCwdFromTranscript(transcript string) (string, error) {
	f, err := os.Open(transcript)
	if err != nil {
		return "", fmt.Errorf("open transcript %s: %w", transcript, err)
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
		return "", fmt.Errorf("read transcript %s: %w", transcript, err)
	}
	return "", fmt.Errorf("no cwd found in transcript %s — cannot determine the session's project directory; refusing to re-dispatch where --resume would fork a blank session", transcript)
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
	inFlight   bool // no terminal notification seen after the launch
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

// detectInFlightWorkflows scans a session transcript for every Workflow-tool
// launch with no later terminal notification, oldest first. A Workflow runs as
// a detached process that dies when the session is stopped, and --resume alone
// restores only the transcript -- so the handoff prompt must carry the resume
// coordinates or the run is silently dropped.
//
// Several workflows can be in flight at once (the tool runs detached, so a
// session can start a second one while the first is going), hence a slice: an
// earlier "return the newest one" form silently dropped the others.
//
// Best-effort by design: any read/parse problem returns nothing and the handoff
// proceeds unaugmented. On a partial read the launches seen so far still count:
// resuming an already-finished workflow is a cheap cache no-op, while dropping
// a live one is the data loss this exists to prevent.
func detectInFlightWorkflows(transcriptPath string) []workflowLaunch {
	var out []workflowLaunch
	for _, l := range detectAllWorkflowLaunches(transcriptPath) {
		if l.inFlight {
			out = append(out, l)
		}
	}
	return out
}

// detectAllWorkflowLaunches returns every launch in the transcript, in the
// order they appear, each flagged with whether a terminal notification for it
// was seen afterwards.
func detectAllWorkflowLaunches(transcriptPath string) []workflowLaunch {
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
	for i := range launches {
		// A terminal signal seen at or before the launch line cannot belong to
		// this launch, so it does not count as finished.
		t, ok := terminal[launches[i].taskID]
		launches[i].inFlight = !ok || t <= launches[i].line
	}
	return launches
}

// augmentPromptForWorkflows appends the resume instruction for every in-flight
// Workflow. It states that cpm has ALREADY transplanted each run's journal +
// agent transcripts into this session's transcript directory, because it has
// (see transplantWorkflowRuns) — the agent no longer has to detect the cache
// miss and repair it by hand, which is what the previous prompt asked for and
// what a human ended up doing twice in one hour.
//
// The cache-miss fallback survives as the last sentence: the transplant runs
// AFTER the re-dispatch (the new session id does not exist before it), so a
// fast first turn can still reach the Workflow tool before the copy lands.
func augmentPromptForWorkflows(prompt string, launches []workflowLaunch) string {
	if len(launches) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(prompt)
	if len(launches) == 1 {
		b.WriteString(" A Workflow was in progress and cpm has already copied its run directory")
	} else {
		fmt.Fprintf(&b, " %d Workflows were in progress and cpm has already copied their run directories", len(launches))
	}
	b.WriteString(" (journal.jsonl + agent transcripts) into this session's transcript directory, so resuming hits the cache instead of re-running finished agents. Resume ")
	if len(launches) == 1 {
		b.WriteString("it")
	} else {
		b.WriteString("them")
	}
	b.WriteString(" FIRST, before anything else:")
	for _, wf := range launches {
		fmt.Fprintf(&b, "\n- runId %s: Workflow({scriptPath: %q, resumeFromRunId: %q})", wf.runID, wf.scriptPath, wf.runID)
	}
	b.WriteString("\nThen verify each resume hit the run's cache: its journal.jsonl must list the previously " +
		"completed agents as cached results. If agents start re-running from scratch, the transplant had " +
		"not landed yet - stop the run, wait a few seconds, confirm the run directory now holds the prior " +
		"journal.jsonl and agent transcripts, and resume again.")
	return b.String()
}

// hasWorkflowRuns reports whether a session directory holds any Workflow run at
// all. Used to skip the post-dispatch wait entirely for the common case — a
// handoff of a session that never ran a Workflow pays nothing.
func hasWorkflowRuns(sessionDir string) bool {
	entries, err := os.ReadDir(filepath.Join(sessionDir, "subagents", "workflows"))
	return err == nil && len(entries) > 0
}

// copiedWorkflowRun records one successfully transplanted run.
type copiedWorkflowRun struct{ runID string }

// copyDirRecursive copies a directory tree, creating dst as needed.
func copyDirRecursive(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // skip anything exotic rather than fail the whole run
		}
		return copyFile(path, target)
	})
}

// transplantWorkflowRuns copies every subagents/workflows/<runId>/ directory
// from the origin session's transcript tree into the re-dispatched session's.
//
// This is the fix for the handoff incident. Workflow resume matches an agent()
// call against a {"type":"result"} line in <session>/subagents/workflows/
// <runId>/journal.jsonl, and it looks ONLY inside the CURRENT session's
// transcript directory. A handoff mints a new session id (the roster records
// the resume dispatch with fork:true), so the journal stays behind under the
// origin id and every completed agent silently re-runs.
//
// Policy:
//   - no subagents/workflows at all -> (nil, nil), no output. Ordinary handoffs
//     are unaffected.
//   - a run dir that already exists at the destination is NEVER overwritten:
//     the resumed session may already have started a fresh run under that id,
//     and clobbering it would destroy live state. It is skipped with a loud
//     warning naming the path.
//   - a copy failure warns and skips that run only; the others still land.
func transplantWorkflowRuns(oldSessionDir, newSessionDir string) ([]copiedWorkflowRun, error) {
	srcRoot := filepath.Join(oldSessionDir, "subagents", "workflows")
	entries, err := os.ReadDir(srcRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", srcRoot, err)
	}

	var copied []copiedWorkflowRun
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runID := e.Name()
		src := filepath.Join(srcRoot, runID)
		dst := filepath.Join(newSessionDir, "subagents", "workflows", runID)
		if _, err := os.Stat(dst); err == nil {
			fmt.Fprintf(os.Stderr, "warning: workflow run %s already exists at %s — leaving it untouched; the resumed session may have started a fresh run under that id. Compare the two journals by hand before resuming.\n", runID, dst)
			continue
		}
		if err := copyDirRecursive(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not transplant workflow run %s (%v) — it will re-run from scratch on resume\n", runID, err)
			continue
		}
		copied = append(copied, copiedWorkflowRun{runID: runID})
	}
	return copied, nil
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

	// Resolve the session's own subtree up front and fail fast: this proves the
	// transcript is visible (shared projects store), yields the cwd the
	// --resume re-dispatch must run in, and locates the origin
	// subagents/workflows/ the transplant below moves. Doing it before the stop
	// means a mis-wired store aborts the handoff without needlessly killing the
	// source worker.
	oldSlugDir, oldTranscript, err := findSessionProjectDir(toDir, fullID)
	if err != nil {
		return err
	}
	oldSessionDir := filepath.Join(oldSlugDir, fullID)
	projectDir, err := sessionCwdFromTranscript(oldTranscript)
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

	// Every Workflow the origin session had in flight died with the stop above;
	// --resume restores the transcript only, so the prompt must instruct the
	// resumed session to pick the runs back up. Best-effort: a failed scan
	// changes nothing and the handoff proceeds with the prompt as given.
	if launches := detectInFlightWorkflows(oldTranscript); len(launches) > 0 {
		ids := make([]string, len(launches))
		for i, l := range launches {
			ids[i] = l.runID
		}
		fmt.Printf("in-flight workflow(s) %s detected - augmenting the re-dispatch prompt to resume them\n", strings.Join(ids, ", "))
		prompt = augmentPromptForWorkflows(prompt, launches)
	}

	// Timestamp floor for identifying the worker this dispatch creates. Backed
	// off a little so clock granularity between cpm and the daemon cannot make
	// the correct roster entry look older than the dispatch.
	dispatchNotBeforeMs := time.Now().Add(-2 * time.Second).UnixMilli()

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
	if err := runClaudeInline(bgPath, bgArgv, bgEnv, projectDir); err != nil {
		return err
	}

	// Transplant the Workflow run directories into the session the re-dispatch
	// just created. This must happen AFTER the dispatch: claude mints a NEW
	// session id for a `--bg --resume` (the roster records the launch with
	// fork:true), and the new id — plus the project slug it lands under, which
	// differs whenever the re-dispatch cwd differs from the origin's launch cwd
	// — are only knowable from what the dispatch itself wrote to disk.
	//
	// Everything below is best-effort: the handoff has already succeeded, and
	// the augmented prompt's closing fallback covers a transplant that does not
	// land. Never turn a completed handoff into a command failure here.
	if !hasWorkflowRuns(oldSessionDir) {
		return nil
	}
	newFullID, newSessionDir, err := waitForNewSession(toDir, fullID, dispatchNotBeforeMs, newSessionPollInterval, newSessionPollTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v — workflow runs were NOT transplanted; the resumed session must repair the run directory itself (the prompt tells it how)\n", err)
		return nil
	}
	runs, err := transplantWorkflowRuns(oldSessionDir, newSessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: transplanting workflow runs into %s failed (%v)\n", newSessionDir, err)
		return nil
	}
	if len(runs) > 0 {
		ids := make([]string, len(runs))
		for i, r := range runs {
			ids[i] = r.runID
		}
		fmt.Printf("transplanted %d workflow run(s) into session %s: %s\n", len(runs), newFullID, strings.Join(ids, ", "))
	}
	return nil
}
