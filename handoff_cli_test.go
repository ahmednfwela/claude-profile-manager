package main

// End-to-end test of `cpm handoff` against a stub `claude` on PATH.
//
// The stub records every argv it is called with and, for the `--bg --resume`
// re-dispatch, fabricates exactly what a real `claude --bg` leaves behind — a
// new session transcript under a NEW project slug, a jobs/<short>/state.json,
// and a daemon/roster.json worker whose dispatch.launch is
// {mode:"resume", sessionId:<origin id>} (all shapes verified against the live
// estate: C:\Users\ahmed\.claude-profiles\gmail\daemon\roster.json worker
// 817c16cf, whose own sessionId is 817c16cf-… while dispatch.launch.sessionId
// is the origin c22695de-…).
//
// The profile trees are fabricated under t.TempDir(); no live profile is read.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const (
	cliOriginID   = "c22695de-96b4-4bab-9a61-c26d8cd822b2"
	cliOriginSlug = "D--projects-devops-aggregate--claude-worktrees-rearm-pillar-a-164"
	cliNewID      = "5faacda6-5395-4fee-99d2-b915db6f7022"
	// The re-dispatch runs at the repo root, so the new session lands under a
	// DIFFERENT slug than the origin worktree — the cross-slug case from the
	// incident. Slugs are never computed by cpm; both are found by globbing for
	// the id-named transcript.
	cliNewSlug = "D--projects-devops-aggregate"

	cliRunInFlight = "wf_22995661-f47" // launched, never terminated
	cliRunDone     = "wf_bbbb2222-bbb" // launched and terminated
)

// stubSource is the fake `claude`. It never talks to a model: it appends its
// argv to $CPM_STUB_LOG and, on `--bg`, materialises the new session's on-disk
// footprint under $CLAUDE_CONFIG_DIR.
const stubSource = `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func valueAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func has(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func main() {
	args := os.Args[1:]
	cwd, _ := os.Getwd()
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")

	if log := os.Getenv("CPM_STUB_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			rec, _ := json.Marshal(map[string]any{"args": args, "cwd": cwd, "configDir": cfg})
			f.Write(append(rec, '\n'))
			f.Close()
		}
	}

	if !has(args, "--bg") {
		return // stop/attach/... : recorded, nothing fabricated
	}

	newID := os.Getenv("CPM_STUB_NEW_SESSION")
	slug := os.Getenv("CPM_STUB_NEW_SLUG")
	short := newID[:8]
	proj := filepath.Join(cfg, "projects", slug)
	if err := os.MkdirAll(filepath.Join(proj, newID), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "stub:", err)
		os.Exit(1)
	}
	transcript := "{\"type\":\"custom-title\",\"sessionId\":\"" + newID + "\",\"cwd\":null}\n" +
		"{\"type\":\"attachment\",\"cwd\":" + strconv.Quote(cwd) + ",\"sessionKind\":\"bg\"}\n"
	os.WriteFile(filepath.Join(proj, newID+".jsonl"), []byte(transcript), 0o644)

	// Optional: the resumed session already started a FRESH run under the same
	// runId before the transplant lands (the race the collision policy exists for).
	if collide := os.Getenv("CPM_STUB_COLLIDE"); collide != "" {
		d := filepath.Join(proj, newID, "subagents", "workflows", collide)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "journal.jsonl"), []byte("FRESH-RUN\n"), 0o644)
	}

	jobDir := filepath.Join(cfg, "jobs", short)
	os.MkdirAll(jobDir, 0o755)
	state, _ := json.Marshal(map[string]any{
		"state":        "working",
		"sessionId":    newID,
		"daemonShort":  short,
		"cwd":          cwd,
		"name":         valueAfter(args, "--name"),
		"linkScanPath": filepath.Join(proj, newID+".jsonl"),
		"intent":       valueAfter(args, "--bg"),
	})
	os.WriteFile(filepath.Join(jobDir, "state.json"), state, 0o644)

	rosterPath := filepath.Join(cfg, "daemon", "roster.json")
	os.MkdirAll(filepath.Dir(rosterPath), 0o755)
	roster := map[string]any{"proto": 1, "workers": map[string]any{}}
	if data, err := os.ReadFile(rosterPath); err == nil {
		json.Unmarshal(data, &roster)
	}
	workers, _ := roster["workers"].(map[string]any)
	if workers == nil {
		workers = map[string]any{}
	}
	workers[short] = map[string]any{
		"pid":       os.Getpid(),
		"sessionId": newID,
		"cwd":       cwd,
		"dispatch": map[string]any{
			"short":     short,
			"sessionId": newID,
			"createdAt": time.Now().UnixMilli(),
			"source":    "shell",
			"cwd":       cwd,
			"launch": map[string]any{
				"mode":      "resume",
				"sessionId": valueAfter(args, "--resume"),
				"fork":      true,
			},
		},
	}
	roster["workers"] = workers
	out, _ := json.MarshalIndent(roster, "", " ")
	os.WriteFile(rosterPath, out, 0o644)

	fmt.Println("Started background session " + short)
}
`

// Both binaries are built once per package run, not per test — four tests each
// building cpm and the stub was by far the slowest thing in the suite.
var (
	buildOnce   sync.Once
	buildDir    string // holds cpm and a bin/ dir containing the stub claude
	buildErr    error
	stubPathDir string
	cpmPath     string
)

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// buildBinaries compiles the real cpm binary under test and the stub `claude`.
func buildBinaries(t *testing.T) (cpmBin, stubDir string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; cannot build cpm and the claude stub")
	}
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "cpm-cli-test")
		if buildErr != nil {
			return
		}
		cpmPath = filepath.Join(buildDir, exeName("cpm"))
		if out, err := exec.Command("go", "build", "-o", cpmPath, ".").CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build cpm: %v\n%s", err, out)
			return
		}

		src := filepath.Join(buildDir, "stubsrc")
		stubPathDir = filepath.Join(buildDir, "bin")
		for _, d := range []string{src, stubPathDir} {
			if buildErr = os.MkdirAll(d, 0o755); buildErr != nil {
				return
			}
		}
		if buildErr = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module cpmstubclaude\n\ngo 1.21\n"), 0o644); buildErr != nil {
			return
		}
		if buildErr = os.WriteFile(filepath.Join(src, "main.go"), []byte(stubSource), 0o644); buildErr != nil {
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(stubPathDir, exeName("claude")), ".")
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build stub claude: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return cpmPath, stubPathDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// originLaunch renders a Workflow launch tool_result exactly as it appears in a
// real transcript (verified against a live session jsonl).
func originLaunch(taskID, runID, sessionDir string) string {
	script := filepath.Join(sessionDir, "workflows", "scripts", "lane-"+runID+".js")
	text := "Workflow launched in background. Task ID: " + taskID + "\\n" +
		"Summary: fixture lane\\n" +
		"Transcript dir: " + jsonEscape(filepath.Join(sessionDir, "subagents", "workflows", runID)) + "\\n" +
		"Script file: " + jsonEscape(script) + "\\n" +
		"Run ID: " + runID + "\\n" +
		"To resume after editing the script: Workflow({scriptPath: \\\"...\\\"})"
	return `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_` + taskID +
		`","type":"tool_result","content":"` + text + `"}]}}`
}

func jsonEscape(s string) string {
	return strings.ReplaceAll(s, `\`, `\\`)
}

func originTerminal(taskID string) string {
	return `{"type":"user","message":{"role":"user","content":"<task-notification>\n<task-id>` + taskID +
		`</task-id>\n<status>completed</status>\n</task-notification>"}}`
}

// handoffFixture fabricates the two-profile tree and returns
// (configPath, fromDir, toDir, workDir).
//
// workDir is a real directory: cpm chdirs the claude child into the session's
// own project cwd, so a hard-coded path would make the whole test
// platform-specific (`chdir D:\projects\...: no such file or directory` on
// Linux/macOS). The project SLUGS stay hard-coded — cpm treats them as opaque
// strings and never parses them, and keeping them fixed preserves the
// worktree-vs-repo-root cross-slug case this test exists for.
//
// In production every profile junctions projects/ to one shared store, so the
// origin transcript is visible from BOTH profiles. Junction creation needs
// elevation on Windows, so the fixture writes the same tree into both profile
// dirs instead; cpm only ever reads the TARGET profile's projects/, which is
// where the transplant must land.
func handoffFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	base := t.TempDir()
	fromDir := filepath.Join(base, "from")
	toDir := filepath.Join(base, "to")
	workDir := filepath.Join(base, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(base, "config.toml"), fmt.Sprintf(
		"source_dir = %q\nbin_dir = %q\n\n[profiles.from]\ndescription = \"origin\"\n\n[profiles.to]\ndescription = \"target\"\n",
		filepath.Join(base, "source"), filepath.Join(base, "bin")))

	originSessionDir := filepath.Join(toDir, "projects", cliOriginSlug, cliOriginID)
	transcript := strings.Join([]string{
		`{"type":"custom-title","sessionId":"` + cliOriginID + `","cwd":null}`,
		`{"type":"attachment","cwd":"` + jsonEscape(workDir) + `","sessionKind":"bg"}`,
		originLaunch("wtaskaaa1", cliRunInFlight, originSessionDir),
		originLaunch("wtaskbbb2", cliRunDone, originSessionDir),
		originTerminal("wtaskbbb2"),
	}, "\n") + "\n"

	for _, dir := range []string{fromDir, toDir} {
		slug := filepath.Join(dir, "projects", cliOriginSlug)
		write(t, filepath.Join(slug, cliOriginID+".jsonl"), transcript)
		for _, run := range []string{cliRunInFlight, cliRunDone} {
			runDir := filepath.Join(slug, cliOriginID, "subagents", "workflows", run)
			write(t, filepath.Join(runDir, "journal.jsonl"),
				`{"type":"started","key":"v2:aaa","agentId":"a1"}`+"\n"+
					`{"type":"result","key":"v2:aaa","agentId":"a1","result":{"lane":"`+run+`","status":"PUSHED","mrs":[]}}`+"\n")
			write(t, filepath.Join(runDir, "agent-a1.jsonl"), `{"type":"assistant","runId":"`+run+`"}`+"\n")
			write(t, filepath.Join(runDir, "agent-a1.meta.json"),
				`{"agentType":"workflow-subagent","spawnDepth":1,"model":"sonnet"}`+"\n")
		}
		// A sibling that must NOT be swept along: the per-session run STATUS file
		// lives beside subagents/, not inside it.
		write(t, filepath.Join(slug, cliOriginID, "workflows", cliRunInFlight+".json"),
			`{"runId":"`+cliRunInFlight+`"}`+"\n")
	}

	write(t, filepath.Join(fromDir, "jobs", cliOriginID[:8], "state.json"),
		`{"state":"working","name":"invora bayader production readiness","sessionId":"`+cliOriginID+`"}`)
	write(t, filepath.Join(fromDir, "daemon", "roster.json"),
		`{"proto":1,"workers":{"`+cliOriginID[:8]+`":{"sessionId":"`+cliOriginID+`"}}}`)

	return filepath.Join(base, "config.toml"), fromDir, toDir, workDir
}

type stubCall struct {
	Args      []string `json:"args"`
	Cwd       string   `json:"cwd"`
	ConfigDir string   `json:"configDir"`
}

func readStubLog(t *testing.T, path string) []stubCall {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub log unreadable — the stub was never invoked: %v", err)
	}
	var calls []stubCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var c stubCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("stub log line %q: %v", line, err)
		}
		calls = append(calls, c)
	}
	return calls
}

func runHandoff(t *testing.T, extraEnv ...string) (out string, toDir string, workDir string, log []stubCall) {
	t.Helper()
	cpm, stubDir := buildBinaries(t)
	configPath, _, toDir, workDir := handoffFixture(t)
	logPath := filepath.Join(t.TempDir(), "stub.log")

	cmd := exec.Command(cpm, "--config", configPath, "handoff", cliOriginID, "from", "to")
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CPM_STUB_LOG="+logPath,
		"CPM_STUB_NEW_SESSION="+cliNewID,
		"CPM_STUB_NEW_SLUG="+cliNewSlug,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cpm handoff failed: %v\n%s", err, combined)
	}
	return string(combined), toDir, workDir, readStubLog(t, logPath)
}

func newRunDir(toDir, runID string) string {
	return filepath.Join(toDir, "projects", cliNewSlug, cliNewID, "subagents", "workflows", runID)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestHandoffCLIDispatchesResumeInSessionCwd(t *testing.T) {
	out, _, workDir, calls := runHandoff(t)

	if len(calls) != 2 {
		t.Fatalf("expected a stop then a --bg re-dispatch, got %d calls:\n%s\n%s", len(calls), out, dump(calls))
	}
	if calls[0].Args[0] != "stop" || calls[0].Args[1] != cliOriginID[:8] {
		t.Fatalf("first call should be `stop <short>`, got %v", calls[0].Args)
	}
	bg := calls[1]
	joined := strings.Join(bg.Args, " ")
	for _, want := range []string{"--bg", "--resume " + cliOriginID, "--name invora bayader production readiness"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("re-dispatch argv missing %q: %v", want, bg.Args)
		}
	}
	if bg.Cwd != workDir {
		t.Fatalf("re-dispatch cwd = %q, want the session's own project dir %q", bg.Cwd, workDir)
	}
}

// The heart of the incident: the in-flight Workflow's journal + agent
// transcripts must be sitting in the NEW session's transcript dir when the
// resumed agent looks for them, or the whole run silently re-executes.
func TestHandoffCLITransplantsWorkflowRuns(t *testing.T) {
	out, toDir, _, _ := runHandoff(t)

	originRun := filepath.Join(toDir, "projects", cliOriginSlug, cliOriginID, "subagents", "workflows", cliRunInFlight)

	for _, run := range []string{cliRunInFlight, cliRunDone} {
		dst := newRunDir(toDir, run)
		for _, f := range []string{"journal.jsonl", "agent-a1.jsonl", "agent-a1.meta.json"} {
			if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
				t.Fatalf("workflow run %s was not transplanted (%s missing) — the resumed session will re-run every agent from scratch\ncpm output:\n%s", run, f, out)
			}
		}
	}

	src := mustRead(t, filepath.Join(originRun, "journal.jsonl"))
	got := mustRead(t, filepath.Join(newRunDir(toDir, cliRunInFlight), "journal.jsonl"))
	if got != src {
		t.Fatalf("journal.jsonl not copied byte-for-byte:\n got: %q\nwant: %q", got, src)
	}

	// The origin tree is evidence, not scratch: it must survive untouched.
	if !strings.Contains(src, `"type":"result"`) {
		t.Fatalf("origin journal was mutated by the handoff: %q", src)
	}
	if _, err := os.Stat(filepath.Join(originRun, "agent-a1.meta.json")); err != nil {
		t.Fatalf("origin run dir damaged by the handoff: %v", err)
	}

	// Only subagents/workflows/ is transplanted — the sibling per-session run
	// status file is not part of the resume cache.
	if _, err := os.Stat(filepath.Join(toDir, "projects", cliNewSlug, cliNewID, "workflows")); err == nil {
		t.Fatalf("handoff copied the sibling workflows/ status dir; only subagents/workflows/ belongs in the transplant")
	}
}

// The prompt must name the run that is actually resumable, and must not tell the
// agent to resume one that already finished.
func TestHandoffCLIPromptNamesInFlightRun(t *testing.T) {
	_, _, _, calls := runHandoff(t)
	prompt := calls[len(calls)-1].Args[promptIndex(t, calls[len(calls)-1].Args)]

	for _, want := range []string{cliRunInFlight, "resumeFromRunId"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("re-dispatch prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, cliRunDone) {
		t.Fatalf("prompt tells the agent to resume an already-completed run %s:\n%s", cliRunDone, prompt)
	}
	if !strings.Contains(prompt, "already copied") && !strings.Contains(prompt, "transplant") {
		t.Fatalf("prompt still asks the agent to self-heal instead of stating cpm transplanted the run dirs:\n%s", prompt)
	}
}

// A run dir already present at the destination is never clobbered — the
// resumed session may have started a fresh run under that id, and silently
// overwriting it would destroy live state.
func TestHandoffCLIRefusesToClobberExistingRunDir(t *testing.T) {
	out, toDir, _, _ := runHandoff(t, "CPM_STUB_COLLIDE="+cliRunInFlight)

	if got := mustRead(t, filepath.Join(newRunDir(toDir, cliRunInFlight), "journal.jsonl")); got != "FRESH-RUN\n" {
		t.Fatalf("existing destination run dir was clobbered: %q", got)
	}
	if !strings.Contains(out, cliRunInFlight) || !strings.Contains(strings.ToLower(out), "exist") {
		t.Fatalf("collision was silent — expected a warning naming %s:\n%s", cliRunInFlight, out)
	}
	// The non-colliding run still lands.
	if _, err := os.Stat(filepath.Join(newRunDir(toDir, cliRunDone), "journal.jsonl")); err != nil {
		t.Fatalf("a collision on one run aborted the others: %v", err)
	}
}

func promptIndex(t *testing.T, args []string) int {
	t.Helper()
	for i, a := range args {
		if a == "--bg" && i+1 < len(args) {
			return i + 1
		}
	}
	t.Fatalf("no --bg prompt in argv %v", args)
	return 0
}

func dump(calls []stubCall) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "  %v (cwd=%s)\n", c.Args, c.Cwd)
	}
	return b.String()
}
