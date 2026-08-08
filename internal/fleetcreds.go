package internal

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sshExitAbsent/sshExitDirAbsent are deterministic, locale-independent
// sentinel exit codes the remote shell snippets use so callers never have to
// string-match an error message ("No such file or directory" differs across
// shells/locales). Distinct values so a read-absent and a write-target-absent
// are never confused with each other.
const (
	sshExitAbsent    = 3 // read: the peer has no credentials file for this alias
	sshExitDirAbsent = 4 // write: the peer has no profile directory for this alias
)

// CredSnapshot summarizes one machine's .credentials.json for a profile
// WITHOUT ever holding a printable secret. Fingerprint is the only field
// derived from a secret (a 12-hex-char prefix of sha256(refreshToken)) and is
// not invertible — it identifies a token *lineage*, not the token. raw holds
// the full file bytes only when a caller explicitly asked for them (a
// transfer), and is never rendered by any Sprintf/Print in this file.
type CredSnapshot struct {
	Present     bool
	ExpiresAt   time.Time // claudeAiOauth.expiresAt
	RefreshExp  time.Time // claudeAiOauth.refreshTokenExpiresAt
	Fingerprint string    // sha256(claudeAiOauth.refreshToken)[:12] hex; "" if absent
	SubType     string    // claudeAiOauth.subscriptionType
	Tier        string    // claudeAiOauth.rateLimitTier
	MCPKeys     []string  // mcpOAuth key NAMES only, sorted — never values
	ModTime     time.Time // file mtime; skew-sensitive tiebreak only, not the primary comparator
	raw         []byte    // full file bytes; populated only when a transfer needs them
}

// CredSyncOpts holds the flags shared by status/push/pull.
type CredSyncOpts struct {
	Peers      []string
	All        bool
	Yes        bool
	Force      bool
	DryRun     bool
	IncludeMCP bool
}

// fingerprint derives a lineage identifier from a refresh token: a truncated
// SHA-256 prefix. 48 bits of a cryptographic hash over a high-entropy secret
// is not invertible, so this is safe to print — it is NOT the secret.
func fingerprint(refreshToken string) string {
	sum := sha256.Sum256([]byte(refreshToken))
	return hex.EncodeToString(sum[:])[:12]
}

// msEpochToTime converts a millisecond epoch to a time.Time, but refuses any
// value that decodes to before 2020 — which almost certainly means a
// caller/file handed us a SECONDS epoch by mistake (a real seconds-epoch
// value read as milliseconds lands in January 1970, not a plausible
// credential expiry). Returning ok=false lets the caller leave the field
// zero rather than silently trusting a nonsense timestamp.
func msEpochToTime(ms int64) (t time.Time, ok bool) {
	t = time.UnixMilli(ms)
	if t.Year() < 2020 {
		return time.Time{}, false
	}
	return t, true
}

// parseCredSnapshot parses raw .credentials.json bytes into a CredSnapshot.
// modTime is the file's mtime (zero for a remote read, where mtime is not
// meaningful across machines with clock skew).
func parseCredSnapshot(data []byte, modTime time.Time) (CredSnapshot, error) {
	snap := CredSnapshot{Present: true, ModTime: modTime}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return CredSnapshot{}, fmt.Errorf("parse credentials: %w", err)
	}

	if raw, ok := top["claudeAiOauth"]; ok {
		var oauth struct {
			AccessToken           string      `json:"accessToken"`
			RefreshToken          string      `json:"refreshToken"`
			ExpiresAt              json.Number `json:"expiresAt"`
			RefreshTokenExpiresAt  json.Number `json:"refreshTokenExpiresAt"`
			SubscriptionType       string      `json:"subscriptionType"`
			RateLimitTier          string      `json:"rateLimitTier"`
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&oauth); err != nil {
			return CredSnapshot{}, fmt.Errorf("parse claudeAiOauth: %w", err)
		}
		if oauth.ExpiresAt != "" {
			if ms, err := oauth.ExpiresAt.Int64(); err == nil {
				if t, ok := msEpochToTime(ms); ok {
					snap.ExpiresAt = t
				}
			}
		}
		if oauth.RefreshTokenExpiresAt != "" {
			if ms, err := oauth.RefreshTokenExpiresAt.Int64(); err == nil {
				if t, ok := msEpochToTime(ms); ok {
					snap.RefreshExp = t
				}
			}
		}
		snap.SubType = oauth.SubscriptionType
		snap.Tier = oauth.RateLimitTier
		if oauth.RefreshToken != "" {
			snap.Fingerprint = fingerprint(oauth.RefreshToken)
		}
	}

	if raw, ok := top["mcpOAuth"]; ok {
		var mcp map[string]json.RawMessage
		if err := json.Unmarshal(raw, &mcp); err == nil {
			keys := make([]string, 0, len(mcp))
			for k := range mcp {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			snap.MCPKeys = keys
		}
	}

	return snap, nil
}

// ReadLocalCredSnapshot reads this machine's .credentials.json for a
// profile. Absence is not an error (Present=false, err=nil) — every caller
// that loops over --all profiles needs to treat "not logged in here" as a
// normal, skippable state.
func ReadLocalCredSnapshot(profileDir string, withRaw bool) (CredSnapshot, error) {
	path := filepath.Join(profileDir, ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CredSnapshot{Present: false}, nil
		}
		return CredSnapshot{}, fmt.Errorf("read local credentials: %w", err)
	}
	var modTime time.Time
	if info, statErr := os.Stat(path); statErr == nil {
		modTime = info.ModTime()
	}
	snap, err := parseCredSnapshot(data, modTime)
	if err != nil {
		return CredSnapshot{}, fmt.Errorf("%s: %w", path, err)
	}
	if withRaw {
		snap.raw = data
	}
	return snap, nil
}

// MergeCredentials produces the bytes to write to a destination
// .credentials.json: start from dst (preserving mcpOAuth and every unknown
// top-level key), overwrite ONLY claudeAiOauth from src. With includeMCP,
// mcpOAuth is also overwritten from src (or removed if src has none). If dst
// is empty/absent, the result is a claudeAiOauth-only document — the
// live-verified shape of a healthy profile.
//
// This is the load-bearing decision on mcpOAuth: those grants are per-machine
// (keyed by a config-hash suffix) and belong to third-party integrations
// (Cloudflare, Figma, Sentry, ...), not to the Claude account being handed
// off. A whole-file replace would silently delete the destination's own MCP
// grants; merge-not-replace never does.
func MergeCredentials(dst, src []byte, includeMCP bool) ([]byte, error) {
	var srcMap map[string]json.RawMessage
	if err := json.Unmarshal(src, &srcMap); err != nil {
		return nil, fmt.Errorf("parse source credentials: %w", err)
	}
	srcOAuth, ok := srcMap["claudeAiOauth"]
	if !ok {
		return nil, fmt.Errorf("source credentials have no claudeAiOauth key")
	}

	result := map[string]json.RawMessage{}
	if len(dst) > 0 {
		if err := json.Unmarshal(dst, &result); err != nil {
			return nil, fmt.Errorf("parse destination credentials: %w", err)
		}
	}
	result["claudeAiOauth"] = srcOAuth

	if includeMCP {
		if mcp, ok := srcMap["mcpOAuth"]; ok {
			result["mcpOAuth"] = mcp
		} else {
			delete(result, "mcpOAuth")
		}
	}

	// encoding/json sorts map[string]... keys alphabetically when marshaling,
	// so this output is deterministic without any extra bookkeeping.
	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal merged credentials: %w", err)
	}
	return out, nil
}

// destHasExtraMCPKeys reports whether dst has an mcpOAuth grant that src does
// not — the condition Gate B(b3) refuses under --include-mcp, since
// replacing mcpOAuth wholesale would otherwise delete it.
func destHasExtraMCPKeys(dst, src CredSnapshot) bool {
	srcSet := make(map[string]bool, len(src.MCPKeys))
	for _, k := range src.MCPKeys {
		srcSet[k] = true
	}
	for _, k := range dst.MCPKeys {
		if !srcSet[k] {
			return true
		}
	}
	return false
}

// transferDecision is the verdict planTransfer computes for one (src, dst)
// pair. Exactly one of "plain write" / Routine / Blocking applies when
// Allowed is true or Blocking is true.
type transferDecision struct {
	Allowed  bool   // false only when there is nothing to do (source absent) or Blocking && no --force
	Blocking bool   // Gate B: a hard refusal that needs --force even with --yes
	Routine  bool   // Gate A: same-lineage/older overwrite that needs --yes (or a prompt)
	Reason   string
}

// planTransfer computes the transfer verdict for one src -> dst pair. It is
// pure (no I/O, no clock mutation beyond time.Now()) so it is exhaustively
// unit-testable without a network or filesystem. Direction-agnostic: for
// push, src is local and dst is the peer; for pull, src is the peer and dst
// is local.
func planTransfer(src, dst CredSnapshot, includeMCP bool) transferDecision {
	if !src.Present {
		return transferDecision{Allowed: false, Reason: "source has no credentials — skipped"}
	}

	if !dst.Present {
		return transferDecision{Allowed: true, Reason: "destination has no credentials yet — plain write"}
	}

	// Gate B(b2): never propagate a source whose refresh chain is already dead.
	if !src.RefreshExp.IsZero() && src.RefreshExp.Before(time.Now()) {
		return transferDecision{
			Allowed:  false,
			Blocking: true,
			Reason:   fmt.Sprintf("source refresh token EXPIRED at %s — pushing it would overwrite a possibly-live destination with a dead token", src.RefreshExp.Format("2006-01-02 15:04")),
		}
	}

	// Gate B(b1): never overwrite a strictly-newer destination.
	if dst.ExpiresAt.After(src.ExpiresAt) {
		return transferDecision{
			Allowed:  false,
			Blocking: true,
			Reason: fmt.Sprintf("destination is NEWER (expires %s) than source (expires %s) — overwriting would invalidate a working session",
				dst.ExpiresAt.Format("2006-01-02 15:04"), src.ExpiresAt.Format("2006-01-02 15:04")),
		}
	}

	// Gate B(b3): --include-mcp must never delete a destination-only grant.
	if includeMCP && destHasExtraMCPKeys(dst, src) {
		return transferDecision{
			Allowed:  false,
			Blocking: true,
			Reason:   "--include-mcp would delete destination mcpOAuth grant(s) the source does not have",
		}
	}

	sameLineage := src.Fingerprint != "" && src.Fingerprint == dst.Fingerprint
	if sameLineage {
		return transferDecision{Allowed: true, Routine: true, Reason: "destination already has this profile (same lineage)"}
	}
	return transferDecision{Allowed: true, Routine: true, Reason: "destination already has this profile (DIFFERENT lineage — divergent logins)"}
}

// validateCredSyncArgs enforces the mutual-exclusion / required-choice rule
// shared by push and pull: aliases XOR --all, never neither, never both.
func validateCredSyncArgs(aliases []string, o CredSyncOpts) error {
	if len(aliases) > 0 && o.All {
		return fmt.Errorf("pass either profile aliases or --all, not both")
	}
	if len(aliases) == 0 && !o.All {
		return fmt.Errorf("specify at least one profile alias, or pass --all")
	}
	return nil
}

// refuseSharedIdentity hard-refuses the shared, non-profile "default"/~/.claude
// identity by name — cpm fleet creds only ever touches <profilesBase>/<alias>/.
func refuseSharedIdentity(alias string) error {
	if alias == "default" {
		return fmt.Errorf(`cpm fleet creds never touches the shared "default"/~/.claude identity — only <profilesBase>/<alias>/ profiles are in scope`)
	}
	return nil
}

// resolveAllAliases returns every configured profile alias, sorted, minus the
// shared "default" keyword (never a real fleet-creds target).
func resolveAllAliases(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		if n == "default" {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resolvePeers validates and returns the peer names to operate on: every
// configured peer when wanted is empty, or the (deduplicated, order-preserved)
// subset named by --peer.
func resolvePeers(f *FleetConfig, wanted []string) ([]string, error) {
	if len(wanted) == 0 {
		return sortedPeerNames(f), nil
	}
	seen := make(map[string]bool, len(wanted))
	out := make([]string, 0, len(wanted))
	for _, w := range wanted {
		if _, ok := f.Peers[w]; !ok {
			return nil, fmt.Errorf("unknown peer %q (configured: %s)", w, strings.Join(sortedPeerNames(f), ", "))
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out, nil
}

// isStdinInteractive reports whether stdin is a terminal. Under dispatch/CI
// (non-TTY), push/pull must never silently hang on a confirmation prompt —
// callers require --yes in that case instead.
func isStdinInteractive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// confirmYesNo reads one line from stdin and reports whether it was y/yes.
func confirmYesNo() bool {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// --- remote path composition -------------------------------------------------

// remoteDir returns the parent directory of a peer path, splitting on
// EITHER separator. A local filepath.Dir call must never be used on a peer
// path: on Windows, filepath.Dir mangles a POSIX peer path (and vice versa
// running on a unix control machine against a Windows peer's backslash
// path). This is the one function safe to use for both directions.
func remoteDir(p string) string {
	i := strings.LastIndexAny(p, "/\\")
	if i < 0 {
		return "."
	}
	return p[:i]
}

// remoteProfileDir returns the peer's <profilesBase>/<alias> directory,
// always joined with "/" regardless of the peer's OS — both POSIX shells and
// PowerShell accept forward slashes, so this deletes an entire class of
// separator bug. A leading "~" is left unexpanded, interpreted by the peer's
// own login shell (the property fleet.go's RemoteConfigPath already depends
// on for config.toml).
func remoteProfileDir(peer *FleetPeer, alias string) string {
	return remoteDir(peer.RemoteConfigPath()) + "/" + alias
}

// remoteCredPath returns the peer's <profilesBase>/<alias>/.credentials.json path.
func remoteCredPath(peer *FleetPeer, alias string) string {
	return remoteProfileDir(peer, alias) + "/.credentials.json"
}

// --- transport: remote command construction ---------------------------------

// readRemoteCmd builds the POSIX remote command that reads a peer's
// credentials file, or prints nothing and exits sshExitAbsent if it does not
// exist. path is embedded UNQUOTED after `p=` so a leading "~" tilde-expands
// on the remote shell (quoting it would suppress that — POSIX tilde
// expansion only applies to an unquoted assignment RHS).
func readRemoteCmd(path string) string {
	return fmt.Sprintf(`p=%s; if [ -f "$p" ]; then cat "$p"; else exit %d; fi`, path, sshExitAbsent)
}

// readRemoteCmdWindows builds the PowerShell equivalent for a Windows peer
// (used by pull only — push to a Windows peer is refused entirely).
func readRemoteCmdWindows(path string) string {
	return fmt.Sprintf(
		`powershell -NoProfile -NonInteractive -Command "if(Test-Path -LiteralPath '%s'){Get-Content -Raw -LiteralPath '%s'}else{exit %d}"`,
		path, path, sshExitAbsent)
}

// pushRemoteCmd builds the POSIX remote command that writes stdin to the
// peer's credentials file atomically and 0600-from-birth:
//   - `umask 077` before the redirect means the temp file is created 0600,
//     with no world-readable window (the reason scp — SFTP, default 0644 —
//     is rejected as a transport for this feature).
//   - `mv -f` within the same directory is an atomic same-filesystem rename:
//     a killed transfer leaves only the .cpm-tmp file, never a
//     truncated/corrupted real credentials file.
//
// dir is embedded unquoted after `d=` for the same tilde-expansion reason as
// readRemoteCmd. remoteCmd carries NO secret — the payload travels on stdin.
func pushRemoteCmd(dir string) string {
	return fmt.Sprintf(
		`umask 077; d=%s; [ -d "$d" ] || exit %d; cat > "$d/.credentials.json.cpm-tmp" && chmod 600 "$d/.credentials.json.cpm-tmp" && mv -f "$d/.credentials.json.cpm-tmp" "$d/.credentials.json"`,
		dir, sshExitDirAbsent)
}

// interpretRemoteReadResult turns an sshCapture result into a CredSnapshot
// (or error), centralizing the sentinel-exit-code contract so it is testable
// without a real SSH round trip.
func interpretRemoteReadResult(stdout []byte, stderr string, code int) (CredSnapshot, error) {
	switch {
	case code == 0:
		return parseCredSnapshot(stdout, time.Time{})
	case code == sshExitAbsent:
		return CredSnapshot{Present: false}, nil
	default:
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = fmt.Sprintf("remote command exited %d", code)
		}
		return CredSnapshot{}, fmt.Errorf("%s", msg)
	}
}

// interpretRemoteWriteResult turns an sshPipe result into an error (or nil),
// centralizing the sentinel-exit-code contract for the write path.
func interpretRemoteWriteResult(alias, peerName string, stderr string, code int) error {
	switch {
	case code == 0:
		return nil
	case code == sshExitDirAbsent:
		return fmt.Errorf("profile %q is not installed on peer %q — run 'cpm fleet sync' first", alias, peerName)
	default:
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = fmt.Sprintf("remote command exited %d", code)
		}
		return fmt.Errorf("push to %q failed: %s", peerName, msg)
	}
}

// readRemoteCredSnapshot reads a peer's credentials file over SSH.
func readRemoteCredSnapshot(peer *FleetPeer, alias string, withRaw bool) (CredSnapshot, error) {
	path := remoteCredPath(peer, alias)
	var remoteCmd string
	if peer.OS == OSWindows {
		remoteCmd = readRemoteCmdWindows(path)
	} else {
		remoteCmd = readRemoteCmd(path)
	}
	stdout, stderr, code, err := sshCapture(peer.Host, remoteCmd)
	if err != nil {
		return CredSnapshot{}, fmt.Errorf("ssh to %s: %w", peer.Host, err)
	}
	snap, ierr := interpretRemoteReadResult(stdout, stderr, code)
	if ierr != nil {
		return CredSnapshot{}, fmt.Errorf("peer %s: %w", peer.Host, ierr)
	}
	if withRaw && snap.Present {
		snap.raw = stdout
	}
	return snap, nil
}

// writeRemoteCredFile writes merged credential bytes to a unix/darwin/linux
// peer over an SSH stdin pipe. Refuses outright for a Windows peer — see the
// design note on pushRemoteCmd's package doc and docs/FLEET.md: piping data
// on stdin while also passing a PowerShell script through two shells'
// quoting layers has a silent-corruption failure mode on a credentials file,
// so it is not attempted at all. `pull` is the supported route onto Windows.
func writeRemoteCredFile(peer *FleetPeer, alias string, data []byte) error {
	if peer.OS == OSWindows {
		return fmt.Errorf("pushing to a Windows peer is not supported — run 'cpm fleet creds pull %s --peer <this-machine>' on that machine instead", alias)
	}
	dir := remoteProfileDir(peer, alias)
	remoteCmd := pushRemoteCmd(dir)
	stderr, code, err := sshPipe(peer.Host, remoteCmd, data)
	if err != nil {
		return fmt.Errorf("ssh to %s: %w", peer.Host, err)
	}
	return interpretRemoteWriteResult(alias, "", stderr, code)
}

// --- formatting ---------------------------------------------------------

func fmtRelative(t time.Time) string {
	now := time.Now()
	if t.After(now) {
		return fmt.Sprintf("expires %s  (+%s)", t.Format("2006-01-02 15:04"), fmtDurationShort(t.Sub(now)))
	}
	return fmt.Sprintf("expires %s  (expired %s ago)", t.Format("2006-01-02 15:04"), fmtDurationShort(now.Sub(t)))
}

func fmtDurationShort(d time.Duration) string {
	h := d.Hours()
	if h < 48 {
		return fmt.Sprintf("%.1fh", h)
	}
	return fmt.Sprintf("%dd", int(h/24))
}

func formatSnapshotLine(s CredSnapshot) string {
	if !s.Present {
		return "ABSENT"
	}
	var parts []string
	if !s.ExpiresAt.IsZero() {
		parts = append(parts, fmtRelative(s.ExpiresAt))
	}
	if !s.RefreshExp.IsZero() {
		parts = append(parts, "refresh-exp "+s.RefreshExp.Format("2006-01-02"))
	}
	if s.Fingerprint != "" {
		parts = append(parts, "fp="+s.Fingerprint)
	}
	if len(parts) == 0 {
		return "present (no claudeAiOauth block)"
	}
	return strings.Join(parts, "  ")
}

// --- top-level verbs ------------------------------------------------------

// FleetCredsStatus prints a read-only matrix: local vs every peer, per
// profile. Never writes anything, local or remote.
func FleetCredsStatus(cfg *Config, configPath string, aliases []string, o CredSyncOpts) error {
	f, err := fleetConfigured(cfg)
	if err != nil {
		return err
	}
	peers, err := resolvePeers(f, o.Peers)
	if err != nil {
		return err
	}
	targets := aliases
	if len(targets) == 0 {
		targets = resolveAllAliases(cfg)
	}
	profilesBase := ProfilesBaseDir(configPath)

	reachable := make(map[string]bool, len(peers))
	for _, name := range peers {
		reachable[name] = PeerReachable(f.Peers[name].Host)
	}

	for _, alias := range targets {
		fmt.Println(alias)
		// The alias is embedded unquoted into every remote shell/PowerShell
		// command built below (readRemoteCmd/readRemoteCmdWindows) — reject
		// anything outside the safe charset BEFORE it ever reaches a peer.
		// aliasRe also forbids "." and "/", which closes the local
		// filepath.Join traversal case for free.
		if err := ValidateAlias(alias); err != nil {
			fmt.Printf("  ERROR: %v\n\n", err)
			continue
		}
		profileDir := filepath.Join(profilesBase, alias)
		local, lerr := ReadLocalCredSnapshot(profileDir, false)
		if lerr != nil {
			fmt.Printf("  local    ERROR: %v\n", lerr)
		} else {
			fmt.Printf("  local    %s\n", formatSnapshotLine(local))
		}

		for _, name := range peers {
			if !reachable[name] {
				fmt.Printf("  %-8s UNREACHABLE\n", name)
				continue
			}
			remote, rerr := readRemoteCredSnapshot(f.Peers[name], alias, false)
			if rerr != nil {
				fmt.Printf("  %-8s ERROR: %v\n", name, rerr)
				continue
			}
			fmt.Printf("  %-8s %s\n", name, formatSnapshotLine(remote))
			if lerr == nil && local.Present && remote.Present &&
				local.Fingerprint != "" && remote.Fingerprint != "" &&
				local.Fingerprint != remote.Fingerprint {
				fmt.Printf("  ! DIVERGENT LINEAGE with %s — these are two independent logins.\n", name)
				fmt.Printf("    Whichever refreshes first invalidates the other. Pick a home machine and push.\n")
			}
		}

		if lerr == nil && local.Present && !local.RefreshExp.IsZero() && local.RefreshExp.Before(time.Now()) {
			fmt.Printf("  ! local refresh token EXPIRED at %s — this copy is dead, do not push it\n", local.RefreshExp.Format("2006-01-02 15:04"))
		}
		fmt.Println()
	}
	return nil
}

// FleetCredsPush sends local credentials to peer(s) for the given aliases
// (or every alias when o.All).
func FleetCredsPush(cfg *Config, configPath string, aliases []string, o CredSyncOpts) error {
	f, err := fleetConfigured(cfg)
	if err != nil {
		return err
	}
	if err := validateCredSyncArgs(aliases, o); err != nil {
		return err
	}
	targets := aliases
	if o.All {
		targets = resolveAllAliases(cfg)
	}
	peers, err := resolvePeers(f, o.Peers)
	if err != nil {
		return err
	}
	if !o.DryRun && !o.Yes && !isStdinInteractive() {
		return fmt.Errorf("stdin is not a terminal — pass --yes to proceed non-interactively (dispatch/background)")
	}

	profilesBase := ProfilesBaseDir(configPath)
	pushedAny := false

	for _, alias := range targets {
		// See the matching comment in FleetCredsStatus: alias is embedded
		// unquoted into every remote command built downstream, so it must be
		// validated before ANY use — including before refuseSharedIdentity,
		// which itself only string-compares (an unvalidated alias could
		// otherwise carry an injection payload straight through it).
		if err := ValidateAlias(alias); err != nil {
			fmt.Printf("%s: %v\n", alias, err)
			continue
		}
		if err := refuseSharedIdentity(alias); err != nil {
			fmt.Printf("%s: %v\n", alias, err)
			continue
		}
		profileDir := filepath.Join(profilesBase, alias)
		src, err := ReadLocalCredSnapshot(profileDir, true)
		if err != nil {
			fmt.Printf("%s: %v\n", alias, err)
			continue
		}
		if !src.Present {
			fmt.Printf("%s: no local credentials — skipped\n", alias)
			continue
		}

		for _, name := range peers {
			peer := f.Peers[name]
			fmt.Printf("\n--- %s -> peer %q (%s) ---\n", alias, name, peer.Host)
			if !PeerReachable(peer.Host) {
				fmt.Printf("  UNREACHABLE — skipped\n")
				continue
			}
			if peer.OS == OSWindows {
				fmt.Printf("  pushing to a Windows peer is not supported — run 'cpm fleet creds pull %s --peer <this-machine>' on %s instead\n", alias, name)
				continue
			}

			dst, err := readRemoteCredSnapshot(peer, alias, true)
			if err != nil {
				fmt.Printf("  %v\n", err)
				continue
			}

			decision := planTransfer(src, dst, o.IncludeMCP)
			if !decision.Allowed {
				fmt.Printf("  ! %s\n", decision.Reason)
				if !o.Force {
					fmt.Printf("    → to override anyway: add --force\n")
					fmt.Printf("  SKIPPED\n")
					continue
				}
				fmt.Printf("  --force set — overriding\n")
			}
			if decision.Routine && !o.Yes {
				if o.DryRun {
					fmt.Printf("  DRY RUN: would prompt — %s\n", decision.Reason)
				} else {
					fmt.Printf("  peer %q already has %q (%s). Overwrite? [y/N] ", name, alias, decision.Reason)
					if !confirmYesNo() {
						fmt.Printf("  SKIPPED (not confirmed)\n")
						continue
					}
				}
			}
			if o.DryRun {
				fmt.Printf("  DRY RUN: would push %s -> %s (local fp=%s)\n", alias, name, src.Fingerprint)
				continue
			}

			// Mid-flight rotation guard: re-read the source immediately before
			// sending; if its lineage changed since the plan was computed, the
			// local session refreshed underneath us and the planned bytes are
			// already stale.
			cur, err := ReadLocalCredSnapshot(profileDir, true)
			if err != nil || cur.Fingerprint != src.Fingerprint {
				fmt.Printf("  source credentials rotated mid-operation — re-run\n")
				continue
			}

			merged, err := MergeCredentials(dst.raw, src.raw, o.IncludeMCP)
			if err != nil {
				fmt.Printf("  merge failed: %v\n", err)
				continue
			}
			if err := writeRemoteCredFile(peer, alias, merged); err != nil {
				fmt.Printf("  %v\n", err)
				continue
			}
			pushedAny = true
			fmt.Printf("  pushed (fp=%s)\n", src.Fingerprint)
			if peer.OS == OSDarwin {
				fmt.Printf("  note: peer %q is darwin. cpm writes the profile's .credentials.json only;\n", name)
				fmt.Printf("        it does not touch the macOS Keychain. Profile sessions read the file\n")
				fmt.Printf("        (CLAUDE_CONFIG_DIR), so this is sufficient — but a pre-existing Keychain\n")
				fmt.Printf("        item for this profile is left stale and untouched.\n")
				fmt.Printf("        Verify with:  cpm fleet creds verify %s --peer %s\n", alias, name)
			}
			fmt.Printf("  after this push, restart any running %s session on %s (a live session holds the\n", alias, name)
			fmt.Printf("  old chain in memory and will overwrite this file with its own, now-dead, token on its next refresh)\n")
		}
	}

	if !pushedAny && !o.DryRun {
		fmt.Println("\nno credentials pushed")
	}
	return nil
}

// FleetCredsPull reads credentials from one named peer into this machine.
func FleetCredsPull(cfg *Config, configPath string, aliases []string, o CredSyncOpts) error {
	f, err := fleetConfigured(cfg)
	if err != nil {
		return err
	}
	if err := validateCredSyncArgs(aliases, o); err != nil {
		return err
	}
	if len(o.Peers) > 1 {
		return fmt.Errorf("pull reads from exactly one peer — pass a single --peer")
	}
	var peerName string
	if len(o.Peers) == 1 {
		peerName = o.Peers[0]
	} else if len(f.Peers) == 1 {
		peerName = sortedPeerNames(f)[0]
	} else {
		return fmt.Errorf("--peer is required: more than one peer is configured (%s)", strings.Join(sortedPeerNames(f), ", "))
	}
	peer, ok := f.Peers[peerName]
	if !ok {
		return fmt.Errorf("unknown peer %q (configured: %s)", peerName, strings.Join(sortedPeerNames(f), ", "))
	}
	if !o.DryRun && !o.Yes && !isStdinInteractive() {
		return fmt.Errorf("stdin is not a terminal — pass --yes to proceed non-interactively (dispatch/background)")
	}
	if !PeerReachable(peer.Host) {
		return fmt.Errorf("peer %q (%s) is unreachable", peerName, peer.Host)
	}

	targets := aliases
	if o.All {
		targets = resolveAllAliases(cfg)
	}

	profilesBase := ProfilesBaseDir(configPath)
	pulledAny := false

	for _, alias := range targets {
		// See the matching comment in FleetCredsStatus.
		if err := ValidateAlias(alias); err != nil {
			fmt.Printf("%s: %v\n", alias, err)
			continue
		}
		if err := refuseSharedIdentity(alias); err != nil {
			fmt.Printf("%s: %v\n", alias, err)
			continue
		}
		profileDir := filepath.Join(profilesBase, alias)
		if _, err := os.Stat(profileDir); os.IsNotExist(err) {
			fmt.Printf("%s: not installed locally — run 'cpm fleet sync' first\n", alias)
			continue
		}

		src, err := readRemoteCredSnapshot(peer, alias, true)
		if err != nil {
			fmt.Printf("%s: %v\n", alias, err)
			continue
		}
		if !src.Present {
			fmt.Printf("%s: peer %q has no credentials for this profile — skipped\n", alias, peerName)
			continue
		}

		dst, err := ReadLocalCredSnapshot(profileDir, true)
		if err != nil {
			fmt.Printf("%s: %v\n", alias, err)
			continue
		}

		decision := planTransfer(src, dst, o.IncludeMCP)
		if !decision.Allowed {
			fmt.Printf("%s: ! %s\n", alias, decision.Reason)
			if !o.Force {
				fmt.Printf("  → to override anyway: add --force\n")
				fmt.Printf("  SKIPPED\n")
				continue
			}
			fmt.Printf("  --force set — overriding\n")
		}
		if decision.Routine && !o.Yes {
			if o.DryRun {
				fmt.Printf("%s: DRY RUN: would prompt — %s\n", alias, decision.Reason)
			} else {
				fmt.Printf("%s: local copy already exists (%s). Overwrite? [y/N] ", alias, decision.Reason)
				if !confirmYesNo() {
					fmt.Printf("  SKIPPED (not confirmed)\n")
					continue
				}
			}
		}
		if o.DryRun {
			fmt.Printf("%s: DRY RUN: would pull from %s (peer fp=%s)\n", alias, peerName, src.Fingerprint)
			continue
		}

		merged, err := MergeCredentials(dst.raw, src.raw, o.IncludeMCP)
		if err != nil {
			fmt.Printf("%s: merge failed: %v\n", alias, err)
			continue
		}
		credPath := filepath.Join(profileDir, ".credentials.json")
		if err := writeFileAtomicPattern(credPath, merged, 0o600, ".cpm-cred-*.tmp"); err != nil {
			fmt.Printf("%s: %v\n", alias, err)
			continue
		}
		pulledAny = true
		fmt.Printf("%s: pulled from %s (fp=%s)\n", alias, peerName, src.Fingerprint)
		fmt.Printf("  after this pull, restart any running %s session on this machine (a live session holds the\n", alias)
		fmt.Printf("  old chain in memory and will overwrite this file with its own, now-dead, token on its next refresh)\n")
	}

	if !pulledAny && !o.DryRun {
		fmt.Println("\nno credentials pulled")
	}
	return nil
}

// FleetCredsVerify runs a real headless auth check on a peer: proof that a
// pushed file alone authenticates a session there, with no GUI and no
// keychain unlock available over SSH.
func FleetCredsVerify(cfg *Config, configPath, peerName, alias string) error {
	f, err := fleetConfigured(cfg)
	if err != nil {
		return err
	}
	// alias is concatenated into launcher below and interpolated unquoted
	// into the remote command — validate before ANY use, same as the other
	// three verbs.
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	if err := refuseSharedIdentity(alias); err != nil {
		return err
	}
	peer, ok := f.Peers[peerName]
	if !ok {
		return fmt.Errorf("unknown peer %q (configured: %s)", peerName, strings.Join(sortedPeerNames(f), ", "))
	}
	if !PeerReachable(peer.Host) {
		return fmt.Errorf("peer %q (%s) is unreachable", peerName, peer.Host)
	}

	launcher := "claude-" + alias
	var remoteCmd string
	if peer.OS == OSWindows {
		remoteCmd = fmt.Sprintf(`powershell -NoProfile -NonInteractive -Command "%s -p 'reply with the single word OK' --output-format text"`, launcher)
	} else {
		// PATH prefix is mandatory: `claude`/the launcher is frequently NOT on
		// the non-interactive SSH PATH (only the interactive login shell's
		// profile puts it there) — live-verified on the darwin peer this design
		// was validated against.
		remoteCmd = fmt.Sprintf(`PATH="$HOME/.local/bin:/opt/homebrew/bin:$PATH" %s -p "reply with the single word OK" --output-format text`, launcher)
	}

	stdout, stderr, code, err := sshCapture(peer.Host, remoteCmd)
	if err != nil {
		return fmt.Errorf("ssh to %s: %w", peer.Host, err)
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return fmt.Errorf("verify failed: %s", msg)
	}
	fmt.Printf("peer %q profile %q: %s\n", peerName, alias, strings.TrimSpace(string(stdout)))
	return nil
}
