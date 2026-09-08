package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input    string
		expected string
	}{
		{"~/foo", filepath.Join(home, "foo")},
		{"~/.claude", filepath.Join(home, ".claude")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"", ""},
	}

	for _, tt := range tests {
		got := ExpandPath(tt.input)
		if got != tt.expected {
			t.Errorf("ExpandPath(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	content := `
source_dir = "~/.claude"
bin_dir = "~/.local/bin"

[profiles.personal]
description = "Personal account"

[profiles.work]
description = "Work account"
model = "sonnet"
add_dirs = ["~/Work/company"]
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.Profiles) != 2 {
		t.Errorf("expected 2 profiles, got %d", len(cfg.Profiles))
	}

	personal := cfg.Profiles["personal"]
	if personal == nil {
		t.Fatal("personal profile not found")
	}
	if personal.Description != "Personal account" {
		t.Errorf("personal description = %q, want %q", personal.Description, "Personal account")
	}

	work := cfg.Profiles["work"]
	if work == nil {
		t.Fatal("work profile not found")
	}
	if work.Model != "sonnet" {
		t.Errorf("work model = %q, want %q", work.Model, "sonnet")
	}
	if len(work.AddDirs) != 1 || work.AddDirs[0] != "~/Work/company" {
		t.Errorf("work add_dirs = %v, want [~/Work/company]", work.AddDirs)
	}
}

func TestLoadConfigWithAttribution(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	content := `
[profiles.work]
description = "Work"

[profiles.work.attribution]
commit = "Co-Authored-By: Claude"
pr = "Generated with Claude Code"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	work := cfg.Profiles["work"]
	if work.Attribution == nil {
		t.Fatal("attribution is nil")
	}
	if work.Attribution.Commit != "Co-Authored-By: Claude" {
		t.Errorf("commit = %q", work.Attribution.Commit)
	}
	if work.Attribution.PR != "Generated with Claude Code" {
		t.Errorf("pr = %q", work.Attribution.PR)
	}
}

func TestLoadConfigWithEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	content := `
[profiles.vertex]
description = "Vertex AI"

[profiles.vertex.env]
CLAUDE_CODE_USE_VERTEX = "1"
CLOUD_ML_REGION = "europe-west1"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	vertex := cfg.Profiles["vertex"]
	if vertex.Env["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Errorf("env CLAUDE_CODE_USE_VERTEX = %q", vertex.Env["CLAUDE_CODE_USE_VERTEX"])
	}
	if vertex.Env["CLOUD_ML_REGION"] != "europe-west1" {
		t.Errorf("env CLOUD_ML_REGION = %q", vertex.Env["CLOUD_ML_REGION"])
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.toml")
	if err == nil {
		t.Error("expected error for missing config")
	}
}

func TestLoadConfigNoProfiles(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	if err := os.WriteFile(configPath, []byte(`source_dir = "~/.claude"`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Error("expected error for config with no profiles")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	content := `
[profiles.test]
description = "Test"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	home, _ := os.UserHomeDir()
	expectedSource := filepath.Join(home, ".claude")
	expectedBin := filepath.Join(home, ".local", "bin")

	if cfg.SourceDir != expectedSource {
		t.Errorf("source_dir = %q, want %q", cfg.SourceDir, expectedSource)
	}
	if cfg.BinDir != expectedBin {
		t.Errorf("bin_dir = %q, want %q", cfg.BinDir, expectedBin)
	}
}

func TestProfilesBaseDir(t *testing.T) {
	// ProfilesBaseDir returns native-separator paths; build the expectation the
	// same way so the assertion holds on Windows (\) and Unix (/) alike.
	in := filepath.FromSlash("/home/user/.claude-profiles/config.toml")
	want := filepath.FromSlash("/home/user/.claude-profiles")
	got := ProfilesBaseDir(in)
	if got != want {
		t.Errorf("ProfilesBaseDir = %q, want %q", got, want)
	}
}

// --- cpm-unification schema additions (design spec §3) ---------------------

func TestLoadConfigWithBaseAndAuth(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")

	content := `
[fleet]
repo_path = "/opt/fleet-repo"

[base]
description_prefix = "Claude Max"
model = "sonnet"
args = ["--dangerously-skip-permissions"]
[base.env]
ENABLE_TOOL_SEARCH = "true"
[base.env.windows]
CLAUDE_CODE_USE_POWERSHELL_TOOL = "1"
[base.env.darwin]

[profiles.bdaya]
description = "Claude Max — bdaya"

[profiles.bdaya.auth]
mode = "oauth"
[profiles.bdaya.auth.env]
CLAUDE_CODE_OAUTH_TOKEN = "${BDAYA_CLAUDE_OAUTH_TOKEN_BDAYA}"

[profiles.alibaba1]
description = "Alibaba Qwen — alibaba1"

[profiles.alibaba1.auth]
mode = "api_key"
[profiles.alibaba1.auth.env]
ANTHROPIC_BASE_URL = "${BDAYA_ALIBABA_BASE_URL}"
ANTHROPIC_API_KEY  = "${BDAYA_ALIBABA_API_KEY}"

[profiles.alibaba1.env]
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Fleet == nil || cfg.Fleet.RepoPath != "/opt/fleet-repo" {
		t.Fatalf("fleet.repo_path = %+v, want /opt/fleet-repo", cfg.Fleet)
	}

	if cfg.Base == nil {
		t.Fatal("cfg.Base is nil")
	}
	if cfg.Base.Model != "sonnet" {
		t.Errorf("base.model = %q, want sonnet", cfg.Base.Model)
	}
	if len(cfg.Base.Args) != 1 || cfg.Base.Args[0] != "--dangerously-skip-permissions" {
		t.Errorf("base.args = %v", cfg.Base.Args)
	}
	common := cfg.Base.BaseEnvCommon()
	if common["ENABLE_TOOL_SEARCH"] != "true" {
		t.Errorf("base.env common = %v", common)
	}
	if _, isOverlay := common["windows"]; isOverlay {
		t.Error("BaseEnvCommon must not leak the windows overlay subtable as a flat key")
	}
	winOverlay := cfg.Base.BaseEnvOverlay("windows")
	if winOverlay["CLAUDE_CODE_USE_POWERSHELL_TOOL"] != "1" {
		t.Errorf("base.env.windows overlay = %v", winOverlay)
	}
	if got := cfg.Base.BaseEnvOverlay("linux"); len(got) != 0 {
		t.Errorf("base.env.linux (undeclared) should be empty, got %v", got)
	}

	bdaya := cfg.Profiles["bdaya"]
	if bdaya.Auth == nil || bdaya.Auth.Mode != "oauth" {
		t.Fatalf("bdaya.auth = %+v", bdaya.Auth)
	}
	if bdaya.Auth.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "${BDAYA_CLAUDE_OAUTH_TOKEN_BDAYA}" {
		t.Errorf("bdaya.auth.env = %v", bdaya.Auth.Env)
	}

	alibaba := cfg.Profiles["alibaba1"]
	if alibaba.Auth == nil || alibaba.Auth.Mode != "api_key" {
		t.Fatalf("alibaba1.auth = %+v", alibaba.Auth)
	}
	if alibaba.Auth.Env["ANTHROPIC_BASE_URL"] != "${BDAYA_ALIBABA_BASE_URL}" {
		t.Errorf("alibaba1.auth.env = %v", alibaba.Auth.Env)
	}
	if alibaba.Env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Errorf("alibaba1.env delta = %v", alibaba.Env)
	}
}

// resolveManageMCPMode is the compat shim named by the design spec's work
// item (A): manage_mcp must keep accepting the legacy TOML boolean every
// existing config.toml already uses, alongside the new mode strings.
func TestResolveManageMCPMode(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want ManageMCPMode
	}{
		{"absent (nil) preserves today's default", nil, ManageMCPLegacyMirror},
		{"legacy true preserves today's mirror behavior", true, ManageMCPLegacyMirror},
		{"legacy false maps to gateway-owns", false, ManageMCPGatewayOwns},
		{"new fleet-render string", "fleet-render", ManageMCPFleetRender},
		{"new gateway-owns string", "gateway-owns", ManageMCPGatewayOwns},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, _, err := resolveManageMCPMode(tt.raw)
			if err != nil {
				t.Fatalf("resolveManageMCPMode(%#v): %v", tt.raw, err)
			}
			if mode != tt.want {
				t.Errorf("resolveManageMCPMode(%#v) = %q, want %q", tt.raw, mode, tt.want)
			}
		})
	}
}

func TestResolveManageMCPModeRejectsUnknownString(t *testing.T) {
	if _, _, err := resolveManageMCPMode("full-mirror-typo"); err == nil {
		t.Error("expected an error for an unrecognized manage_mcp string")
	}
}

func TestConfigManageMCPEnabledCompat(t *testing.T) {
	if !(&Config{}).ManageMCPEnabled() {
		t.Error("nil manage_mcp should default to enabled")
	}
	if !(&Config{ManageMCP: true}).ManageMCPEnabled() {
		t.Error("manage_mcp=true should be enabled")
	}
	if (&Config{ManageMCP: false}).ManageMCPEnabled() {
		t.Error("manage_mcp=false should be disabled")
	}
	if !(&Config{ManageMCP: "fleet-render"}).ManageMCPEnabled() {
		t.Error("manage_mcp=\"fleet-render\" should be enabled")
	}
}

func TestEffectiveManageMCPModePerProfileOverride(t *testing.T) {
	cfg := &Config{
		ManageMCP: "gateway-owns",
		Profiles: map[string]*Profile{
			"inherits": {},
			"override": {ManageMCP: "fleet-render"},
		},
	}
	mode, err := cfg.EffectiveManageMCPMode("inherits")
	if err != nil || mode != ManageMCPGatewayOwns {
		t.Errorf("inherits: got %q, %v; want gateway-owns", mode, err)
	}
	mode, err = cfg.EffectiveManageMCPMode("override")
	if err != nil || mode != ManageMCPFleetRender {
		t.Errorf("override: got %q, %v; want fleet-render", mode, err)
	}
	if _, err := cfg.EffectiveManageMCPMode("nope"); err == nil {
		t.Error("expected error for unknown profile")
	}
}
