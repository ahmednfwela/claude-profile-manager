package internal

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallWrapper(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "claude-test")
	content := "#!/usr/bin/env bash\necho test\n"

	if err := InstallWrapper(scriptPath, content); err != nil {
		t.Fatalf("InstallWrapper failed: %v", err)
	}

	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("cannot read script: %v", err)
	}
	if string(data) != content {
		t.Errorf("script content mismatch")
	}

	// The executable bit is meaningless on Windows (NTFS ACLs govern execution).
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(scriptPath)
		if info.Mode()&0o111 == 0 {
			t.Error("script should be executable")
		}
	}
}

func TestInstallWrapperIdempotent(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "claude-test")
	content := "#!/usr/bin/env bash\necho test\n"

	InstallWrapper(scriptPath, content)
	info1, _ := os.Stat(scriptPath)

	// Second install with same content should not rewrite
	err := InstallWrapper(scriptPath, content)
	if err != nil {
		t.Fatalf("second InstallWrapper failed: %v", err)
	}

	info2, _ := os.Stat(scriptPath)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("wrapper should not be rewritten when content is unchanged")
	}
}

func TestCleanupStaleScripts(t *testing.T) {
	dir := t.TempDir()

	// Create a stale script with our marker
	stale := filepath.Join(dir, "claude-old")
	os.WriteFile(stale, []byte("#!/bin/bash\n"+marker+"\necho old\n"), 0o755)

	// Create an active script
	active := filepath.Join(dir, "claude-keep")
	os.WriteFile(active, []byte("#!/bin/bash\n"+marker+"\necho keep\n"), 0o755)

	// Create a non-cpm script
	other := filepath.Join(dir, "other-script")
	os.WriteFile(other, []byte("#!/bin/bash\necho other\n"), 0o755)

	// A non-launcher file that DOES embed the marker — e.g. the cpm binary,
	// which carries the marker as a compiled string constant. It must never be
	// matched/removed (regression: it was being deleted because the name guard
	// was missing and the marker substring matched the binary).
	binary := filepath.Join(dir, "cpm.exe")
	os.WriteFile(binary, []byte("MZ\x00\x00"+marker+"\x00compiled\n"), 0o755)

	activeNames := map[string]bool{"claude-keep": true}
	CleanupStaleScripts(dir, activeNames)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale script should have been removed")
	}
	if _, err := os.Stat(active); err != nil {
		t.Error("active script should be kept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("non-cpm script should be kept")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Error("a marker-bearing non-launcher (cpm binary) must NOT be removed")
	}
}
