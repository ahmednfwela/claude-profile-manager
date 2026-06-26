//go:build windows

package internal

import (
	"strings"
	"testing"
)

// On Windows the launcher is a thin .cmd resolvable via PATHEXT.
func TestLauncherFileNameWindows(t *testing.T) {
	if got := LauncherFileName("glm"); got != "claude-glm.cmd" {
		t.Errorf("LauncherFileName = %q, want claude-glm.cmd", got)
	}
}

// The Windows launcher must only delegate to `cpm run <name>` — it must never
// embed env vars or secrets.
func TestGenerateLauncherWindowsDelegates(t *testing.T) {
	cpmPath := `C:\Users\ahmed\.local\bin\cpm.exe`
	profile := &Profile{
		Description: "GLM",
		Env:         map[string]string{"ANTHROPIC_AUTH_TOKEN": "super-secret-token"},
	}

	out := GenerateLauncher("glm", `C:\Users\ahmed\.claude-profiles\glm`, profile, cpmPath)

	mustContain := []string{
		"@echo off",
		marker,    // greppable so CleanupStaleScripts reaps it
		cpmPath,   // absolute cpm path
		"run glm", // delegation to cpm run <name>
		"%*",      // forwards all args
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("launcher missing %q in:\n%s", s, out)
		}
	}

	// The token / env must never be written into the on-PATH launcher.
	if strings.Contains(out, "super-secret-token") {
		t.Errorf("launcher leaked the token:\n%s", out)
	}
	if strings.Contains(out, "ANTHROPIC_AUTH_TOKEN") || strings.Contains(out, "export ") {
		t.Errorf("launcher must not set env vars:\n%s", out)
	}
}
