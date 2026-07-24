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
	base := channelBasePort(cfg)
	pinned := explicitPorts(cfg)
	for i, a := range sortedAliases(cfg) {
		if a != alias {
			continue
		}
		port := base + i
		for pinned[port] {
			port++
		}
		return port, nil
	}
	return 0, fmt.Errorf("unknown profile %q", alias)
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

// ChannelServerScript locates the bundled channel server. It sits next to the cpm
// binary in a release, and next to the source tree in development, so both work
// without configuration.
func ChannelServerScript() (string, error) {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "channel", "httpchan.mjs"),
			filepath.Join(dir, "..", "channel", "httpchan.mjs"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "channel", "httpchan.mjs"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("channel server not found; looked in: %s", strings.Join(candidates, ", "))
}
