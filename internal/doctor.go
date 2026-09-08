package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

type Check struct {
	Name   string
	Status string // "ok", "warn", "error"
	Detail string
}

// RunDoctor diagnoses profile/config issues. configPath enables the two
// config.toml-aware checks added by the design spec's work item (A) --
// orphan-directory scan and the [base] local-modification flag -- both of
// which need to read config.toml's raw text and (for the [base] check) the
// fleet-repo checkout it points at. Pass "" to skip those two checks
// entirely (e.g. from a caller that only has an in-memory *Config, no file
// on disk) -- every other check runs exactly as before.
func RunDoctor(cfg *Config, profilesBase, configPath string) []Check {
	var checks []Check

	// Check claude binary
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		checks = append(checks, Check{"claude binary", "error", "claude not found on PATH"})
	} else {
		checks = append(checks, Check{"claude binary", "ok", claudePath})
	}

	// Check source dir
	if _, err := os.Stat(cfg.SourceDir); os.IsNotExist(err) {
		checks = append(checks, Check{"source directory", "error", fmt.Sprintf("%s does not exist", cfg.SourceDir)})
	} else {
		checks = append(checks, Check{"source directory", "ok", cfg.SourceDir})
	}

	// Check bin dir
	if _, err := os.Stat(cfg.BinDir); os.IsNotExist(err) {
		checks = append(checks, Check{"bin directory", "warn", fmt.Sprintf("%s does not exist (will be created on install)", cfg.BinDir)})
	} else {
		checks = append(checks, Check{"bin directory", "ok", cfg.BinDir})
	}

	// Check bin dir is on PATH
	pathDirs := filepath.SplitList(os.Getenv("PATH"))
	binOnPath := false
	for _, d := range pathDirs {
		if d == cfg.BinDir {
			binOnPath = true
			break
		}
	}
	if binOnPath {
		checks = append(checks, Check{"bin dir on PATH", "ok", cfg.BinDir})
	} else {
		detail := fmt.Sprintf("%s is not on PATH", cfg.BinDir)
		if runtime.GOOS == "windows" {
			// Guide the user; never mutate PATH automatically. setx truncates
			// PATH at 1024 chars, so recommend the .NET Environment API instead.
			detail += fmt.Sprintf(" — add it (user scope, no elevation): "+
				"[Environment]::SetEnvironmentVariable('Path', \"$env:Path;%s\", 'User')", cfg.BinDir)
		}
		checks = append(checks, Check{"bin dir on PATH", "warn", detail})
	}

	// Check each profile
	for name := range cfg.Profiles {
		profileDir := filepath.Join(profilesBase, name)

		if _, err := os.Stat(profileDir); os.IsNotExist(err) {
			checks = append(checks, Check{fmt.Sprintf("profile/%s", name), "warn", "not installed (run cpm install)"})
			continue
		}

		// Check links (symlinks on Unix, junctions on Windows)
		for _, dir := range symlinkDirs {
			link := filepath.Join(profileDir, dir)
			if !isLink(link) {
				continue // not a link or doesn't exist
			}
			// os.Stat follows the link/junction; an error means a dangling target.
			if _, err := os.Stat(link); err != nil {
				checks = append(checks, Check{fmt.Sprintf("profile/%s/%s", name, dir), "error", "broken link (target missing)"})
			}
		}

		// Check credentials
		credPath := filepath.Join(profileDir, ".credentials.json")
		credInfo, err := os.Stat(credPath)
		if os.IsNotExist(err) {
			checks = append(checks, Check{fmt.Sprintf("profile/%s/credentials", name), "warn", "not authenticated (run claude-" + name + ")"})
		} else if err == nil {
			age := time.Since(credInfo.ModTime())
			if age > 7*24*time.Hour {
				checks = append(checks, Check{fmt.Sprintf("profile/%s/credentials", name), "warn", fmt.Sprintf("credentials last updated %s ago", formatDuration(age))})
			} else {
				checks = append(checks, Check{fmt.Sprintf("profile/%s/credentials", name), "ok", fmt.Sprintf("last updated %s ago", formatDuration(age))})
			}
		}

		// Check wrapper script
		scriptPath := filepath.Join(cfg.BinDir, LauncherFileName(name))
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			checks = append(checks, Check{fmt.Sprintf("profile/%s/wrapper", name), "warn", "wrapper script missing (run cpm install)"})
		} else {
			checks = append(checks, Check{fmt.Sprintf("profile/%s/wrapper", name), "ok", scriptPath})
		}
	}

	checks = append(checks, orphanDirectoryChecks(cfg, profilesBase)...)
	checks = append(checks, syncDriftChecks(cfg, configPath)...)

	return checks
}

// orphanDirectoryChecks scans profilesBase for a directory with no matching
// [profiles.*] key in cfg (design spec §4). Reported and NEVER touched -- no
// auto-delete, no auto-adopt, ever: this defends against the "digrum "
// trailing-space incident class (a mistyped CLAUDE_CONFIG_DIR that no
// existing command lists, so it stays invisible until scanned for), and
// deliberately does not require configPath -- it works from cfg.Profiles and
// the live directory listing alone.
func orphanDirectoryChecks(cfg *Config, profilesBase string) []Check {
	entries, err := os.ReadDir(profilesBase)
	if err != nil {
		return nil
	}
	var orphanNames []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, registered := cfg.Profiles[e.Name()]; registered {
			continue
		}
		orphanNames = append(orphanNames, e.Name())
	}
	sort.Strings(orphanNames)

	checks := make([]Check, 0, len(orphanNames))
	for _, name := range orphanNames {
		checks = append(checks, Check{
			Name:   fmt.Sprintf("profile-orphan/%s", name),
			Status: "drift",
			Detail: fmt.Sprintf("ORPHAN (not in config.toml, NEVER touched): %s", filepath.Join(profilesBase, name)),
		})
	}
	return checks
}

// syncDriftChecks reuses `cpm sync`'s own dry-run diff engine (RunSync) to
// report the [base] local-modification flag and any per-profile drift
// (design spec §4, items 2-3) -- deliberately the SAME computation `cpm
// sync --dry-run` uses, so doctor and sync can never disagree about what
// counts as "in sync". Returns nil when configPath is "" (an in-memory-only
// *Config with no file on disk to diff against) rather than erroring --
// every other doctor check still runs.
func syncDriftChecks(cfg *Config, configPath string) []Check {
	if configPath == "" {
		return nil
	}
	report, err := RunSync(cfg, configPath, SyncOptions{All: true, GOOS: runtime.GOOS})
	if err != nil {
		// A config.toml predating this feature (no [fleet].repo_path, no
		// [base]) is a valid quiet state inside RunSync itself; an error
		// here means something else is actually wrong (e.g. configPath no
		// longer readable) and is worth a check entry, not a silent skip.
		return []Check{{Name: "sync-drift", Status: "warn", Detail: fmt.Sprintf("could not compute sync drift: %v", err)}}
	}

	var checks []Check
	if report.BaseDiff != nil {
		checks = append(checks, Check{
			Name:   "config.toml [base]",
			Status: "drift",
			Detail: "local [base] differs from fleet/profiles/base.toml -- will be overwritten on the next `cpm sync --apply` (run `cpm sync --dry-run` to see the diff)",
		})
	}
	for _, p := range report.Profiles {
		for _, d := range p.Diffs {
			checks = append(checks, Check{
				Name:   fmt.Sprintf("profile/%s/%s", p.Profile, d.File),
				Status: "drift",
				Detail: fmt.Sprintf("drift detected (run `cpm sync --dry-run --profile %s` to see the diff)", p.Profile),
			})
		}
	}
	return checks
}

func PrintChecks(checks []Check) {
	for _, c := range checks {
		var icon string
		switch c.Status {
		case "ok":
			icon = "  OK"
		case "warn":
			icon = "WARN"
		case "error":
			icon = " ERR"
		case "drift":
			icon = "DRFT"
		}
		fmt.Printf("  [%s] %-35s %s\n", icon, c.Name, c.Detail)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// GetCredentialInfo reads a profile's .credentials.json and reports its
// account identifier and expiry state.
//
// The REAL shape written by Claude Code nests everything under
// claudeAiOauth (claudeAiOauth.expiresAt, a MILLISECOND epoch — confirmed
// live: a freshly-minted token's expiresAt is exactly +8h from its file
// mtime in ms, not seconds). The flat top-level expires_at/expires_in/email
// keys checked below were never confirmed against a real credentials file;
// they are kept only as a legacy-shape fallback for compatibility. Before
// this fix, GetCredentialInfo looked ONLY at the flat keys, so every real
// profile's expiry silently read as "valid" regardless of actual state
// (live-verified: `cpm credentials` reported "(unknown account) [valid]" for
// 6/6 profiles while 5 had access tokens already expired by 8-22h) — this is
// the same file cpm fleet creds now reads via CredSnapshot, so the two must
// not disagree about it.
//
// GetCredentialInfo does not know a profile's configured email (only the
// credentials file, which the real shape never carries) — a caller with
// access to Config should prefer cfg.Profiles[alias].Email over
// "(unknown account)" when this returns that sentinel.
func GetCredentialInfo(profileDir string) (account string, expired bool, err error) {
	credPath := filepath.Join(profileDir, ".credentials.json")
	data, err := os.ReadFile(credPath)
	if err != nil {
		return "", false, fmt.Errorf("no credentials found")
	}

	var creds map[string]any
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", false, fmt.Errorf("cannot parse credentials")
	}

	// Try to extract account info (legacy flat shape only; the real
	// claudeAiOauth shape carries no account identifier at all).
	if email, ok := creds["email"].(string); ok {
		account = email
	} else if sub, ok := creds["subject"].(string); ok {
		account = sub
	} else if id, ok := creds["account_uuid"].(string); ok {
		account = id
	} else {
		account = "(unknown account)"
	}

	// Real shape: claudeAiOauth.expiresAt, milliseconds.
	if oauth, ok := creds["claudeAiOauth"].(map[string]any); ok {
		if expiresAtMs, ok := oauth["expiresAt"].(float64); ok {
			expTime := time.UnixMilli(int64(expiresAtMs))
			return account, time.Now().After(expTime), nil
		}
	}

	// Legacy/fallback shape: flat seconds-epoch expires_at, or a relative
	// expires_in measured from the file's mtime.
	if expiresAt, ok := creds["expires_at"].(float64); ok {
		expTime := time.Unix(int64(expiresAt), 0)
		expired = time.Now().After(expTime)
	} else if expiresIn, ok := creds["expires_in"].(float64); ok {
		info, statErr := os.Stat(credPath)
		if statErr == nil {
			expTime := info.ModTime().Add(time.Duration(expiresIn) * time.Second)
			expired = time.Now().After(expTime)
		}
	}

	return account, expired, nil
}
