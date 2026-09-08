package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFleetBase(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `
model = "sonnet"
args = ["--dangerously-skip-permissions"]

[env]
ENABLE_TOOL_SEARCH = "true"

[env.windows]
CLAUDE_CODE_USE_POWERSHELL_TOOL = "1"
`
	if err := os.WriteFile(filepath.Join(repo, "profiles", "base.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	base, err := LoadFleetBase(repo)
	if err != nil {
		t.Fatalf("LoadFleetBase: %v", err)
	}
	if base.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet", base.Model)
	}
	if len(base.Args) != 1 || base.Args[0] != "--dangerously-skip-permissions" {
		t.Errorf("Args = %v", base.Args)
	}
	if base.BaseEnvCommon()["ENABLE_TOOL_SEARCH"] != "true" {
		t.Errorf("BaseEnvCommon = %v", base.BaseEnvCommon())
	}
	if base.BaseEnvOverlay("windows")["CLAUDE_CODE_USE_POWERSHELL_TOOL"] != "1" {
		t.Errorf("BaseEnvOverlay(windows) = %v", base.BaseEnvOverlay("windows"))
	}
}

func TestLoadFleetBaseMissingFile(t *testing.T) {
	if _, err := LoadFleetBase(t.TempDir()); err == nil {
		t.Error("expected an error when fleet/profiles/base.toml does not exist")
	}
}

func TestFleetDeviceID(t *testing.T) {
	cfg := &Config{Fleet: &FleetConfig{ID: "windows-desktop"}}
	if got := FleetDeviceID(cfg); got != "windows-desktop" {
		t.Errorf("FleetDeviceID = %q, want windows-desktop", got)
	}

	cfgNoFleet := &Config{}
	if got := FleetDeviceID(cfgNoFleet); got == "" {
		t.Error("FleetDeviceID must fall back to something non-empty (os.Hostname) when fleet.id is unset")
	}
}

func TestLoadMCPServersPatchMissingFileIsNilNotError(t *testing.T) {
	patch, err := LoadMCPServersPatch(t.TempDir())
	if err != nil {
		t.Fatalf("expected no error for a missing patch file, got %v", err)
	}
	if patch != nil {
		t.Errorf("expected a nil patch, got %v", patch)
	}
}

func TestLoadMCPServersPatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcpServers.patch.json"),
		[]byte(`{"socraticode":{"type":"http","url":"http://x/mcp"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	patch, err := LoadMCPServersPatch(dir)
	if err != nil {
		t.Fatalf("LoadMCPServersPatch: %v", err)
	}
	if patch["socraticode"] == nil {
		t.Errorf("patch = %v, missing socraticode key", patch)
	}
}

func TestFleetRenderedDirShape(t *testing.T) {
	got := FleetRenderedDir("/opt/fleet-repo", "windows-desktop", "alibaba1")
	want := filepath.Join("/opt/fleet-repo", "rendered", "windows-desktop", "alibaba1")
	if got != want {
		t.Errorf("FleetRenderedDir = %q, want %q", got, want)
	}
}
