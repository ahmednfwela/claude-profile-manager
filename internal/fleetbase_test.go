package internal

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeBaseTOML drops content at <tmp>/profiles/base.toml and returns the
// fleet-repo root LoadFleetBase expects.
func writeBaseTOML(t *testing.T, content string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "profiles", "base.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestLoadFleetBaseRealFleetShape is the A<->B contract test: the fixture is
// a byte-for-byte copy of shared/claude-plugins fleet/profiles/base.toml at
// 3d2be134 (work item B, merged as 4f698a44) -- the shape onboard renders a
// new machine from and fleet-doctor F10 grades a live config.toml against.
// cpm v0.6.0 decoded this exact file to an EMPTY [base] and rendered nothing
// into config.toml, silently; so this test pins every field the file
// carries, not merely "it parsed".
func TestLoadFleetBaseRealFleetShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "fleet-base.toml"))
	if err != nil {
		t.Fatal(err)
	}
	repo := writeBaseTOML(t, string(raw))

	fb, err := LoadFleetBase(repo)
	if err != nil {
		t.Fatalf("LoadFleetBase: %v", err)
	}
	wantArgs := []string{
		"--dangerously-skip-permissions", "--effort", "ultracode",
		"--channels=plugin:bdaya-defaults@bdaya",
		"--append-system-prompt-file", "~/.claude-profiles/lane-authority.md",
	}
	if !reflect.DeepEqual(fb.Base.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", fb.Base.Args, wantArgs)
	}
	if fb.Base.Model != "" {
		t.Errorf("Model = %q, want empty (base.toml pins no model -- shared/claude-plugins#598)", fb.Base.Model)
	}
	wantCommon := map[string]string{
		"ENABLE_TOOL_SEARCH":            "true",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS": "128000",
		"CLAUDE_CODE_PLUGIN_CACHE_DIR":  "~/.claude/plugins",
	}
	if got := fb.Base.BaseEnvCommon(); !reflect.DeepEqual(got, wantCommon) {
		t.Errorf("BaseEnvCommon = %v, want %v", got, wantCommon)
	}
	if got := fb.Base.BaseEnvOverlay("windows"); len(got) != 1 || got["CLAUDE_CODE_USE_POWERSHELL_TOOL"] != "1" {
		t.Errorf("BaseEnvOverlay(windows) = %v, want exactly {CLAUDE_CODE_USE_POWERSHELL_TOOL:1}", got)
	}
	if got := fb.Base.BaseEnvOverlay("darwin"); len(got) != 0 {
		t.Errorf("BaseEnvOverlay(darwin) = %v, want empty (declared as an empty table)", got)
	}
	if got := fb.Base.BaseEnvOverlay("linux"); len(got) != 0 {
		t.Errorf("BaseEnvOverlay(linux) = %v, want empty (undeclared)", got)
	}

	// The verbatim block: starts at the [base] header (banner dropped) and
	// keeps the fleet's own key spellings so F10 can compare it 1:1.
	head := fb.Block
	if len(head) > 40 {
		head = head[:40]
	}
	if !strings.HasPrefix(fb.Block, "[base]\n") {
		t.Errorf("Block must start at the [base] header, got prefix %q", head)
	}
	for _, want := range []string{
		"[base.env]",
		"os_overlay = {",
		`description_prefix = "Claude Max"`,
		"~/.claude-profiles/lane-authority.md",
	} {
		if !strings.Contains(fb.Block, want) {
			t.Errorf("Block missing verbatim %q", want)
		}
	}
	if strings.HasSuffix(fb.Block, "\n") || strings.Contains(fb.Block, "\r") {
		t.Error("Block must be trailing-newline-trimmed and LF-only")
	}
}

// The shape cpm v0.6.0 expected (bare top-level model/args, [env.<goos>]
// headers) is NOT the fleet's shape; accepting it silently is exactly how the
// empty-[base] bug hid. It must be a loud error now.
func TestLoadFleetBaseRejectsBareTopLevelShape(t *testing.T) {
	repo := writeBaseTOML(t, "model = \"sonnet\"\nargs = [\"--dangerously-skip-permissions\"]\n\n[env]\nENABLE_TOOL_SEARCH = \"true\"\n\n[env.windows]\nCLAUDE_CODE_USE_POWERSHELL_TOOL = \"1\"\n")
	_, err := LoadFleetBase(repo)
	if err == nil {
		t.Fatal("expected an error for a base.toml without a [base] header")
	}
	if !strings.Contains(err.Error(), "[base]") {
		t.Errorf("error should name the missing [base] header, got: %v", err)
	}
}

func TestLoadFleetBaseRejectsEmptyBase(t *testing.T) {
	repo := writeBaseTOML(t, "# banner\n[base]\n")
	if _, err := LoadFleetBase(repo); err == nil {
		t.Fatal("expected an error for an empty [base] table")
	}
}

// A commented-out "[base]" mention in the banner is not a header.
func TestLoadFleetBaseIgnoresCommentedHeader(t *testing.T) {
	repo := writeBaseTOML(t, "# the [base] table below\n#[base]\n[base]\nmodel = \"sonnet\"\n")
	fb, err := LoadFleetBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	if fb.Block != "[base]\nmodel = \"sonnet\"" {
		t.Errorf("Block = %q", fb.Block)
	}
}

func TestLoadFleetBaseNormalisesCRLF(t *testing.T) {
	repo := writeBaseTOML(t, "[base]\r\nmodel = \"sonnet\"\r\n")
	fb, err := LoadFleetBase(repo)
	if err != nil {
		t.Fatal(err)
	}
	if fb.Block != "[base]\nmodel = \"sonnet\"" {
		t.Errorf("Block = %q", fb.Block)
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
