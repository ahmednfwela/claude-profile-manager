package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	checks := RunDoctor(cfg, profilesBase)

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

	checks := RunDoctor(cfg, tmpDir)

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

	checks := RunDoctor(cfg, tmpDir)

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
