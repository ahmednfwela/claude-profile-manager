package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

type Attribution struct {
	Commit string `toml:"commit"`
	PR     string `toml:"pr"`
}

type Profile struct {
	Description string            `toml:"description"`
	Email       string            `toml:"email"`
	Model       string            `toml:"model"`
	AddDirs     []string          `toml:"add_dirs"`
	Args        []string          `toml:"args"`
	Env         map[string]string `toml:"env"`
	Attribution *Attribution      `toml:"attribution"`
	// ChannelPort pins this profile's channel endpoint instead of deriving it.
	// Set it when a listening session must survive a roster change (adding a
	// profile that sorts earlier shifts every derived port after it).
	ChannelPort int `toml:"channel_port"`

	// Auth declares this profile's auth material -- the ONLY place a carrier
	// credential enters RenderProfile's merge (design spec §3). Its Env keys
	// are reserved: RenderProfile errors if any of them are also set in Env
	// above (see RenderProfile's auth-key collision check).
	Auth *ProfileAuth `toml:"auth"`

	// ManageMCP overrides the machine-wide Config.ManageMCP mode for this one
	// profile (design spec §2/§11 item 9 -- Config.ManageMCP is config-wide by
	// itself, so a profile needing a different mode than its siblings states
	// it here). nil/absent inherits the machine-wide default -- see
	// EffectiveManageMCPMode. Accepts the same raw shapes as Config.ManageMCP
	// (legacy bool or new mode string); resolved via resolveManageMCPMode.
	ManageMCP any `toml:"manage_mcp"`
}

// ProfileAuth is a profile's declared auth material: which carrier
// (mode = "oauth" | "api_key") and the env vars that carry it. It is kept
// distinct from Profile.Env (the profile's other, non-auth declared delta) so
// RenderProfile can enforce that auth keys never silently collide with an
// ordinary delta key (design spec §3).
type ProfileAuth struct {
	Mode string            `toml:"mode"`
	Env  map[string]string `toml:"env"`
}

// Base is the fleet-wide [base] table (design spec §3): the args/env common
// to every profile on a machine, rendered verbatim from fleet/profiles/
// base.toml by `cpm sync` (see RenderBaseBlock/SpliceBase). Model/Args are
// the fleet-wide defaults a profile may override (Profile.Model) or extend
// (Profile.Args); Env holds BOTH the flat common entries and any per-GOOS
// overlay subtables -- see BaseEnvCommon/BaseEnvOverlay for why they need to
// be read apart rather than declared as two separate TOML keys.
type Base struct {
	Model string   `toml:"model"`
	Args  []string `toml:"args"`
	// Env decodes [base.env]'s table as map[string]any (not map[string]string)
	// because BurntSushi/toml would otherwise reject the [base.env.<goos>]
	// overlay subtables it also lives under as a type mismatch (a subtable
	// value where a plain string is expected). Read it through
	// BaseEnvCommon/BaseEnvOverlay, never directly.
	Env map[string]any `toml:"env"`
}

// BaseEnvCommon returns [base.env]'s flat string entries, excluding any
// nested per-GOOS overlay subtables (see BaseEnvOverlay for those). Safe to
// call on a nil *Base.
func (b *Base) BaseEnvCommon() map[string]string {
	out := map[string]string{}
	if b == nil {
		return out
	}
	for k, v := range b.Env {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// BaseEnvOverlay returns the [base.env.<goos>] overlay table for the given
// GOOS name (e.g. "windows", "darwin", "linux"), or an empty map if this Base
// declares no overlay for it. Safe to call on a nil *Base.
func (b *Base) BaseEnvOverlay(goos string) map[string]string {
	out := map[string]string{}
	if b == nil {
		return out
	}
	raw, ok := b.Env[goos]
	if !ok {
		return out
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

type CloudConfig struct {
	Remote   string   `toml:"remote"`
	AutoPush bool     `toml:"auto_push"`
	Exclude  []string `toml:"exclude"`
}

// FleetPeer describes another machine in the fleet, reachable over SSH.
type FleetPeer struct {
	Host       string `toml:"host"`        // SSH host/alias (resolved via ~/.ssh/config)
	OS         string `toml:"os"`          // windows | darwin | linux (of the peer)
	CPM        string `toml:"cpm"`         // path to the peer's cpm binary (default: "cpm" on PATH)
	ConfigPath string `toml:"config_path"` // peer's config.toml (default: ~/.claude-profiles/config.toml)
}

// FleetConfig declares this machine's identity and its peer machines, so cpm can
// reconcile the profile set across the fleet while respecting each machine's OS.
type FleetConfig struct {
	ID              string                `toml:"id"`               // this machine's name (informational)
	OS              string                `toml:"os"`               // this machine's OS (informational; runtime.GOOS wins)
	DefaultTemplate string                `toml:"default_template"` // profile to clone args/env from when adding accounts
	Peers           map[string]*FleetPeer `toml:"peers"`
	// RepoPath is the local checkout `cpm sync` reads fleet/profiles/base.toml
	// (and, for manage_mcp="fleet-render" profiles, the staged
	// rendered/<device>/<profile>/mcpServers.patch.json patches) from --
	// design spec §3/§6: the MARKETPLACE-INSTALLED plugin-cache checkout, not
	// a git-submodule dev clone (the two can sit at different commits
	// invisibly). A per-machine local path, never git-tracked content.
	RepoPath string `toml:"repo_path"`
}

// RemoteConfigPath returns the peer's config.toml path, defaulting to the
// conventional location when unset.
func (p *FleetPeer) RemoteConfigPath() string {
	if p.ConfigPath != "" {
		return p.ConfigPath
	}
	return "~/.claude-profiles/config.toml"
}

// CPMBinary returns the peer's cpm invocation, defaulting to PATH resolution.
func (p *FleetPeer) CPMBinary() string {
	if p.CPM != "" {
		return p.CPM
	}
	return "cpm"
}

type Config struct {
	SourceDir string              `toml:"source_dir"`
	BinDir    string              `toml:"bin_dir"`
	// ManageMCP is deliberately typed `any`, not ManageMCPMode: TOML may spell
	// it as the new mode string ("fleet-render" | "gateway-owns") or -- for
	// every config.toml written before this change -- a bare legacy boolean.
	// Never compare this field directly; resolve it via ManageMCPMode() or
	// EffectiveManageMCPMode() (see resolveManageMCPMode for the exact
	// mapping and its rationale).
	ManageMCP any                 `toml:"manage_mcp"`
	Profiles  map[string]*Profile `toml:"profiles"`
	Cloud     *CloudConfig        `toml:"cloud"`
	Fleet     *FleetConfig        `toml:"fleet"`
	// ChannelBasePort is the first port of the per-profile channel range
	// (default DefaultChannelBasePort). Ports are derived from it, never stored.
	ChannelBasePort int `toml:"channel_base_port"`

	// Base is the fleet-wide [base] table (design spec §3), CPM-MANAGED via
	// the sentinel-comment splice in basesync.go -- see RenderBaseBlock,
	// ExtractBase, SpliceBase. nil when config.toml declares no [base] yet
	// (a config.toml predating this feature, or one whose fleet repo has
	// never been synced).
	Base *Base `toml:"base"`
}

// ManageMCPMode is manage_mcp's resolved, forward-looking value. See
// resolveManageMCPMode for how a raw TOML value (absent / legacy bool / new
// string) maps onto one of these.
type ManageMCPMode string

const (
	// ManageMCPFleetRender is the new mode (design spec §2): cpm applies a
	// pre-rendered mcpServers patch, staged by the fleet repo, as an
	// additive-only merge -- see MergeMCPServersPatch. Never auto-selected by
	// the legacy-boolean compat shim (see ManageMCPLegacyMirror) -- it is
	// opt-in only, by writing this exact string in config.toml, until the
	// owner resolves OQ-7 (design spec §10 item 7).
	ManageMCPFleetRender ManageMCPMode = "fleet-render"
	// ManageMCPGatewayOwns preserves today's `manage_mcp = false` behavior:
	// cpm never touches mcpServers except cpm-channel's own self-registration
	// (ChannelInstall, ungated by this flag either way -- design spec OQ-3).
	ManageMCPGatewayOwns ManageMCPMode = "gateway-owns"
	// ManageMCPLegacyMirror is the resolved mode for a config.toml that
	// leaves manage_mcp unset, or spells it as the bare legacy TOML boolean
	// `true` -- cpm's original, wholesale mirror-from-~/.claude.json
	// behavior (SyncMCPServers). Deliberately kept distinct from
	// ManageMCPFleetRender: they are different mechanisms (verbatim full-map
	// replace vs. an additive patch merge sourced from fleet-repo staging
	// that may not exist yet on a given machine), and the design spec's OQ-7
	// explicitly leaves the legacy-bool mapping to the owner ("do not
	// guess") -- so upgrading the cpm binary alone, with no config.toml edit,
	// must never silently switch a profile onto fleet-render.
	ManageMCPLegacyMirror ManageMCPMode = "legacy-mirror"
)

// resolveManageMCPMode maps a raw manage_mcp value -- as decoded from TOML,
// so nil (key absent), bool (legacy), or string (new mode) -- onto one of the
// ManageMCPMode constants above. legacyBool reports whether raw was a bare
// TOML boolean (nil counts as legacy too, since it reproduces the pre-schema
// default), which callers can use to surface a deprecation notice (e.g. `cpm
// doctor`) without this function itself producing I/O side effects.
func resolveManageMCPMode(raw any) (mode ManageMCPMode, legacyBool bool, err error) {
	switch v := raw.(type) {
	case nil:
		return ManageMCPLegacyMirror, true, nil
	case bool:
		if v {
			return ManageMCPLegacyMirror, true, nil
		}
		return ManageMCPGatewayOwns, true, nil
	case string:
		switch ManageMCPMode(v) {
		case ManageMCPFleetRender, ManageMCPGatewayOwns, ManageMCPLegacyMirror:
			return ManageMCPMode(v), false, nil
		default:
			return "", false, fmt.Errorf("manage_mcp: unknown mode %q (want %q or %q)", v, ManageMCPFleetRender, ManageMCPGatewayOwns)
		}
	default:
		return "", false, fmt.Errorf("manage_mcp: unsupported value %#v (%T)", raw, raw)
	}
}

// ManageMCPMode resolves Config.ManageMCP's raw value into its effective
// mode. Most callers targeting a specific profile should prefer
// EffectiveManageMCPMode, which also applies that profile's own override.
func (c *Config) ManageMCPMode() (ManageMCPMode, error) {
	mode, _, err := resolveManageMCPMode(c.ManageMCP)
	return mode, err
}

// EffectiveManageMCPMode resolves the given profile's effective manage_mcp
// mode: its own [profiles.X] manage_mcp override if set, else the
// machine-wide Config default (design spec §2/§11 item 9).
func (c *Config) EffectiveManageMCPMode(name string) (ManageMCPMode, error) {
	p, ok := c.Profiles[name]
	if !ok {
		return "", fmt.Errorf("unknown profile %q", name)
	}
	if p != nil && p.ManageMCP != nil {
		mode, _, err := resolveManageMCPMode(p.ManageMCP)
		return mode, err
	}
	return c.ManageMCPMode()
}

// ManageMCPEnabled reports whether cpm should sync MCP servers into profiles
// at all -- true for every mode except ManageMCPGatewayOwns. Preserved for
// the existing `cpm add` call site (AddProfile), which still only needs a
// yes/no answer for whether to run the legacy SyncMCPServers mirror; a
// resolution error (a malformed manage_mcp value) fails open (true) rather
// than silently disabling MCP management out from under an existing profile.
func (c *Config) ManageMCPEnabled() bool {
	mode, err := c.ManageMCPMode()
	if err != nil {
		return true
	}
	return mode != ManageMCPGatewayOwns
}

// IsMaxProfile reports whether a profile is a plain OAuth/subscription ("Max")
// profile — i.e. it does NOT set a custom base URL or a static auth token. These
// are the only profiles eligible to be an add-account template (never glm-class).
func (p *Profile) IsMaxProfile() bool {
	if p.Env == nil {
		return true
	}
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if _, ok := p.Env[k]; ok {
			return false
		}
	}
	return true
}

// TemplateProfileName resolves which profile to copy args/env from for a new
// account. Precedence: explicit override → fleet.default_template → the
// lexically-first Max profile. Returns an error if none is usable.
func (c *Config) TemplateProfileName(explicit string) (string, error) {
	if explicit != "" {
		p, ok := c.Profiles[explicit]
		if !ok {
			return "", fmt.Errorf("template profile %q not found", explicit)
		}
		if !p.IsMaxProfile() {
			return "", fmt.Errorf("template profile %q sets a custom base URL / token and is not a valid Max template", explicit)
		}
		return explicit, nil
	}
	if c.Fleet != nil && c.Fleet.DefaultTemplate != "" {
		name := c.Fleet.DefaultTemplate
		p, ok := c.Profiles[name]
		if !ok {
			return "", fmt.Errorf("fleet.default_template %q not found in profiles", name)
		}
		if !p.IsMaxProfile() {
			return "", fmt.Errorf("fleet.default_template %q is not a valid Max template", name)
		}
		return name, nil
	}
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if c.Profiles[n].IsMaxProfile() {
			return n, nil
		}
	}
	return "", fmt.Errorf("no Max profile found to use as a template (add one, or pass --from)")
}

func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude-profiles", "config.toml")
}

// defaultBinDir is the default install location for the generated launchers.
// ~/.local/bin is already on PATH on the target machines (Unix, and the Windows
// host this port targets), so it is kept as the single cross-platform default.
// If it is not on PATH, `cpm doctor` warns (it never mutates PATH automatically).
func defaultBinDir() string {
	return "~/.local/bin"
}

func LoadConfig(path string) (*Config, error) {
	path = ExpandPath(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read config: %w", err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("cannot parse config: %w", err)
	}

	if cfg.SourceDir == "" {
		cfg.SourceDir = "~/.claude"
	}
	if cfg.BinDir == "" {
		cfg.BinDir = defaultBinDir()
	}
	cfg.SourceDir = ExpandPath(cfg.SourceDir)
	cfg.BinDir = ExpandPath(cfg.BinDir)

	if len(cfg.Profiles) == 0 {
		return nil, fmt.Errorf("no profiles defined in %s", path)
	}

	return &cfg, nil
}

func ExpandPath(p string) string {
	if len(p) == 0 {
		return p
	}
	if p[0] == '~' {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// LoadCloudConfig loads config without requiring profiles to be defined.
func LoadCloudConfig(path string) (*Config, error) {
	path = ExpandPath(path)

	data, err := os.ReadFile(path)
	if err != nil {
		// Return a default config if no config file exists
		return &Config{
			SourceDir: ExpandPath("~/.claude"),
			BinDir:    ExpandPath(defaultBinDir()),
		}, nil
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("cannot parse config: %w", err)
	}

	if cfg.SourceDir == "" {
		cfg.SourceDir = "~/.claude"
	}
	if cfg.BinDir == "" {
		cfg.BinDir = defaultBinDir()
	}
	cfg.SourceDir = ExpandPath(cfg.SourceDir)
	cfg.BinDir = ExpandPath(cfg.BinDir)

	return &cfg, nil
}

func ProfilesBaseDir(configPath string) string {
	return filepath.Dir(ExpandPath(configPath))
}
