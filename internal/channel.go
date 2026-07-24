package internal

import (
	"fmt"
	"net"
	"net/http"
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
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "text/plain", strings.NewReader(message))
	if err != nil {
		return fmt.Errorf("channel %s (%s) unreachable: %w — is a session running with its channel loaded?", alias, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("channel %s (%s) returned %s", alias, url, resp.Status)
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
