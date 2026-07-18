package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyTranscriptVisible(t *testing.T) {
	dir := t.TempDir()
	fullID := "12345678-1234-1234-1234-123456789abc"

	// Missing projects tree -> refuse with the mis-route explanation.
	if err := verifyTranscriptVisible(dir, fullID); err == nil {
		t.Fatal("expected error when transcript is absent, got nil")
	}

	slug := filepath.Join(dir, "projects", "D--some-project")
	if err := os.MkdirAll(slug, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slug, fullID+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Present under any project slug -> pass.
	if err := verifyTranscriptVisible(dir, fullID); err != nil {
		t.Fatalf("expected transcript to be visible, got: %v", err)
	}
}
