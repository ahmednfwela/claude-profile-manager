package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// LoadFleetBase reads fleet/profiles/base.toml from a fleet-repo checkout at
// repoPath and decodes it into a Base -- the "single generator for [base]"
// named by the design spec's work item (B). The file's own top-level shape
// mirrors config.toml's [base] table WITHOUT the "base" wrapper key (its
// entire content already IS the base declaration): plain `model`/`args` at
// the top level, plus `[env]`/`[env.<goos>]` tables (not `[base.env]` --
// there is no surrounding [base] table in this standalone file).
func LoadFleetBase(repoPath string) (*Base, error) {
	path := filepath.Join(ExpandPath(repoPath), "profiles", "base.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	var base Base
	if err := toml.Unmarshal(data, &base); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", path, err)
	}
	return &base, nil
}

// FleetDeviceID resolves "this machine's" identity for fleet-repo staging
// path lookups (FleetRenderedDir): cfg.Fleet.ID when set (FleetConfig's own
// doc comment calls it "this machine's name"), else os.Hostname().
func FleetDeviceID(cfg *Config) string {
	if cfg != nil && cfg.Fleet != nil && cfg.Fleet.ID != "" {
		return cfg.Fleet.ID
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown-device"
}

// FleetRenderedDir resolves the staging directory a fleet-repo checkout at
// repoPath writes cpm-consumable render output to, for one device+profile
// pair (design spec §5: "fleet/rendered/<device>/<profile>/{settings.json,
// mcpServers.patch.json}, git-ignored build output"). This exact path
// convention is coordinated with work items (B) (shared/claude-plugins) and
// (C) (shared/misc/mcp-gateway) -- see design spec §7C ("agree on one
// constant, don't let each side invent its own"); this Go-side helper is the
// single place cpm derives it, so a future rename only touches one line.
func FleetRenderedDir(repoPath, device, profile string) string {
	return filepath.Join(ExpandPath(repoPath), "rendered", device, profile)
}

// LoadMCPServersPatch reads <renderedDir>/mcpServers.patch.json -- the
// staged, fleet-computed additive-merge patch a manage_mcp="fleet-render"
// profile applies via MergeMCPServersPatch. A missing file returns a nil
// patch and a nil error: a profile with nothing staged to merge is not a
// failure, it just has no fleet-render work to do this sync.
func LoadMCPServersPatch(renderedDir string) (map[string]any, error) {
	path := filepath.Join(renderedDir, "mcpServers.patch.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return patch, nil
}
