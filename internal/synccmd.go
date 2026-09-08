package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// SyncOptions controls one `cpm sync` invocation (design spec §4).
type SyncOptions struct {
	Profile string // targets one profile; ignored when All is true
	All     bool   // targets every declared profile
	Apply   bool   // false (default) = dry-run: compute diffs, write nothing
	GOOS    string // defaults to runtime.GOOS; overridable so RunSync stays testable cross-platform
	Device  string // defaults to FleetDeviceID(cfg); overridable for fixture tests
}

// FileDiff names one file's before/after text for `cpm sync`'s per-file,
// per-profile diff output (design spec §4: "prints a unified diff per file
// per profile").
type FileDiff struct {
	File   string
	Before string
	After  string
}

// ProfileSyncResult is one profile's sync outcome. Empty Diffs means the
// profile is already fully in sync.
type ProfileSyncResult struct {
	Profile string
	Diffs   []FileDiff
}

// SyncReport is RunSync's complete result: the shared [base] diff (config.toml
// has exactly one, not one per profile) plus each targeted profile's own
// file diffs.
type SyncReport struct {
	BaseDiff *FileDiff
	Profiles []ProfileSyncResult
	// Warnings name the parts of the job RunSync could NOT do -- no [fleet]
	// repo_path, or a repo_path whose checkout has no profiles/base.toml --
	// so "everything already in sync" is never printed over a sync that
	// silently skipped its main input (the exact failure a fresh machine hit
	// on v0.6.0). Not drift: HasDrift ignores them; PrintSyncReport shows
	// them every time.
	Warnings []string
}

// HasDrift reports whether ANY diff -- [base] or any profile's -- is
// non-empty. Both `cpm sync --dry-run`'s exit code and `cpm doctor`'s drift
// report key off this single computation (design spec §4/§5: "the graders
// ... read the live files cpm just wrote" -- RunSync itself must not become
// a second, silently-diverging notion of "in sync").
func (r SyncReport) HasDrift() bool {
	if r.BaseDiff != nil {
		return true
	}
	for _, p := range r.Profiles {
		if len(p.Diffs) > 0 {
			return true
		}
	}
	return false
}

// RunSync computes (and, when opts.Apply is true, writes) the sync diff for
// the targeted profile(s): the shared config.toml [base] sentinel-splice
// (design spec §2), and, for each profile whose effective manage_mcp mode is
// "fleet-render", its staged mcpServers patch (design spec §5). It is the
// shared engine behind both `cpm sync` and `cpm doctor`'s drift report --
// deliberately the ONLY place that computes "did anything change", so the two
// commands can never disagree.
//
// A config whose [fleet].repo_path is unset, or whose fleet-repo checkout
// has no profiles/base.toml yet, produces no [base] diff (config.toml
// predating this feature is a valid state) -- but never a QUIET one: each
// case lands in SyncReport.Warnings so the operator learns the sync had no
// input, instead of reading "everything already in sync".
func RunSync(cfg *Config, configPath string, opts SyncOptions) (SyncReport, error) {
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	device := opts.Device
	if device == "" {
		device = FleetDeviceID(cfg)
	}

	var report SyncReport

	switch {
	case cfg.Fleet == nil || cfg.Fleet.RepoPath == "":
		report.Warnings = append(report.Warnings,
			"[fleet] repo_path is not set in config.toml -- config.toml [base] and per-profile mcpServers were NOT synced (set repo_path to this machine's shared/claude-plugins fleet/ checkout)")
	default:
		basePath := filepath.Join(ExpandPath(cfg.Fleet.RepoPath), "profiles", "base.toml")
		if _, statErr := os.Stat(basePath); statErr != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("%s not found -- config.toml [base] was NOT synced (fleet checkout predates work item B, or [fleet] repo_path points elsewhere)", basePath))
		} else {
			fleetBase, err := LoadFleetBase(cfg.Fleet.RepoPath)
			if err != nil {
				return report, fmt.Errorf("load fleet/profiles/base.toml: %w", err)
			}
			rendered := RenderBaseBlock(fleetBase.Block)
			newTrimmed := strings.TrimRight(rendered, "\n")

			rawConfig, err := os.ReadFile(ExpandPath(configPath))
			if err != nil {
				return report, fmt.Errorf("read config: %w", err)
			}
			existing, found := ExtractBase(string(rawConfig))
			if !found || existing != newTrimmed {
				report.BaseDiff = &FileDiff{File: "config.toml [base]", Before: existing, After: newTrimmed}
				if opts.Apply {
					spliced, changed, err := SpliceBase(string(rawConfig), rendered)
					if err != nil {
						return report, err
					}
					if changed {
						if err := writeFileAtomic(ExpandPath(configPath), []byte(spliced), 0o644); err != nil {
							return report, fmt.Errorf("write config: %w", err)
						}
					}
				}
			}
		}
	}

	names, err := syncTargetNames(cfg, opts)
	if err != nil {
		return report, err
	}

	profilesBase := ProfilesBaseDir(configPath)

	for _, name := range names {
		mode, err := cfg.EffectiveManageMCPMode(name)
		if err != nil {
			return report, err
		}
		if mode != ManageMCPFleetRender {
			continue
		}
		if cfg.Fleet == nil || cfg.Fleet.RepoPath == "" {
			continue // fleet-render declared but no repo_path configured yet -- nothing to stage from
		}

		renderedDir := FleetRenderedDir(cfg.Fleet.RepoPath, device, name)
		patch, err := LoadMCPServersPatch(renderedDir)
		if err != nil {
			return report, fmt.Errorf("profile %q: %w", name, err)
		}
		if len(patch) == 0 {
			continue
		}

		profileDir := filepath.Join(profilesBase, name)
		before, after, changed, err := DiffMCPServersPatch(profileDir, patch)
		if err != nil {
			return report, fmt.Errorf("profile %q: %w", name, err)
		}
		if !changed {
			continue
		}

		result := ProfileSyncResult{Profile: name, Diffs: []FileDiff{{
			File: filepath.Join(name, ".claude.json") + " mcpServers", Before: before, After: after,
		}}}
		if opts.Apply {
			if _, err := MergeMCPServersPatch(profileDir, patch); err != nil {
				return report, fmt.Errorf("profile %q: %w", name, err)
			}
		}
		report.Profiles = append(report.Profiles, result)
	}

	return report, nil
}

// syncTargetNames resolves the profile names one SyncOptions call targets:
// every declared profile (sorted, for deterministic output) when All is set,
// else the single named Profile (validated against cfg.Profiles).
func syncTargetNames(cfg *Config, opts SyncOptions) ([]string, error) {
	if opts.All {
		return sortedAliases(cfg), nil
	}
	if opts.Profile == "" {
		return nil, fmt.Errorf("cpm sync: pass --profile <name> or --all")
	}
	if _, ok := cfg.Profiles[opts.Profile]; !ok {
		return nil, fmt.Errorf("unknown profile %q (available: %s)", opts.Profile, strings.Join(sortedAliases(cfg), ", "))
	}
	return []string{opts.Profile}, nil
}

// PrintSyncReport renders a SyncReport to stdout: a unified before/after
// block per changed file, labeled dry-run or applied. applied reflects
// whether the caller actually wrote (opts.Apply), purely for the label --
// RunSync has already done (or not done) the writing by the time this runs.
func PrintSyncReport(report SyncReport, applied bool) {
	verb := "would change"
	if applied {
		verb = "applied"
	}
	for _, w := range report.Warnings {
		fmt.Fprintln(os.Stderr, "cpm sync: warning: "+w)
	}
	if !report.HasDrift() {
		fmt.Println("cpm sync: everything already in sync.")
		return
	}
	if report.BaseDiff != nil {
		fmt.Printf("\n%s (%s):\n--- before\n%s\n--- after\n%s\n", report.BaseDiff.File, verb, report.BaseDiff.Before, report.BaseDiff.After)
	}
	for _, p := range report.Profiles {
		for _, d := range p.Diffs {
			fmt.Printf("\n%s (%s):\n--- before\n%s\n--- after\n%s\n", d.File, verb, d.Before, d.After)
		}
	}
}
