package internal

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jakubkontra/cpm/channel"
)

// DefaultChannelBasePort is the first port of the per-profile channel range. A
// Claude Code "channel" is an MCP server the session spawns, which listens on a
// local port; a message to a session is an HTTP POST to that port. Each profile
// therefore needs ONE stable, collision-free port that both sender and receiver
// can derive from the same config without storing it anywhere.
const DefaultChannelBasePort = 8790

// ChannelServerName is the MCP-server key the channel is registered under in a
// profile's config. Stable, because the session's `--dangerously-load-development-
// channels server:<name>` flag must name the same key.
const ChannelServerName = "cpm-channel"

// ChannelInstall wires a profile's sessions to their inbox: it registers the channel
// as a `type:"http"` MCP server at the profile's derived port.
//
// HTTP, not stdio, is load-bearing. A stdio channel is spawned as a SUBPROCESS of one
// session, so it is 1:1 with that session and dies with it. An HTTP channel is an
// independent server, so ONE per profile serves EVERY session of that profile and a
// push fans out to all of them (proven 2026-07-24). The entry MUST point directly at
// loopback and MUST NOT be routed through an MCP proxy: a proxied channel has its
// capability stripped and every push silently discarded.
//
// The write is surgical -- existing mcpServers and unrelated top-level keys are
// preserved, because this file is the profile's whole session config.
func ChannelInstall(cfg *Config, profilesBase, alias string) error {
	port, err := ChannelPort(cfg, alias)
	if err != nil {
		return err
	}
	dir := filepath.Join(profilesBase, alias)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}
	cfgPath := filepath.Join(dir, ".claude.json")

	doc := map[string]any{}
	if raw, err := os.ReadFile(cfgPath); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			// Refuse rather than overwrite: this file holds the profile's whole
			// session config, so clobbering it on a parse error loses real state.
			return fmt.Errorf("%s is not valid JSON (refusing to overwrite): %w", cfgPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", cfgPath, err)
	}

	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[ChannelServerName] = map[string]any{
		"type": "http",
		"url":  fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
	}
	doc["mcpServers"] = servers

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return os.WriteFile(cfgPath, append(out, '\n'), 0o644)
}

// ChannelState is one profile's channel endpoint and whether anything is bound.
type ChannelState struct {
	Alias     string
	Port      int
	Listening bool
}

// channelBasePort resolves the configured base, falling back to the default.
func channelBasePort(cfg *Config) int {
	if cfg != nil && cfg.ChannelBasePort > 0 {
		return cfg.ChannelBasePort
	}
	return DefaultChannelBasePort
}

// sortedAliases returns the profile aliases in a stable order. Derivation keys on
// this order, so the same config always yields the same ports.
func sortedAliases(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Profiles))
	for a := range cfg.Profiles {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// explicitPorts collects the ports profiles pinned by hand, so a derived port
// never lands on one.
func explicitPorts(cfg *Config) map[int]bool {
	pinned := map[int]bool{}
	for _, p := range cfg.Profiles {
		if p != nil && p.ChannelPort > 0 {
			pinned[p.ChannelPort] = true
		}
	}
	return pinned
}

// ChannelPort returns the channel endpoint port for a profile.
//
// An explicit `channel_port` on the profile always wins — that is the escape
// hatch for a session already running on a known port. Otherwise the port is
// DERIVED from the alias's position in the sorted alias list, so it is stable
// across runs and identical for every process that reads this config; a derived
// port that would land on a hand-pinned one steps forward until it is free.
//
// Caveat worth knowing: adding a profile that sorts BEFORE existing ones shifts
// their derived ports. That is fine for a fresh session (both ends re-derive)
// but not for one already listening — pin `channel_port` on any profile whose
// endpoint must survive a roster change.
func ChannelPort(cfg *Config, alias string) (int, error) {
	if cfg == nil || len(cfg.Profiles) == 0 {
		return 0, fmt.Errorf("no profiles configured")
	}
	prof, ok := cfg.Profiles[alias]
	if !ok {
		// Loud, actionable: a silent default would POST into another profile's session.
		return 0, fmt.Errorf("unknown profile %q — known profiles: %s",
			alias, strings.Join(sortedAliases(cfg), ", "))
	}
	if prof != nil && prof.ChannelPort > 0 {
		return prof.ChannelPort, nil
	}
	port, ok := channelAllocation(cfg)[alias]
	if !ok {
		return 0, fmt.Errorf("unknown profile %q", alias)
	}
	return port, nil
}

// channelAllocation assigns every profile its endpoint in one pass.
//
// Computing a port from an alias's INDEX alone is unsafe: stepping over a pinned
// port pushes that alias onto the NEXT alias's natural slot, and the next alias
// does not step, so two profiles collide. A review sweep found 40 of 90 realistic
// rosters colliding that way (44%) -- and the collision is persistent, because
// ChannelInstall writes the same URL into both profiles.
//
// So allocation is whole-roster: pinned ports are claimed first, then each
// remaining alias in sorted order takes the next port that is neither pinned nor
// already handed out. Deterministic (sorted input, no map iteration) and
// collision-free by construction.
func channelAllocation(cfg *Config) map[string]int {
	alloc := make(map[string]int, len(cfg.Profiles))
	taken := map[int]bool{}
	aliases := sortedAliases(cfg)

	for _, a := range aliases {
		if p := cfg.Profiles[a]; p != nil && p.ChannelPort > 0 {
			alloc[a] = p.ChannelPort
			taken[p.ChannelPort] = true
		}
	}
	next := channelBasePort(cfg)
	for _, a := range aliases {
		if _, done := alloc[a]; done {
			continue
		}
		for taken[next] {
			next++
		}
		alloc[a] = next
		taken[next] = true
	}
	return alloc
}

// SendToProfile POSTs a message to a profile's channel endpoint — the wire form
// of "wake session X and give it this instruction". A non-2xx or a refused
// connection is an ERROR: reporting success when the message never reached the
// session would silently drop fleet work.
func SendToProfile(cfg *Config, alias, message string) error {
	port, err := ChannelPort(cfg, alias)
	if err != nil {
		return err
	}
	// /push is the channel server's trigger endpoint; /mcp is the protocol endpoint
	// Claude Code itself connects to. Posting to / reaches neither and would be a
	// silent no-op, so the path is part of the contract, not a detail.
	url := fmt.Sprintf("http://127.0.0.1:%d/push", port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "text/plain", strings.NewReader(message))
	if err != nil {
		return fmt.Errorf("channel %s (%s) unreachable: %w — is a session running with its channel loaded?", alias, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("channel %s (%s) returned %s", alias, url, resp.Status)
	}
	// A 200 only proves the SERVER accepted the push. The server answers 200 with
	// sessionCount 0 when no session is connected -- the message reached nobody.
	// Reporting success there is the decoupled-signal failure: the caller believes
	// work was dispatched that no one will ever do.
	var pushed struct {
		SessionCount *int `json:"sessionCount"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err == nil && json.Unmarshal(body, &pushed) == nil && pushed.SessionCount != nil && *pushed.SessionCount == 0 {
		return fmt.Errorf("channel %s accepted the push but no session received it — "+
			"is a session running with `--dangerously-load-development-channels server:%s`?",
			alias, ChannelServerName)
	}
	return nil
}

// portListening reports whether anything is bound on the local port. Used instead
// of a POST so status never injects a message as a side effect.
func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ChannelStatus reports every profile's endpoint and whether it is bound, sorted
// by alias so repeated runs diff cleanly. A port that is Listening with no session
// running is the orphaned-subprocess case: killing a session can leave its channel
// holding the port.
func ChannelStatus(cfg *Config) []ChannelState {
	aliases := sortedAliases(cfg)
	out := make([]ChannelState, 0, len(aliases))
	for _, a := range aliases {
		port, err := ChannelPort(cfg, a)
		if err != nil {
			continue
		}
		out = append(out, ChannelState{Alias: a, Port: port, Listening: portListening(port)})
	}
	return out
}

// ChannelServerScript locates the channel server, preferring the SELF-CONTAINED
// bundle (httpchan.bundle.mjs, SDK inlined by `make channel-bundle`). The bundle is
// what ships in a release, because the release archive carries no node_modules -- an
// un-bundled httpchan.mjs there dies with ERR_MODULE_NOT_FOUND. The plain source is
// accepted too, for a dev checkout that has run `npm install` in channel/.
func ChannelServerScript() (string, error) {
	// Bundle first at every root: a root that has both must use the one with no
	// runtime dependency.
	roots := []string{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		roots = append(roots, dir, filepath.Join(dir, ".."))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	candidates := []string{}
	for _, name := range []string{"httpchan.bundle.mjs", "httpchan.mjs"} {
		for _, r := range roots {
			candidates = append(candidates, filepath.Join(r, "channel", name))
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	// No on-disk copy — the NORMAL case for a released binary: goreleaser ships
	// bare binaries (its format:binary ignores archive `files:`, proven on the
	// v0.5.0 release), so channel/ never exists next to the exe. Extract the
	// embedded bundle to a version-scoped cache path; version-scoping makes an
	// upgrade self-invalidating with no staleness comparison.
	if p, err := extractEmbeddedBundle(); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("channel server not found; looked in: %s (and embedded-bundle extraction failed) | build it with: "+
		"cd channel && npm install && bun build httpchan.mjs --target=node --outfile=httpchan.bundle.mjs",
		strings.Join(candidates, ", "))
}

// extractEmbeddedBundle writes the go:embed-ded channel server bundle to a
// version-scoped per-user cache path and returns it. Concurrent extraction is
// race-safe: each writer uses a unique temp file, and a losing rename defers to
// the winner's already-present file.
func extractEmbeddedBundle() (string, error) {
	if len(channel.BundleMJS) == 0 {
		return "", fmt.Errorf("embedded channel bundle is empty")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(cacheDir, "cpm", "channel", Version, "httpchan.bundle.mjs")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "httpchan-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(channel.BundleMJS); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		// A concurrent extraction may have won the rename (Windows refuses to
		// rename over an existing file) — the winner's copy is byte-identical.
		if _, serr := os.Stat(dst); serr == nil {
			return dst, nil
		}
		return "", err
	}
	return dst, nil
}
