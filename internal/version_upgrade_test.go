package internal

import (
	"os"
	"path/filepath"
	"testing"
)

// Fixed-name ".old" collisions killed a live upgrade (2026-08-09, 0.5.0→0.5.1
// under running lanes): the previous upgrade's .old was still a running image,
// so Remove failed and the rename chain died "Access is denied". The reaper
// must delete every unlocked leftover aside and must never touch anything else.
func TestReapStaleAsides(t *testing.T) {
	dir := t.TempDir()
	stale1 := filepath.Join(dir, binaryName()+".old")
	stale2 := filepath.Join(dir, binaryName()+".old-1234-99999")
	keep := filepath.Join(dir, binaryName())
	for _, p := range []string{stale1, stale2, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	reapStaleAsides(dir)
	for _, p := range []string{stale1, stale2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale aside not reaped: %s", p)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("live binary must never be reaped: %v", err)
	}
}
