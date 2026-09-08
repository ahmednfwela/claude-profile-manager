package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The mirror-image fixture named by the design spec §5/§7A: a mcpServers
// patch application must be additive-merge-only, preserving a live entry it
// did not itself write -- here, one written the exact way ChannelInstall
// writes cpm-channel (internal/channel.go), so this test fails the moment
// `cpm sync`'s writer becomes a full-map replace instead of a key-level
// merge. See channel_test.go's TestChannelInstallPreservesExistingServers for
// the existing, symmetric regression test on ChannelInstall's own side of
// this same shared map.
func TestMergeMCPServersPatchPreservesChannelInstallEntry(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "bdaya")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed .claude.json exactly the way ChannelInstall would leave it: a
	// live cpm-channel entry plus an unrelated top-level key.
	cfg := cfgWith("bdaya")
	cfg.Profiles["bdaya"].ChannelPort = 8790
	if err := ChannelInstall(cfg, dir, "bdaya"); err != nil {
		t.Fatalf("ChannelInstall: %v", err)
	}

	patch := map[string]any{
		"socraticode": map[string]any{
			"type": "http",
			"url":  "http://127.0.0.1:9999/mcp",
		},
	}
	changed, err := MergeMCPServersPatch(profileDir, patch)
	if err != nil {
		t.Fatalf("MergeMCPServersPatch: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true (a new server key was merged in)")
	}

	raw, err := os.ReadFile(filepath.Join(profileDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("invalid JSON after merge: %v", err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)

	if servers["cpm-channel"] == nil {
		t.Fatal("merge clobbered the live ChannelInstall-written cpm-channel entry")
	}
	cc, _ := servers["cpm-channel"].(map[string]any)
	if cc["url"] != "http://127.0.0.1:8790/mcp" {
		t.Errorf("cpm-channel entry was modified: %v", cc)
	}

	if servers["socraticode"] == nil {
		t.Fatal("merge did not apply the new patch key")
	}

	// Re-applying the identical patch is a no-op.
	changed2, err := MergeMCPServersPatch(profileDir, patch)
	if err != nil {
		t.Fatalf("second MergeMCPServersPatch: %v", err)
	}
	if changed2 {
		t.Error("re-applying an identical patch should report changed=false")
	}
}

func TestMergeMCPServersPatchPreservesUnrelatedTopLevelKeys(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".claude.json"),
		[]byte(`{"mcpServers":{"keepme":{"type":"http","url":"http://example/mcp"}},"other":42}`), 0o644)

	patch := map[string]any{"newserver": map[string]any{"type": "http", "url": "http://x/mcp"}}
	if _, err := MergeMCPServersPatch(dir, patch); err != nil {
		t.Fatalf("MergeMCPServersPatch: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, ".claude.json"))
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if doc["other"] == nil {
		t.Error("merge dropped an unrelated top-level key")
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers["keepme"] == nil {
		t.Error("merge dropped an unrelated existing mcpServers key")
	}
	if servers["newserver"] == nil {
		t.Error("merge did not add the new patch key")
	}
}

func TestMergeMCPServersPatchEmptyPatchIsNoop(t *testing.T) {
	dir := t.TempDir()
	changed, err := MergeMCPServersPatch(dir, nil)
	if err != nil {
		t.Fatalf("MergeMCPServersPatch(nil): %v", err)
	}
	if changed {
		t.Error("an empty/nil patch must never report changed=true")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude.json")); !os.IsNotExist(err) {
		t.Error("an empty/nil patch must not create .claude.json")
	}
}

func TestDiffMCPServersPatchDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"mcpServers":{}}`), 0o644)

	patch := map[string]any{"x": map[string]any{"type": "http", "url": "http://x/mcp"}}
	before, after, changed, err := DiffMCPServersPatch(dir, patch)
	if err != nil {
		t.Fatalf("DiffMCPServersPatch: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if before == after {
		t.Error("before/after should differ when a patch introduces a new key")
	}

	raw, _ := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if string(raw) != `{"mcpServers":{}}` {
		t.Error("DiffMCPServersPatch must never write to disk")
	}
}
