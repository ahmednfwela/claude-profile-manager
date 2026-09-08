package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestConfig writes a minimal config.toml declaring the given profile
// names, so CloneProfile's config-append step has a real file to append to.
func writeTestConfig(t *testing.T, configPath string, profileNames ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("source_dir = \"~/.claude\"\n")
	for _, name := range profileNames {
		fmt.Fprintf(&b, "\n[profiles.%s]\ndescription = %q\n", name, name)
	}
	if err := os.WriteFile(configPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCloneProfile(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	profilesBase := filepath.Join(tmpDir, "profiles")
	srcProfile := filepath.Join(profilesBase, "original")
	configPath := filepath.Join(tmpDir, "config.toml")

	os.MkdirAll(sourceDir, 0o755)
	os.MkdirAll(srcProfile, 0o755)
	writeTestConfig(t, configPath, "original")

	// Create source files
	os.WriteFile(filepath.Join(srcProfile, "settings.json"), []byte(`{"test": true}`), 0o644)
	os.WriteFile(filepath.Join(srcProfile, "CLAUDE.md"), []byte("# Claude"), 0o644)

	// Create source dirs for symlinks
	for _, dir := range []string{"skills", "plugins"} {
		os.MkdirAll(filepath.Join(sourceDir, dir), 0o755)
	}

	cfg := &Config{
		SourceDir: sourceDir,
		Profiles: map[string]*Profile{
			"original": {Description: "Original"},
		},
	}

	err := CloneProfile("original", "cloned", profilesBase, sourceDir, configPath, cfg)
	if err != nil {
		t.Fatalf("CloneProfile failed: %v", err)
	}

	clonedDir := filepath.Join(profilesBase, "cloned")

	// Check copied files
	data, err := os.ReadFile(filepath.Join(clonedDir, "settings.json"))
	if err != nil {
		t.Fatal("settings.json not cloned")
	}
	if string(data) != `{"test": true}` {
		t.Errorf("cloned settings.json = %q", string(data))
	}

	// Check links point to source dir (not the original profile).
	// linkPointsTo works for both POSIX symlinks and Windows junctions.
	for _, dir := range []string{"skills", "plugins"} {
		link := filepath.Join(clonedDir, dir)
		expected := filepath.Join(sourceDir, dir)
		if !linkPointsTo(link, expected) {
			t.Errorf("%s should link to %s", dir, expected)
		}
	}
}

func TestCloneProfileSourceNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &Config{
		Profiles: map[string]*Profile{
			"missing": {Description: "Missing"},
		},
	}

	configPath := filepath.Join(tmpDir, "config.toml") // never reached: error fires before any append
	err := CloneProfile("missing", "new", tmpDir, tmpDir, configPath, cfg)
	if err == nil {
		t.Error("expected error for uninstalled source profile")
	}
}

func TestCloneProfileTargetExists(t *testing.T) {
	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, "source"), 0o755)
	os.MkdirAll(filepath.Join(tmpDir, "target"), 0o755)

	cfg := &Config{
		Profiles: map[string]*Profile{
			"source": {Description: "Source"},
		},
	}

	configPath := filepath.Join(tmpDir, "config.toml") // never reached: error fires before any append
	err := CloneProfile("source", "target", tmpDir, tmpDir, configPath, cfg)
	if err == nil {
		t.Error("expected error when target profile already exists")
	}
}

func TestCloneProfileDoesNotCopyCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	profilesBase := filepath.Join(tmpDir, "profiles")
	srcProfile := filepath.Join(profilesBase, "original")

	os.MkdirAll(sourceDir, 0o755)
	os.MkdirAll(srcProfile, 0o755)

	// Create credentials in source profile
	os.WriteFile(filepath.Join(srcProfile, ".credentials.json"), []byte(`{"token": "secret"}`), 0o644)
	os.WriteFile(filepath.Join(srcProfile, "settings.json"), []byte(`{}`), 0o644)

	cfg := &Config{
		SourceDir: sourceDir,
		Profiles: map[string]*Profile{
			"original": {Description: "Original"},
		},
	}

	configPath := filepath.Join(tmpDir, "config.toml")
	writeTestConfig(t, configPath, "original")

	CloneProfile("original", "cloned", profilesBase, sourceDir, configPath, cfg)

	clonedDir := filepath.Join(profilesBase, "cloned")
	if _, err := os.Stat(filepath.Join(clonedDir, ".credentials.json")); !os.IsNotExist(err) {
		t.Error("credentials should NOT be cloned")
	}
}

// --- cpm-unification: [profiles.<dst>] + auth placeholder (design spec §4) -

// TestCloneProfileAppendsAuthPlaceholder is the acceptance-criterion test:
// `cpm clone` must leave config.toml with a syntactically valid placeholder
// [profiles.<dst>.auth] stanza a human can fill in, closing the exact gap
// that left profiles like alibaba1/synthetic1 config.toml-blind.
func TestCloneProfileAppendsAuthPlaceholder(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	profilesBase := filepath.Join(tmpDir, "profiles")
	srcProfile := filepath.Join(profilesBase, "original")
	configPath := filepath.Join(tmpDir, "config.toml")

	os.MkdirAll(sourceDir, 0o755)
	os.MkdirAll(srcProfile, 0o755)
	writeTestConfig(t, configPath, "original")

	cfg := &Config{SourceDir: sourceDir, Profiles: map[string]*Profile{"original": {}}}
	if err := CloneProfile("original", "synthetic1", profilesBase, sourceDir, configPath, cfg); err != nil {
		t.Fatalf("CloneProfile failed: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// The appended block must parse as valid TOML and be loadable by cpm
	// itself -- "syntactically valid" is proven by round-tripping it through
	// LoadConfig, not just string-matching the raw bytes.
	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("config.toml no longer parses after clone: %v\n--- content ---\n%s", err, raw)
	}
	synthetic, ok := reloaded.Profiles["synthetic1"]
	if !ok {
		t.Fatalf("[profiles.synthetic1] was not appended:\n%s", raw)
	}
	if synthetic.Auth == nil {
		t.Fatalf("[profiles.synthetic1.auth] placeholder was not appended:\n%s", raw)
	}
	if synthetic.Auth.Mode != "" {
		t.Errorf("a freshly-cloned placeholder's auth.mode should be blank (TODO), got %q", synthetic.Auth.Mode)
	}

	// The pre-existing [profiles.original] stanza must survive untouched.
	if _, ok := reloaded.Profiles["original"]; !ok {
		t.Error("cloning must not disturb the existing [profiles.original] stanza")
	}
}
