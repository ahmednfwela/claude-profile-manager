package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFleetRepoFixture builds a minimal fleet-repo checkout at dir/fleet-repo
// containing fleet/profiles/base.toml (the "single generator for [base]")
// and, when patch != "", a staged mcpServers.patch.json for device/profile.
// Returns the repo's root path.
func writeFleetRepoFixture(t *testing.T, dir, device, profile, baseTOML, mcpPatchJSON string) string {
	t.Helper()
	repo := filepath.Join(dir, "fleet-repo")
	if err := os.MkdirAll(filepath.Join(repo, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "profiles", "base.toml"), []byte(baseTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if mcpPatchJSON != "" {
		renderedDir := FleetRenderedDir(repo, device, profile)
		if err := os.MkdirAll(renderedDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(renderedDir, "mcpServers.patch.json"), []byte(mcpPatchJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// TestRunSyncDryRunGoldenDiff is the golden-file test named by the design
// spec's work item (A): a fixture config.toml (with a stale [base] block)
// plus a fixture fleet-repo dir must produce an EXACT, predictable diff from
// `cpm sync --all --dry-run` -- and must not write anything.
func TestRunSyncDryRunGoldenDiff(t *testing.T) {
	dir := t.TempDir()
	repo := writeFleetRepoFixture(t, dir, "test-device", "alibaba1",
		`model = "sonnet"
args = ["--dangerously-skip-permissions"]

[env]
ENABLE_TOOL_SEARCH = "true"
`, "")

	configPath := filepath.Join(dir, "config.toml")
	original := "source_dir = \"~/.claude\"\n\n" +
		"[fleet]\n" +
		"id = \"test-device\"\n" +
		"repo_path = " + tomlValue(repo) + "\n\n" +
		baseSentinelBeginLine + "\n" +
		"[base]\n" +
		"model = \"OLD-MODEL\"\n" +
		baseSentinelEndLine + "\n\n" +
		"[profiles.bdaya]\n" +
		"description = \"bdaya\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	report, err := RunSync(cfg, configPath, SyncOptions{All: true, Apply: false, GOOS: "linux"})
	if err != nil {
		t.Fatalf("RunSync (dry-run): %v", err)
	}

	if report.BaseDiff == nil {
		t.Fatal("expected a [base] diff (fixture base.toml differs from the live OLD-MODEL block)")
	}
	wantAfter := strings.TrimRight(RenderBaseBlock(&Base{
		Model: "sonnet",
		Args:  []string{"--dangerously-skip-permissions"},
		Env:   map[string]any{"ENABLE_TOOL_SEARCH": "true"},
	}), "\n")
	if report.BaseDiff.After != wantAfter {
		t.Errorf("BaseDiff.After = %q, want %q", report.BaseDiff.After, wantAfter)
	}
	if !strings.Contains(report.BaseDiff.Before, "OLD-MODEL") {
		t.Errorf("BaseDiff.Before = %q, want it to contain OLD-MODEL", report.BaseDiff.Before)
	}
	if !report.HasDrift() {
		t.Error("HasDrift() should be true when a [base] diff is present")
	}

	// Dry-run must never write.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != original {
		t.Error("cpm sync --dry-run must not modify config.toml")
	}
}

func TestRunSyncNoDriftWhenAlreadyInSync(t *testing.T) {
	dir := t.TempDir()
	base := &Base{Model: "sonnet"}
	repo := writeFleetRepoFixture(t, dir, "test-device", "bdaya", `model = "sonnet"`, "")

	configPath := filepath.Join(dir, "config.toml")
	content := "[fleet]\nrepo_path = " + tomlValue(repo) + "\n\n" +
		RenderBaseBlock(base) + "\n" +
		"[profiles.bdaya]\ndescription = \"bdaya\"\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunSync(cfg, configPath, SyncOptions{All: true, GOOS: "linux"})
	if err != nil {
		t.Fatalf("RunSync: %v", err)
	}
	if report.HasDrift() {
		t.Errorf("expected no drift, got BaseDiff=%v Profiles=%v", report.BaseDiff, report.Profiles)
	}
}

func TestRunSyncApplyWritesSpliceAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	repo := writeFleetRepoFixture(t, dir, "test-device", "bdaya", `model = "sonnet"`, "")

	configPath := filepath.Join(dir, "config.toml")
	original := "[fleet]\nrepo_path = " + tomlValue(repo) + "\n\n" +
		baseSentinelBeginLine + "\n[base]\nmodel = \"OLD\"\n" + baseSentinelEndLine + "\n\n" +
		"[profiles.bdaya]\ndescription = \"bdaya\"\n"
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunSync(cfg, configPath, SyncOptions{All: true, Apply: true, GOOS: "linux"}); err != nil {
		t.Fatalf("RunSync --apply: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sonnet") {
		t.Error("apply did not write the new model into config.toml")
	}
	if strings.Contains(string(raw), "\"OLD\"") {
		t.Error("apply left the stale model in place")
	}
	// The unrelated profile stanza must survive the apply untouched.
	if !strings.Contains(string(raw), "[profiles.bdaya]") {
		t.Error("apply must not disturb [profiles.*] stanzas")
	}

	// Second apply with nothing changed must be a no-op (idempotent).
	cfg2, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	report2, err := RunSync(cfg2, configPath, SyncOptions{All: true, Apply: true, GOOS: "linux"})
	if err != nil {
		t.Fatalf("second RunSync --apply: %v", err)
	}
	if report2.HasDrift() {
		t.Error("a second --apply run with no upstream change should produce an empty diff")
	}
}

// End-to-end (through RunSync, not just MergeMCPServersPatch directly):
// applying a staged mcpServers patch must preserve a live ChannelInstall
// entry it did not itself write.
func TestRunSyncApplyMCPServersPreservesChannelInstallEntry(t *testing.T) {
	dir := t.TempDir()
	repo := writeFleetRepoFixture(t, dir, "test-device", "alibaba1",
		`model = "sonnet"`,
		`{"socraticode":{"type":"http","url":"http://127.0.0.1:9999/mcp"}}`)

	configPath := filepath.Join(dir, "config.toml")
	// manage_mcp MUST precede the [fleet] table header: TOML keeps assigning
	// un-bracketed keys to whichever table header appeared last, so placing
	// it after [fleet] (even on its own line) would silently become
	// fleet.manage_mcp instead of the top-level Config.ManageMCP.
	content := "manage_mcp = \"fleet-render\"\n\n" +
		"[fleet]\nid = \"test-device\"\nrepo_path = " + tomlValue(repo) + "\n\n" +
		"[profiles.alibaba1]\ndescription = \"alibaba1\"\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	profilesBase := ProfilesBaseDir(configPath)
	profileDir := filepath.Join(profilesBase, "alibaba1")

	// Seed a live ChannelInstall-written cpm-channel entry BEFORE syncing.
	cfg.Profiles["alibaba1"].ChannelPort = 8790
	if err := ChannelInstall(cfg, profilesBase, "alibaba1"); err != nil {
		t.Fatalf("ChannelInstall: %v", err)
	}

	report, err := RunSync(cfg, configPath, SyncOptions{Profile: "alibaba1", Apply: true, GOOS: "linux"})
	if err != nil {
		t.Fatalf("RunSync --apply: %v", err)
	}
	if !report.HasDrift() {
		t.Fatal("expected the staged mcpServers patch to register as drift/applied")
	}

	raw, err := os.ReadFile(filepath.Join(profileDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "cpm-channel") {
		t.Error("cpm sync --apply clobbered the live ChannelInstall-written cpm-channel entry")
	}
	if !strings.Contains(got, "socraticode") {
		t.Error("cpm sync --apply did not merge the staged mcpServers patch")
	}
}
