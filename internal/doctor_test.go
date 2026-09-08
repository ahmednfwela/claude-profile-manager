package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunDoctorBasic(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	profilesBase := filepath.Join(tmpDir, "profiles")
	binDir := filepath.Join(tmpDir, "bin")

	os.MkdirAll(sourceDir, 0o755)
	os.MkdirAll(profilesBase, 0o755)
	os.MkdirAll(binDir, 0o755)

	cfg := &Config{
		SourceDir: sourceDir,
		BinDir:    binDir,
		Profiles: map[string]*Profile{
			"test": {Description: "Test"},
		},
	}

	checks := RunDoctor(cfg, profilesBase, "")

	// Should have at least checks for claude binary, source dir, bin dir
	if len(checks) < 3 {
		t.Errorf("expected at least 3 checks, got %d", len(checks))
	}

	// Source dir should be OK
	found := false
	for _, c := range checks {
		if c.Name == "source directory" && c.Status == "ok" {
			found = true
			break
		}
	}
	if !found {
		t.Error("source directory check should be OK")
	}
}

func TestRunDoctorMissingSourceDir(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &Config{
		SourceDir: filepath.Join(tmpDir, "nonexistent"),
		BinDir:    tmpDir,
		Profiles:  map[string]*Profile{"test": {}},
	}

	checks := RunDoctor(cfg, tmpDir, "")

	found := false
	for _, c := range checks {
		if c.Name == "source directory" && c.Status == "error" {
			found = true
			break
		}
	}
	if !found {
		t.Error("missing source directory should produce error check")
	}
}

func TestRunDoctorProfileNotInstalled(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	os.MkdirAll(sourceDir, 0o755)

	cfg := &Config{
		SourceDir: sourceDir,
		BinDir:    tmpDir,
		Profiles:  map[string]*Profile{"missing": {Description: "Missing"}},
	}

	checks := RunDoctor(cfg, tmpDir, "")

	found := false
	for _, c := range checks {
		if c.Name == "profile/missing" && c.Status == "warn" {
			found = true
			break
		}
	}
	if !found {
		t.Error("uninstalled profile should produce warn check")
	}
}

// --- cpm-unification doctor extensions (design spec §4) --------------------

// TestRunDoctorOrphanScan is the integration test named by the design spec's
// work item (A): a directory present under profilesBase with no matching
// config.toml [profiles.*] key must be flagged, and RunDoctor must not touch
// it in any way (no delete, no rename, no adopt -- "NEVER touched, ever").
func TestRunDoctorOrphanScan(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	profilesBase := filepath.Join(tmpDir, "profiles")
	os.MkdirAll(sourceDir, 0o755)
	os.MkdirAll(filepath.Join(profilesBase, "registered"), 0o755)

	// The deliberately-unregistered directory -- not in cfg.Profiles at all.
	orphanDir := filepath.Join(profilesBase, "orphan-leftover")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(orphanDir, "settings.json")
	if err := os.WriteFile(sentinel, []byte(`{"marker":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(sentinel)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		SourceDir: sourceDir,
		BinDir:    tmpDir,
		Profiles:  map[string]*Profile{"registered": {Description: "Registered"}},
	}

	checks := RunDoctor(cfg, profilesBase, "")

	found := false
	for _, c := range checks {
		if c.Status == "drift" && strings.Contains(c.Detail, "ORPHAN") && strings.Contains(c.Detail, "orphan-leftover") {
			found = true
		}
		// Must never name the REGISTERED directory as an orphan.
		if strings.Contains(c.Detail, "ORPHAN") && strings.Contains(c.Detail, string(filepath.Separator)+"registered") {
			t.Errorf("registered profile directory was misreported as an orphan: %+v", c)
		}
	}
	if !found {
		t.Errorf("expected an ORPHAN drift check naming orphan-leftover, got: %+v", checks)
	}

	// Never touched: same size, same content, still present.
	after, err := os.Stat(sentinel)
	if err != nil {
		t.Fatalf("orphan directory's file was removed/moved by RunDoctor: %v", err)
	}
	if after.Size() != before.Size() {
		t.Error("orphan directory's file was modified by RunDoctor")
	}
	raw, _ := os.ReadFile(sentinel)
	if string(raw) != `{"marker":true}` {
		t.Error("orphan directory's file content changed")
	}
}

func TestRunDoctorNoOrphanFalsePositiveWhenEverythingRegistered(t *testing.T) {
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	profilesBase := filepath.Join(tmpDir, "profiles")
	os.MkdirAll(sourceDir, 0o755)
	os.MkdirAll(filepath.Join(profilesBase, "a"), 0o755)
	os.MkdirAll(filepath.Join(profilesBase, "b"), 0o755)

	cfg := &Config{
		SourceDir: sourceDir,
		BinDir:    tmpDir,
		Profiles:  map[string]*Profile{"a": {}, "b": {}},
	}
	checks := RunDoctor(cfg, profilesBase, "")
	for _, c := range checks {
		if c.Status == "drift" && strings.Contains(c.Detail, "ORPHAN") {
			t.Errorf("false-positive orphan report: %+v", c)
		}
	}
}

// TestRunDoctorBaseLocalModificationFlag: if the live [base] table doesn't
// match fleet/profiles/base.toml's current content, `cpm doctor` must flag
// it BEFORE the next sync silently overwrites it (design spec §4).
func TestRunDoctorBaseLocalModificationFlag(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "fleet-repo")
	os.MkdirAll(filepath.Join(repo, "profiles"), 0o755)
	os.WriteFile(filepath.Join(repo, "profiles", "base.toml"), []byte(`model = "sonnet"`), 0o644)

	sourceDir := filepath.Join(dir, "source")
	os.MkdirAll(sourceDir, 0o755)
	profilesBase := filepath.Join(dir, "profiles")
	os.MkdirAll(profilesBase, 0o755)

	configPath := filepath.Join(dir, "config.toml")
	content := "source_dir = " + tomlValue(sourceDir) + "\n" +
		"[fleet]\nrepo_path = " + tomlValue(repo) + "\n\n" +
		baseSentinelBeginLine + "\n[base]\nmodel = \"LOCALLY-EDITED\"\n" + baseSentinelEndLine + "\n\n" +
		"[profiles.p]\ndescription = \"p\"\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	checks := RunDoctor(cfg, profilesBase, configPath)

	found := false
	for _, c := range checks {
		if c.Status == "drift" && strings.Contains(c.Name, "base") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a [base] local-modification drift check, got: %+v", checks)
	}

	// Doctor must never write config.toml itself.
	raw, _ := os.ReadFile(configPath)
	if !strings.Contains(string(raw), "LOCALLY-EDITED") {
		t.Error("cpm doctor must never overwrite [base] itself -- that is cpm sync --apply's job")
	}
}

func TestRunDoctorNoBaseDriftWhenInSync(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "fleet-repo")
	os.MkdirAll(filepath.Join(repo, "profiles"), 0o755)
	os.WriteFile(filepath.Join(repo, "profiles", "base.toml"), []byte(`model = "sonnet"`), 0o644)

	sourceDir := filepath.Join(dir, "source")
	os.MkdirAll(sourceDir, 0o755)
	profilesBase := filepath.Join(dir, "profiles")
	os.MkdirAll(profilesBase, 0o755)

	configPath := filepath.Join(dir, "config.toml")
	content := "source_dir = " + tomlValue(sourceDir) + "\n" +
		"[fleet]\nrepo_path = " + tomlValue(repo) + "\n\n" +
		RenderBaseBlock(&Base{Model: "sonnet"}) + "\n" +
		"[profiles.p]\ndescription = \"p\"\n"
	os.WriteFile(configPath, []byte(content), 0o644)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	checks := RunDoctor(cfg, profilesBase, configPath)
	for _, c := range checks {
		if c.Status == "drift" && strings.Contains(c.Name, "base") {
			t.Errorf("false-positive [base] drift report: %+v", c)
		}
	}
}

func TestGetCredentialInfoNoFile(t *testing.T) {
	dir := t.TempDir()
	_, _, err := GetCredentialInfo(dir)
	if err == nil {
		t.Error("expected error for missing credentials")
	}
}

func TestGetCredentialInfoWithEmail(t *testing.T) {
	dir := t.TempDir()
	creds := map[string]any{
		"email":      "user@example.com",
		"expires_at": float64(time.Now().Add(time.Hour).Unix()),
	}
	data, _ := json.Marshal(creds)
	os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o644)

	account, expired, err := GetCredentialInfo(dir)
	if err != nil {
		t.Fatalf("GetCredentialInfo failed: %v", err)
	}
	if account != "user@example.com" {
		t.Errorf("account = %q, want user@example.com", account)
	}
	if expired {
		t.Error("credentials should not be expired")
	}
}

func TestGetCredentialInfoExpired(t *testing.T) {
	dir := t.TempDir()
	creds := map[string]any{
		"email":      "user@example.com",
		"expires_at": float64(time.Now().Add(-time.Hour).Unix()),
	}
	data, _ := json.Marshal(creds)
	os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o644)

	_, expired, err := GetCredentialInfo(dir)
	if err != nil {
		t.Fatalf("GetCredentialInfo failed: %v", err)
	}
	if !expired {
		t.Error("credentials should be expired")
	}
}

// TestGetCredentialInfo_ClaudeAiOauthExpiry covers the real credentials
// shape (nested claudeAiOauth.expiresAt, millisecond epoch) that
// GetCredentialInfo previously never looked at -- it only checked flat
// legacy keys, so every real profile silently read as "valid" regardless of
// actual expiry (live-verified: `cpm credentials` reported
// "(unknown account) [valid]" for 6/6 profiles while 5 had access tokens
// already expired). This is the same file cpm fleet creds's CredSnapshot
// reads, so the two must not disagree.
func TestGetCredentialInfo_ClaudeAiOauthExpiry(t *testing.T) {
	dir := t.TempDir()
	pastMs := time.Now().Add(-time.Hour).UnixMilli()
	creds := map[string]any{
		"claudeAiOauth": map[string]any{
			"expiresAt":    float64(pastMs),
			"refreshToken": "fake-refresh-token",
		},
	}
	data, _ := json.Marshal(creds)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, expired, err := GetCredentialInfo(dir)
	if err != nil {
		t.Fatalf("GetCredentialInfo failed: %v", err)
	}
	if !expired {
		t.Error("claudeAiOauth.expiresAt in the past should report expired=true")
	}
}

func TestGetCredentialInfo_ClaudeAiOauthNotExpired(t *testing.T) {
	dir := t.TempDir()
	futureMs := time.Now().Add(3 * time.Hour).UnixMilli()
	creds := map[string]any{
		"claudeAiOauth": map[string]any{
			"expiresAt": float64(futureMs),
		},
	}
	data, _ := json.Marshal(creds)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, expired, err := GetCredentialInfo(dir)
	if err != nil {
		t.Fatalf("GetCredentialInfo failed: %v", err)
	}
	if expired {
		t.Error("claudeAiOauth.expiresAt in the future should report expired=false")
	}
}

// Legacy flat expires_at (seconds epoch) must still work -- this is the
// exact fixture TestGetCredentialInfoWithEmail/Expired above already cover,
// re-asserted here explicitly as the "fallback path" half of the doctor.go
// fix contract.
func TestGetCredentialInfo_LegacyExpiresAtStillHonored(t *testing.T) {
	dir := t.TempDir()
	creds := map[string]any{"expires_at": float64(time.Now().Add(time.Hour).Unix())}
	data, _ := json.Marshal(creds)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, expired, err := GetCredentialInfo(dir)
	if err != nil {
		t.Fatalf("GetCredentialInfo failed: %v", err)
	}
	if expired {
		t.Error("legacy expires_at in the future should report expired=false")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "30m"},
		{5 * time.Hour, "5h"},
		{3 * 24 * time.Hour, "3d"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestPrintChecks(t *testing.T) {
	// Just verify it doesn't panic
	checks := []Check{
		{"test ok", "ok", "details"},
		{"test warn", "warn", "details"},
		{"test error", "error", "details"},
	}
	PrintChecks(checks)
}
