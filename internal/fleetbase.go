package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// FleetBase is fleet/profiles/base.toml as cpm consumes it: the decoded
// [base] table (what RenderProfile merges) AND the verbatim text of that
// table (what `cpm sync` splices into config.toml between the CPM-MANAGED
// sentinels). Both come from the same bytes, so the file fleet-doctor's F10
// check grades a device against and the block cpm writes can never say two
// different things.
type FleetBase struct {
	Base *Base
	// Block is base.toml from its "[base]" header line to end of file: the
	// `[base]` / `[base.env]` / `os_overlay = {...}` shape, comments after
	// the header included, the leading documentation banner excluded, line
	// endings normalised to "\n", trailing whitespace trimmed.
	Block string
}

// LoadFleetBase reads fleet/profiles/base.toml from a fleet-repo checkout at
// repoPath -- the "single generator for [base]" named by the design spec's
// work item (B).
//
// The file's shape is the SAME as config.toml's [base] table, wrapper
// included. This is what shared/claude-plugins ships, what onboard renders
// a first-enrolled machine from, and what its fleet-doctor F10 check compares
// a live config.toml against key for key:
//
//	[base]
//	model = ""
//	args = ["--dangerously-skip-permissions", "...", "~/.claude-profiles/lane-authority.md"]
//	[base.env]
//	KEY = "value"
//	os_overlay = { windows = { KEY = "v" }, darwin = {} }
//
// The per-GOOS overlay is the `os_overlay` INLINE TABLE under [base.env]
// (keyed by Go's runtime.GOOS names) rather than [base.env.<goos>] subtable
// headers, because the fleet's own TOML reader cannot parse nested dotted
// headers -- see that file's FORMAT NOTE 2. `~/...` values are placeholders
// each machine resolves at render time (ResolveRendered), never here.
//
// A file without a "[base]" header line (e.g. a bare top-level model/args
// file -- the shape cpm v0.6.0 wrongly expected, which decoded the real file
// to an EMPTY table and rendered an empty [base] into config.toml without a
// word) is an error, as is a [base] table with no model, args, or env: cpm
// refuses to render nothing where the fleet clearly meant something.
func LoadFleetBase(repoPath string) (*FleetBase, error) {
	path := filepath.Join(ExpandPath(repoPath), "profiles", "base.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	block, ok := extractBaseTable(string(data))
	if !ok {
		return nil, fmt.Errorf("%s has no [base] table header -- expected the fleet shape ([base] / [base.env] / os_overlay = {...}); a bare top-level model/args file is not accepted", path)
	}
	var file struct {
		Base *Base `toml:"base"`
	}
	if err := toml.Unmarshal([]byte(block), &file); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", path, err)
	}
	b := file.Base
	if b == nil || (b.Model == "" && len(b.Args) == 0 && len(b.Env) == 0) {
		return nil, fmt.Errorf("%s: [base] declares no model, args, or env -- refusing to render an empty [base] into config.toml", path)
	}
	return &FleetBase{Base: b, Block: block}, nil
}

// extractBaseTable returns content from the first line that is exactly
// "[base]" (whitespace-trimmed; a commented-out or indented mention does not
// count) through end of file, CRLF normalised to LF and trailing whitespace
// removed. ok=false when no such line exists.
func extractBaseTable(content string) (block string, ok bool) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "[base]" {
			return strings.TrimRight(strings.Join(lines[i:], "\n"), " \t\n"), true
		}
	}
	return "", false
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
