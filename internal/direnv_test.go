package internal

import (
	"strings"
	"testing"
)

func TestGenerateDirenvSnippet(t *testing.T) {
	out := GenerateDirenvSnippet("work", "/home/user/.claude-profiles/work", ShellBash)

	mustContain := []string{
		`CLAUDE_CONFIG_DIR="/home/user/.claude-profiles/work"`,
		`CLAUDE_PROFILE="work"`,
	}

	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("direnv snippet missing %q", s)
		}
	}
}

func TestGenerateDirenvSnippetPowerShellUnsupported(t *testing.T) {
	out := GenerateDirenvSnippet("work", `C:\Users\u\.claude-profiles\work`, ShellPowerShell)

	// direnv has no PowerShell story — we emit a note pointing at `cpm use`,
	// never a bash snippet that would not run.
	if !strings.Contains(out, "not supported") {
		t.Errorf("expected unsupported note, got:\n%s", out)
	}
	if strings.Contains(out, "export ") {
		t.Errorf("powershell direnv output should not contain bash export:\n%s", out)
	}
	if !strings.Contains(out, "cpm use work") {
		t.Errorf("expected guidance to use 'cpm use', got:\n%s", out)
	}
}
