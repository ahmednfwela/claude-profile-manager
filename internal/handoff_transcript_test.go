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

func TestSessionProjectDir(t *testing.T) {
	fullID := "12345678-1234-1234-1234-123456789abc"

	// Missing transcript -> refuse (same mis-route class as verifyTranscriptVisible).
	if _, err := sessionProjectDir(t.TempDir(), fullID); err == nil {
		t.Fatal("expected error when transcript is absent, got nil")
	}

	// Real-world shape: the first line is a custom-title record with a null cwd;
	// the cwd only appears on a later line. sessionProjectDir must skip the
	// null and return the first real cwd.
	dir := t.TempDir()
	slug := filepath.Join(dir, "projects", "D--projects-devops-aggregate")
	if err := os.MkdirAll(slug, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"custom-title","sessionId":"12345678-1234-1234-1234-123456789abc","cwd":null}
{"type":"attachment","cwd":"D:\\projects\\devops-aggregate","sessionKind":"bg"}
{"type":"assistant","cwd":"D:\\projects\\devops-aggregate"}
`
	if err := os.WriteFile(filepath.Join(slug, fullID+".jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sessionProjectDir(dir, fullID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `D:\projects\devops-aggregate`; got != want {
		t.Fatalf("cwd = %q, want %q", got, want)
	}

	// Transcript present but no line carries a cwd -> refuse rather than
	// re-dispatch in the wrong directory scope.
	dir2 := t.TempDir()
	slug2 := filepath.Join(dir2, "projects", "D--some-project")
	if err := os.MkdirAll(slug2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slug2, fullID+".jsonl"), []byte("{}\n{\"cwd\":null}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionProjectDir(dir2, fullID); err == nil {
		t.Fatal("expected error when no cwd is present in transcript, got nil")
	}
}
