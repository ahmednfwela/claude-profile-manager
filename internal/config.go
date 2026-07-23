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
	ManageMCP *bool               `toml:"manage_mcp"` // default true; false => cpm never touches .claude.json mcpServers (gateway owns MCP)
	Profiles  map[string]*Profile `toml:"profiles"`
	Cloud     *CloudConfig        `toml:"cloud"`
	Fleet     *FleetConfig        `toml:"fleet"`
}

// ManageMCPEnabled reports whether cpm should sync MCP servers into profiles.
// Defaults to true; set `manage_mcp = false` when an external MCP proxy/gateway
// is the sole owner of every profile's .claude.json mcpServers.
func (c *Config) ManageMCPEnabled() bool {
	return c.ManageMCP == nil || *c.ManageMCP
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
