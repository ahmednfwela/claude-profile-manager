package internal

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// sshBaseOpts are applied to every ssh invocation: fail fast, never prompt.
var sshBaseOpts = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
}

// sshRun runs `ssh <host> <remoteCmd>` and returns combined output. remoteCmd is
// passed as a SINGLE argument so the remote login shell interprets it (expanding
// ~ etc.) and so local path-mangling layers (MSYS/Git-Bash) never rewrite a bare
// leading-path token. Callers must only build remoteCmd from validated/trusted
// values (safe alias/email + config-controlled paths) — never secrets.
func sshRun(host, remoteCmd string) (string, error) {
	args := append(append([]string{}, sshBaseOpts...), host, remoteCmd)
	cmd := exec.Command("ssh", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// PeerReachable reports whether the peer answers a trivial SSH command.
func PeerReachable(host string) bool {
	_, err := sshRun(host, "true")
	return err == nil
}

// profileEmail returns a profile's account email: the explicit `email` field if
// set, otherwise the address parsed from its description.
func profileEmail(p *Profile) string {
	if p.Email != "" {
		return p.Email
	}
	return emailRe.FindString(p.Description)
}

// localProfileEmails maps each local profile alias to its account email.
func localProfileEmails(cfg *Config) map[string]string {
	m := make(map[string]string, len(cfg.Profiles))
	for name, p := range cfg.Profiles {
		m[name] = profileEmail(p)
	}
	return m
}

// remotePeerProfiles reads a peer's config.toml over SSH and returns
// alias -> email for every profile it defines.
func remotePeerProfiles(peer *FleetPeer) (map[string]string, error) {
	out, err := sshRun(peer.Host, "cat "+peer.RemoteConfigPath())
	if err != nil {
		return nil, fmt.Errorf("read remote config: %s", strings.TrimSpace(out))
	}
	var cfg Config
	if err := toml.Unmarshal([]byte(out), &cfg); err != nil {
		return nil, fmt.Errorf("parse remote config: %w", err)
	}
	m := make(map[string]string, len(cfg.Profiles))
	for name, p := range cfg.Profiles {
		m[name] = profileEmail(p)
	}
	return m, nil
}

// fleetConfigured returns the fleet config or an actionable error.
func fleetConfigured(cfg *Config) (*FleetConfig, error) {
	if cfg.Fleet == nil || len(cfg.Fleet.Peers) == 0 {
		return nil, fmt.Errorf("no fleet peers configured — add a [fleet.peers.<name>] block with host/os/cpm to config.toml")
	}
	return cfg.Fleet, nil
}

func sortedPeerNames(f *FleetConfig) []string {
	names := make([]string, 0, len(f.Peers))
	for n := range f.Peers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FleetStatus prints this machine's identity, its profiles, and for each peer:
// reachability plus a profile-set diff (which aliases are missing where).
func FleetStatus(cfg *Config, configPath string) error {
	f, err := fleetConfigured(cfg)
	if err != nil {
		return err
	}

	id := "(unset)"
	if f.ID != "" {
		id = f.ID
	}
	fmt.Printf("This machine: %s  [os=%s]\n", id, CurrentOS())
	local := localProfileEmails(cfg)
	fmt.Printf("Local profiles (%d): %s\n\n", len(local), strings.Join(sortedStringKeys(local), ", "))

	for _, name := range sortedPeerNames(f) {
		peer := f.Peers[name]
		fmt.Printf("Peer %q  host=%s os=%s\n", name, peer.Host, peer.OS)
		if !PeerReachable(peer.Host) {
			fmt.Printf("  UNREACHABLE (ssh %s failed)\n\n", peer.Host)
			continue
		}
		remote, err := remotePeerProfiles(peer)
		if err != nil {
			fmt.Printf("  reachable, but %v\n\n", err)
			continue
		}
		var missingThere, missingHere []string
		for a := range local {
			if _, ok := remote[a]; !ok {
				missingThere = append(missingThere, a)
			}
		}
		for a := range remote {
			if _, ok := local[a]; !ok {
				missingHere = append(missingHere, a)
			}
		}
		sort.Strings(missingThere)
		sort.Strings(missingHere)
		fmt.Printf("  reachable; profiles (%d): %s\n", len(remote), strings.Join(sortedStringKeys(remote), ", "))
		if len(missingThere) == 0 && len(missingHere) == 0 {
			fmt.Printf("  in sync\n\n")
			continue
		}
		if len(missingThere) > 0 {
			fmt.Printf("  missing on peer:  %s\n", strings.Join(missingThere, ", "))
		}
		if len(missingHere) > 0 {
			fmt.Printf("  missing locally:  %s\n", strings.Join(missingHere, ", "))
		}
		fmt.Println()
	}
	return nil
}

// remoteAddProfile runs `cpm add <email> <alias>` on a peer over SSH. The peer's
// own cpm materializes the profile from the peer's template, so the result is
// correct for the peer's OS without any cross-OS path translation here.
func remoteAddProfile(peer *FleetPeer, email, alias string) (string, error) {
	// email + alias are validated shell-safe; CPMBinary is config-controlled.
	return sshRun(peer.Host, fmt.Sprintf("%s add %s %s", peer.CPMBinary(), email, alias))
}

// AddProfileToFleet adds the account locally, then propagates it to every
// reachable peer by invoking the peer's own `cpm add` over SSH. It prints a
// login matrix at the end (credentials are never transported).
func AddProfileToFleet(cfg *Config, configPath, email, alias, fromProfile string, loginNow bool) error {
	f, err := fleetConfigured(cfg)
	if err != nil {
		return err
	}
	if err := AddProfile(cfg, configPath, email, alias, fromProfile, loginNow); err != nil {
		return fmt.Errorf("local add: %w", err)
	}

	type peerResult struct {
		name string
		ok   bool
		msg  string
	}
	var results []peerResult
	for _, name := range sortedPeerNames(f) {
		peer := f.Peers[name]
		fmt.Printf("\n--- peer %q (%s) ---\n", name, peer.Host)
		if !PeerReachable(peer.Host) {
			results = append(results, peerResult{name, false, "unreachable"})
			fmt.Printf("  UNREACHABLE — skipped\n")
			continue
		}
		out, err := remoteAddProfile(peer, email, alias)
		if err != nil {
			results = append(results, peerResult{name, false, strings.TrimSpace(out)})
			fmt.Printf("  FAILED: %s\n", strings.TrimSpace(out))
			continue
		}
		results = append(results, peerResult{name, true, ""})
		fmt.Print(indent(out, "  "))
	}

	fmt.Printf("\n=== login matrix for %s (%s) ===\n", alias, email)
	fmt.Printf("  local:  claude-%s   # /login as %s\n", alias, email)
	for _, r := range results {
		peer := f.Peers[r.name]
		if r.ok {
			fmt.Printf("  %-8s ssh %s -t %s   then /login as %s\n", r.name+":", peer.Host, launcherHint(peer, alias), email)
		} else {
			fmt.Printf("  %-8s NOT ADDED (%s)\n", r.name+":", r.msg)
		}
	}
	return nil
}

// FleetSync reconciles the union of profile aliases across the local machine and
// every reachable peer: any alias present somewhere but missing on a machine is
// added there (locally via AddProfile, remotely via the peer's `cpm add`).
func FleetSync(cfg *Config, configPath string) error {
	f, err := fleetConfigured(cfg)
	if err != nil {
		return err
	}

	// Build the union alias -> email from local + reachable peers.
	union := map[string]string{}
	for a, e := range localProfileEmails(cfg) {
		if e != "" {
			union[a] = e
		}
	}
	peerProfiles := map[string]map[string]string{}
	for _, name := range sortedPeerNames(f) {
		peer := f.Peers[name]
		if !PeerReachable(peer.Host) {
			fmt.Printf("peer %q unreachable — skipping\n", name)
			continue
		}
		rp, err := remotePeerProfiles(peer)
		if err != nil {
			fmt.Printf("peer %q: %v — skipping\n", name, err)
			continue
		}
		peerProfiles[name] = rp
		for a, e := range rp {
			if _, ok := union[a]; !ok && e != "" {
				union[a] = e
			}
		}
	}

	if len(union) == 0 {
		fmt.Println("no accounts with resolvable emails found — nothing to sync")
		return nil
	}

	changed := false
	// Local: add any union alias missing locally.
	for _, alias := range sortedStringKeys(union) {
		if _, ok := cfg.Profiles[alias]; ok {
			continue
		}
		fmt.Printf("\n[local] adding missing profile %q (%s)\n", alias, union[alias])
		if err := AddProfile(cfg, configPath, union[alias], alias, "", false); err != nil {
			fmt.Printf("  FAILED: %v\n", err)
		} else {
			changed = true
		}
	}

	// Peers: add any union alias missing on that peer.
	for _, name := range sortedPeerNames(f) {
		rp, ok := peerProfiles[name]
		if !ok {
			continue // was unreachable / unreadable
		}
		peer := f.Peers[name]
		for _, alias := range sortedStringKeys(union) {
			if _, has := rp[alias]; has {
				continue
			}
			fmt.Printf("\n[%s] adding missing profile %q (%s)\n", name, alias, union[alias])
			out, err := remoteAddProfile(peer, union[alias], alias)
			if err != nil {
				fmt.Printf("  FAILED: %s\n", strings.TrimSpace(out))
			} else {
				fmt.Print(indent(out, "  "))
				changed = true
			}
		}
	}

	if !changed {
		fmt.Println("\nFleet already in sync — no profiles added.")
	} else {
		fmt.Println("\nFleet reconciled. Each newly-added profile still needs its own /login (credentials are never synced).")
	}
	return nil
}

// launcherHint returns the launcher command name for a peer's OS.
func launcherHint(peer *FleetPeer, alias string) string {
	if peer.OS == OSWindows {
		return "claude-" + alias
	}
	return "claude-" + alias
}

// indent prefixes every non-empty line of s with prefix.
func indent(s, prefix string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}
