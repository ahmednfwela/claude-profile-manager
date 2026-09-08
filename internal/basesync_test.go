package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalBaseBlock is the smallest fleet-shaped [base] table the sync/doctor
// fixtures below share between "what base.toml says" and "what config.toml
// should hold" -- the two must be the SAME bytes for no-drift to be true.
const minimalBaseBlock = "[base]\nmodel = \"sonnet\""

// RenderBaseBlock is a verbatim wrapper: what fleet/profiles/base.toml says
// is what config.toml's [base] says -- key spellings, `~/` placeholders and
// the os_overlay inline table included -- because fleet-doctor F10 compares
// the two files key for key.
func TestRenderBaseBlockIsVerbatim(t *testing.T) {
	block := "[base]\n" +
		"description_prefix = \"Claude Max\"\n" +
		"model = \"\"\n" +
		"args = [\"--dangerously-skip-permissions\", \"~/.claude-profiles/lane-authority.md\"]\n" +
		"[base.env]\n" +
		"ENABLE_TOOL_SEARCH = \"true\"\n" +
		"os_overlay = { windows = { CLAUDE_CODE_USE_POWERSHELL_TOOL = \"1\" }, darwin = {} }\n"
	got := RenderBaseBlock(block)
	want := baseSentinelBeginLine + "\n" + strings.TrimRight(block, "\n") + "\n" + baseSentinelEndLine + "\n"
	if got != want {
		t.Fatalf("RenderBaseBlock rewrote its input:\n--- got\n%s\n--- want\n%s", got, want)
	}
	if RenderBaseBlock(block) != got || RenderBaseBlock(block+"\n\n") != got {
		t.Error("RenderBaseBlock must be deterministic and trailing-whitespace-insensitive")
	}

	// cpm's own config loader must read the spliced block back with the
	// overlay intact and the unknown description_prefix tolerated -- the
	// shape is shared with the fleet, not private to cpm.
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(got+"\n[profiles.p]\ndescription = \"p\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig on a spliced block: %v", err)
	}
	if cfg.Base == nil || cfg.Base.BaseEnvOverlay("windows")["CLAUDE_CODE_USE_POWERSHELL_TOOL"] != "1" {
		t.Errorf("spliced [base] did not round-trip through LoadConfig: %+v", cfg.Base)
	}
	if common := cfg.Base.BaseEnvCommon(); common["ENABLE_TOOL_SEARCH"] != "true" || len(common) != 1 {
		t.Errorf("BaseEnvCommon after round-trip = %v (os_overlay must not leak as a flat key)", common)
	}
	if len(cfg.Base.Args) != 2 || cfg.Base.Args[1] != "~/.claude-profiles/lane-authority.md" {
		t.Errorf("Args after round-trip = %v (placeholder must stay verbatim in [base])", cfg.Base.Args)
	}
}

// The headline new-code-path test (design spec §2): splicing a freshly
// rendered [base] block into config.toml must touch ONLY the byte range
// between the sentinel pair -- every [profiles.*] stanza, its own comments,
// and any channel_port pin annotations before or after the sentinel pair
// must survive byte-for-byte.
func TestSpliceBasePreservesBytesOutsideSentinels(t *testing.T) {
	original := "source_dir = \"~/.claude\"\n" +
		"bin_dir    = \"~/.local/bin\"\n\n" +
		baseSentinelBeginLine + "\n" +
		"[base]\n" +
		"model = \"OLD-MODEL\"\n" +
		baseSentinelEndLine + "\n\n" +
		"# bdaya -- added by `cpm add`. Isolated CLAUDE_CONFIG_DIR.\n" +
		"[profiles.bdaya]\n" +
		"description = \"Claude Max — bdaya\"\n" +
		"channel_port = 8791 # pinned, do not renumber\n\n" +
		"[profiles.bdaya.auth]\n" +
		"mode = \"oauth\"\n"

	rendered := RenderBaseBlock(minimalBaseBlock)

	spliced, changed, err := SpliceBase(original, rendered)
	if err != nil {
		t.Fatalf("SpliceBase: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true (model differs)")
	}

	// Everything before the BEGIN sentinel must be byte-identical.
	beginIdx := strings.Index(original, baseSentinelBeginLine)
	if beginIdx < 0 {
		t.Fatal("test fixture missing its own BEGIN sentinel")
	}
	prefix := original[:beginIdx]
	if spliced[:len(prefix)] != prefix {
		t.Fatalf("prefix bytes were modified:\nwant %q\ngot  %q", prefix, spliced[:len(prefix)])
	}

	// Everything after the END sentinel's line must be byte-identical.
	endIdx := strings.Index(original, baseSentinelEndLine) + len(baseSentinelEndLine)
	suffix := original[endIdx:]
	if spliced[len(spliced)-len(suffix):] != suffix {
		t.Fatalf("suffix bytes were modified:\nwant %q\ngot  %q", suffix, spliced[len(spliced)-len(suffix):])
	}

	// The new content actually landed in the spliced range.
	if !strings.Contains(spliced, "sonnet") {
		t.Error("spliced content does not contain the new model value")
	}
	if strings.Contains(spliced, "OLD-MODEL") {
		t.Error("spliced content still contains the old model value")
	}

	// Re-splicing the identical rendered block a second time is a no-op.
	again, changed2, err := SpliceBase(spliced, rendered)
	if err != nil {
		t.Fatalf("second SpliceBase: %v", err)
	}
	if changed2 {
		t.Error("re-splicing an identical block should report changed=false")
	}
	if again != spliced {
		t.Error("re-splicing an identical block must be byte-identical (no accumulating blank lines etc.)")
	}
}

func TestSpliceBaseAppendsWhenNoSentinelsPresent(t *testing.T) {
	original := "source_dir = \"~/.claude\"\n\n[profiles.bdaya]\ndescription = \"x\"\n"
	rendered := RenderBaseBlock(minimalBaseBlock)

	spliced, changed, err := SpliceBase(original, rendered)
	if err != nil {
		t.Fatalf("SpliceBase: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when appending a first [base] block")
	}
	if spliced[:len(original)] != original {
		t.Fatalf("appending a new [base] block must not disturb existing content:\nwant prefix %q\ngot  %q", original, spliced[:len(original)])
	}
	if !strings.Contains(spliced, "sonnet") {
		t.Error("appended block missing the rendered content")
	}
}

func TestExtractBaseRoundTrip(t *testing.T) {
	rendered := RenderBaseBlock(minimalBaseBlock)
	content := "x = 1\n" + rendered + "\ny = 2\n"

	got, ok := ExtractBase(content)
	if !ok {
		t.Fatal("ExtractBase did not find the sentinel-delimited block")
	}
	want := strings.TrimRight(rendered, "\n")
	if got != want {
		t.Errorf("ExtractBase = %q, want %q", got, want)
	}

	if _, ok := ExtractBase("no sentinels here"); ok {
		t.Error("ExtractBase should report ok=false when no sentinels are present")
	}
}
