package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var aliasRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// emailRe is intentionally permissive on the local part but rejects anything
// that is not clearly an address. A stricter unsafe-character check runs
// separately so a validated email is always safe to pass as an SSH argv token.
var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// shellUnsafe matches characters we refuse in an alias/email so a value is
// always safe to forward to a peer as a plain SSH argument.
var shellUnsafe = regexp.MustCompile("[`$;&|<>()\\\\\"' \\t\\r\\n]")

// ValidateAlias checks that an alias is a safe profile/launcher/config name.
func ValidateAlias(alias string) error {
	if alias == "" {
		return fmt.Errorf("alias must not be empty")
	}
	if !aliasRe.MatchString(alias) {
		return fmt.Errorf("invalid alias %q: use letters, digits, '-' and '_' (must start alphanumeric)", alias)
	}
	return nil
}

// ValidateEmail checks that an email is well-formed and shell-safe.
func ValidateEmail(email string) error {
	if !emailRe.MatchString(email) {
		return fmt.Errorf("invalid email %q", email)
	}
	if shellUnsafe.MatchString(email) {
		return fmt.Errorf("email %q contains unsafe characters", email)
	}
	return nil
}

// tomlValue renders a string as a TOML value, using a single-quoted literal when
// the value contains backslashes (e.g. Windows paths like
// `${USERPROFILE}\.claude\plugins`) so backslashes are never interpreted as
// escapes — matching the hand-written config convention.
func tomlValue(s string) string {
	if strings.Contains(s, "\\") && !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	r := strings.ReplaceAll(s, "\\", "\\\\")
	r = strings.ReplaceAll(r, "\"", "\\\"")
	return "\"" + r + "\""
}

// RenderProfileBlock renders a `[profiles.<alias>]` TOML block from a template
// profile, overriding description/email for the new account. Output is
// deterministic (sorted env keys) so it is unit-testable. env is expected to be
// already OS-appropriate for the target machine.
func RenderProfileBlock(alias, email, description string, args []string, model string, addDirs []string, env map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n# %s — added by `cpm add`. Isolated CLAUDE_CONFIG_DIR; run `claude-%s` then /login.\n", email, alias)
	fmt.Fprintf(&b, "[profiles.%s]\n", alias)
	fmt.Fprintf(&b, "description = %s\n", tomlValue(description))
	fmt.Fprintf(&b, "email = %s\n", tomlValue(email))
	if len(args) > 0 {
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = tomlValue(a)
		}
		fmt.Fprintf(&b, "args = [%s]\n", strings.Join(parts, ", "))
	}
	if model != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlValue(model))
	}
	if len(addDirs) > 0 {
		parts := make([]string, len(addDirs))
		for i, d := range addDirs {
			parts[i] = tomlValue(d)
		}
		fmt.Fprintf(&b, "add_dirs = [%s]\n", strings.Join(parts, ", "))
	}
	if len(env) > 0 {
		fmt.Fprintf(&b, "[profiles.%s.env]\n", alias)
		for _, k := range sortedEnvKeys(env) {
			fmt.Fprintf(&b, "%s = %s\n", k, tomlValue(env[k]))
		}
	}
	return b.String()
}

// staleConfigLockAge is how long a config.toml.lock sidecar file may sit
// unmodified before appendProfileBlock treats it as abandoned (left behind by
// a process that crashed or was killed while holding it) and reaps it, so a
// single hard kill can never wedge every future `cpm add` shut.
const staleConfigLockAge = 30 * time.Second

// acquireConfigLock takes a simple, cross-platform mutual-exclusion lock on
// config.toml via O_CREATE|O_EXCL on a `<config>.lock` sidecar file, so two
// concurrent `cpm add` invocations (e.g. a local add racing a fleet peer's
// add over SSH — see AddProfileToFleet) can no longer both read the
// pre-append file and race a write, silently discarding one another's
// appended block. Call the returned release func (always, via defer) to drop
// the lock.
func acquireConfigLock(configPath string) (release func(), err error) {
	lockPath := ExpandPath(configPath) + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("cannot create config lock: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > staleConfigLockAge {
			os.Remove(lockPath) // orphaned by a crashed/killed process; reap and retry immediately
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for config lock %s (another `cpm add` running?)", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// writeFileAtomic writes data to a temp file in the same directory as path,
// then renames it into place. The rename is atomic on both POSIX and Windows
// (same mechanism used by the self-update in version.go), so a crash or a
// killed process between the write and the rename can never leave path
// truncated/partially written — it is always either its full previous
// content or its full new content, never anything in between.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomicPattern(path, data, perm, ".cpm-config-*.tmp")
}

// writeFileAtomicPattern is writeFileAtomic generalized over the temp-file
// glob pattern, so callers writing a different class of file (e.g.
// fleetcreds.go's credential pulls, which use ".cpm-cred-*.tmp" so an
// interrupted transfer's leftover temp file is recognizable at a glance) get
// the same atomicity guarantee without colliding on cpm's config-append temp
// pattern.
func writeFileAtomicPattern(path string, data []byte, perm os.FileMode, tmpPattern string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("cannot write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot write temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot set permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("cannot replace config: %w", err)
	}
	return nil
}

// appendProfileBlock appends a rendered profile block to the config file,
// preserving all existing comments/formatting (BurntSushi/toml does not
// round-trip comments, so we never re-serialize the whole file). The
// read-modify-write is guarded by acquireConfigLock and written via
// writeFileAtomic so neither a crash mid-write nor a concurrent `cpm add` can
// truncate/corrupt config.toml or silently lose another process's append.
func appendProfileBlock(configPath, block string) error {
	release, err := acquireConfigLock(configPath)
	if err != nil {
		return err
	}
	defer release()

	p := ExpandPath(configPath)
	data, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("cannot read config: %w", err)
	}
	content := string(data)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += block
	return writeFileAtomic(p, []byte(content), 0o644)
}

// AddProfile adds a new account profile locally: it validates inputs, clones the
// template's args/env, appends a `[profiles.<alias>]` block to config.toml,
// materializes the profile dir (seed settings, link shared dirs, optional MCP
// sync), installs the launcher, and prints the login next-step. Credentials are
// never created — the user must `/login` once.
func AddProfile(cfg *Config, configPath, email, alias, fromProfile string, loginNow bool) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	if err := ValidateEmail(email); err != nil {
		return err
	}
	if _, exists := cfg.Profiles[alias]; exists {
		return fmt.Errorf("profile %q already exists in config", alias)
	}

	tmplName, err := cfg.TemplateProfileName(fromProfile)
	if err != nil {
		return err
	}
	tmpl := cfg.Profiles[tmplName]
	description := fmt.Sprintf("Claude Max — %s", email)

	block := RenderProfileBlock(alias, email, description, tmpl.Args, tmpl.Model, tmpl.AddDirs, tmpl.Env)
	if err := appendProfileBlock(configPath, block); err != nil {
		return err
	}
	fmt.Printf("Added [profiles.%s] to %s (template: %s)\n", alias, ExpandPath(configPath), tmplName)

	// Reload to validate the append parsed and to get the canonical profile.
	cfg2, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("config no longer parses after add (check %s): %w", ExpandPath(configPath), err)
	}
	profile, ok := cfg2.Profiles[alias]
	if !ok {
		return fmt.Errorf("internal error: profile %q missing after append", alias)
	}

	profilesBase := ProfilesBaseDir(configPath)
	profileDir := filepath.Join(profilesBase, alias)

	fmt.Printf("\nProfile: %s\n", alias)
	if err := SetupProfile(alias, profileDir, cfg2.SourceDir, false); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if err := PatchAttribution(profileDir, profile.Attribution); err != nil {
		return fmt.Errorf("attribution: %w", err)
	}
	if cfg2.ManageMCPEnabled() {
		if err := SyncMCPServers(profileDir); err != nil {
			return fmt.Errorf("mcp sync: %w", err)
		}
	} else {
		fmt.Println("  skipped MCP sync (manage_mcp = false; gateway owns MCP)")
	}

	cpmPath, err := os.Executable()
	if err != nil {
		cpmPath = "cpm"
	}
	wrapper := GenerateLauncher(alias, profileDir, profile, cpmPath)
	scriptPath := filepath.Join(cfg2.BinDir, LauncherFileName(alias))
	if err := InstallWrapper(scriptPath, wrapper); err != nil {
		return fmt.Errorf("wrapper: %w", err)
	}

	fmt.Printf("\nProfile %q ready.\n", alias)
	if loginNow {
		fmt.Printf("Signing in to %s — a browser window will open...\n", email)
		if err := runProfileLogin(alias, profileDir, profile, email); err != nil {
			fmt.Printf("  sign-in didn't complete (%v)\n", err)
			fmt.Printf("  finish later: claude-%s auth login --email %s\n", alias, email)
		} else if acct, ok := profileAuthStatus(alias, profileDir, profile); ok {
			fmt.Printf("  signed in as %s\n", acct)
		} else {
			fmt.Printf("  sign-in launched; verify with: claude-%s auth status\n", alias)
		}
	} else {
		fmt.Printf("  Sign in:  claude-%s auth login --email %s   (or launch claude-%s and /login)\n", alias, email, alias)
	}
	return nil
}

// runProfileLogin runs `claude auth login --email <email>` directly under the
// profile's isolated environment, with inherited stdio so the interactive
// OAuth sign-in works.
//
// It builds the invocation via BuildRunInvocation — the exact same plumbing
// `cpm run` and the generated launchers use — instead of shelling out to a
// nested `cpm run <alias> auth login` subprocess. A nested `cpm run` cannot be
// steered with a forwarded --config: `run` sets cobra's DisableFlagParsing,
// and cobra's ParseFlags is an unconditional no-op whenever DisableFlagParsing
// is set (spf13/cobra command.go), regardless of whether --config is placed
// before or after `run` on the child's argv — verified live, both placements
// left the child resolving internal.DefaultConfigPath(), i.e. the REAL
// ~/.claude-profiles/config.toml, ignoring whatever --config the parent `cpm
// add` was actually invoked with. Calling BuildRunInvocation in-process with
// the alias/profileDir/profile the parent already resolved from its own
// --config removes that second, unsteerable config resolution entirely.
//
// `auth` is in runBypass (exec_run.go), so BuildRunInvocation returns the
// argv undecorated — no --add-dir/--model/--dangerously-skip-permissions is
// injected into the auth subcommand.
func runProfileLogin(alias, profileDir string, profile *Profile, email string) error {
	claudePath, argv, env, err := BuildRunInvocation(alias, profileDir, profile, []string{"auth", "login", "--email", email})
	if err != nil {
		return err
	}
	cmd := exec.Command(claudePath, argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// profileAuthStatus returns the signed-in account email for a profile, if the
// profile is authenticated (via `claude auth status --json`). See
// runProfileLogin for why this calls BuildRunInvocation directly rather than
// shelling out to a nested `cpm run`.
func profileAuthStatus(alias, profileDir string, profile *Profile) (string, bool) {
	claudePath, argv, env, err := BuildRunInvocation(alias, profileDir, profile, []string{"auth", "status", "--json"})
	if err != nil {
		return "", false
	}
	cmd := exec.Command(claudePath, argv[1:]...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	var s struct {
		LoggedIn bool   `json:"loggedIn"`
		Email    string `json:"email"`
	}
	if json.Unmarshal(out, &s) != nil || !s.LoggedIn {
		return "", false
	}
	return s.Email, true
}
