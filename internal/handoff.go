package internal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// HandoffSession moves a background session between profiles/accounts: it
// stops the worker on the source profile, then re-dispatches the conversation
// as a new background session on the target profile via `claude --bg --resume`.
//
// This relies on the shared projects store (all profiles junction projects/ to
// one directory), so the transcript is already visible to the target profile.
// The stop MUST happen first: two profiles resuming one session means two
// processes appending the same transcript file.
func HandoffSession(fromName, fromDir string, fromProfile *Profile, toName, toDir string, toProfile *Profile, sessionID, prompt, name string) error {
	fullID, shortID, err := ResolveSessionID(sessionID, fromDir)
	if err != nil {
		return err
	}

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
