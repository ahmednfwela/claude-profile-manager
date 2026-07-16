package internal

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubClaudeOnPath puts a fake `claude` (claude.exe on Windows) on PATH so
// BuildRunInvocation's exec.LookPath("claude") resolves during tests.
func stubClaudeOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestBuildRunInvocationInjectsArgs(t *testing.T) {
	stubClaudeOnPath(t)
	p := &Profile{Args: []string{"--dangerously-skip-permissions"}}
	_, argv, _, err := BuildRunInvocation("glm", t.TempDir(), p, []string{"-p", "hi"})
	if err != nil {
		t.Fatalf("BuildRunInvocation: %v", err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--dangerously-skip-permissions") {
		t.Errorf("profile Args should be injected; got %v", argv)
	}
	// The user's own args must still come last.
	if argv[len(argv)-2] != "-p" || argv[len(argv)-1] != "hi" {
		t.Errorf("user args should follow profile args; got %v", argv)
	}
}

func TestBuildRunInvocationBypassSkipsArgs(t *testing.T) {
	stubClaudeOnPath(t)
	p := &Profile{Args: []string{"--dangerously-skip-permissions"}}
	// Every bypassed subcommand must NOT receive the profile decoration —
	// claude treats flags-before-subcommand as a prompt, so decoration turns
	// e.g. `stop <id>` into a billed model turn (observed live 2026-07-15).
	for _, sub := range []string{"mcp", "stop", "attach", "logs", "respawn", "daemon", "rm", "config"} {
		_, argv, _, err := BuildRunInvocation("glm", t.TempDir(), p, []string{sub, "arg1"})
		if err != nil {
			t.Fatalf("BuildRunInvocation(%s): %v", sub, err)
		}
		if strings.Contains(strings.Join(argv, " "), "--dangerously-skip-permissions") {
			t.Errorf("bypass subcommand %q should not get profile Args; got %v", sub, argv)
		}
	}
}
