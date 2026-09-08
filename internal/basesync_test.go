package internal

import (
	"strings"
	"testing"
)

func TestRenderBaseBlockDeterministicAndSorted(t *testing.T) {
	b := &Base{
		Model: "sonnet",
		Args:  []string{"--dangerously-skip-permissions", "--effort", "ultracode"},
		Env: map[string]any{
			"CLAUDE_CODE_MAX_OUTPUT_TOKENS": "128000",
			"ENABLE_TOOL_SEARCH":            "true",
			"windows": map[string]any{
				"CLAUDE_CODE_USE_POWERSHELL_TOOL": "1",
			},
			"darwin": map[string]any{},
		},
	}
	block := RenderBaseBlock(b)
	again := RenderBaseBlock(b)
	if block != again {
		t.Fatalf("RenderBaseBlock is not deterministic:\n---first---\n%s\n---second---\n%s", block, again)
	}
	for _, want := range []string{"[base]", `model = "sonnet"`, "[base.env]", "[base.env.windows]"} {
		if !strings.Contains(block, want) {
			t.Errorf("rendered block missing %q:\n%s", want, block)
		}
	}
	// darwin overlay is empty -- must not render an empty [base.env.darwin] section.
	if strings.Contains(block, "[base.env.darwin]") {
		t.Errorf("empty darwin overlay should not render a section header:\n%s", block)
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

	newBase := &Base{Model: "sonnet"}
	rendered := RenderBaseBlock(newBase)

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
	rendered := RenderBaseBlock(&Base{Model: "sonnet"})

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
	rendered := RenderBaseBlock(&Base{Model: "sonnet"})
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
