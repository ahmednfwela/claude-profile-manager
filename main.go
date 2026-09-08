package main

import (
	"os/exec"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jakubkontra/cpm/internal"
	"github.com/spf13/cobra"
)

var configPath string

const banner = `
   _____ ____  __  __
  / ____|  _ \|  \/  |
 | |    | |_) | \  / |
 | |    |  __/| |\/| |
 | |____| |   | |  | |
  \_____|_|   |_|  |_|
  Claude Profile Manager
`

func main() {
	root := &cobra.Command{
		Use:   "cpm",
		Short: "Claude Profile Manager — manage multiple Claude Code accounts",
		Long:  banner + "\n  Manage multiple Claude Code accounts with isolated profiles.\n  https://github.com/jakubkontra/claude-profile-manager",
	}

	root.PersistentFlags().StringVar(&configPath, "config", internal.DefaultConfigPath(), "path to config file")

	root.AddCommand(installCmd())
	root.AddCommand(listCmd())
	root.AddCommand(statusCmd())
	root.AddCommand(direnvCmd())
	root.AddCommand(useCmd())
	root.AddCommand(whichCmd())
	root.AddCommand(initCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(syncCmd())
	root.AddCommand(runCmd())
	root.AddCommand(handoffCmd())
	root.AddCommand(cloneCmd())
	root.AddCommand(promptCmd())
	root.AddCommand(credentialsCmd())
	root.AddCommand(hookCmd())
	root.AddCommand(linkCmd())
	root.AddCommand(unlinkCmd())
	root.AddCommand(versionCmd())
	root.AddCommand(upgradeCmd())
	root.AddCommand(cloudCmd())
	root.AddCommand(addCmd())
	root.AddCommand(fleetCmd())
	root.AddCommand(channelCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func installCmd() *cobra.Command {
	var sync, force bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Create profile directories and install wrapper scripts",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			profilesBase := internal.ProfilesBaseDir(configPath)

			if sync && !force {
				diverged := internal.CheckDivergence(cfg, profilesBase)
				if len(diverged) > 0 {
					fmt.Print("\nDiverged profile files detected:\n\n")
					for _, d := range diverged {
						fmt.Printf("  %s\n", d.Details)
					}
					fmt.Println("\nMerge changes back to source first, or use: --sync --force")
					return nil
				}
			}

			activeNames := make(map[string]bool)
			names := sortedKeys(cfg.Profiles)

			cpmPath, err := os.Executable()
			if err != nil {
				cpmPath = "cpm" // fall back to PATH resolution
			}

			for _, name := range names {
				profile := cfg.Profiles[name]
				profileDir := filepath.Join(profilesBase, name)
				scriptName := internal.LauncherFileName(name)

				activeNames[scriptName] = true

				fmt.Printf("\nProfile: %s\n", name)

				if err := internal.SetupProfile(name, profileDir, cfg.SourceDir, sync); err != nil {
					return fmt.Errorf("profile %s: %w", name, err)
				}

				if err := internal.PatchAttribution(profileDir, profile.Attribution); err != nil {
					return fmt.Errorf("profile %s attribution: %w", name, err)
				}

				if cfg.ManageMCPEnabled() {
					if err := internal.SyncMCPServers(profileDir); err != nil {
						return fmt.Errorf("profile %s mcp sync: %w", name, err)
					}
				}

				wrapper := internal.GenerateLauncher(name, profileDir, profile, cpmPath)
				scriptPath := filepath.Join(cfg.BinDir, scriptName)
				if err := internal.InstallWrapper(scriptPath, wrapper); err != nil {
					return fmt.Errorf("profile %s wrapper: %w", name, err)
				}
			}

			fmt.Println("\nCleanup:")
			internal.CleanupStaleScripts(cfg.BinDir, activeNames)

			fmt.Println("\nDone.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&sync, "sync", false, "re-copy mutable files from source")
	cmd.Flags().BoolVar(&force, "force", false, "force overwrite diverged files (use with --sync)")

	return cmd
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all configured profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			profilesBase := internal.ProfilesBaseDir(configPath)
			names := sortedKeys(cfg.Profiles)
			current := internal.CurrentProfile()

			for _, name := range names {
				profile := cfg.Profiles[name]
				profileDir := filepath.Join(profilesBase, name)

				status := "not installed"
				if _, err := os.Stat(profileDir); err == nil {
					status = "installed"
				}

				credPath := filepath.Join(profileDir, ".credentials.json")
				if _, err := os.Stat(credPath); err == nil {
					status = "authenticated"
				}

				desc := profile.Description
				if desc == "" {
					desc = "(no description)"
				}

				marker := "  "
				if name == current {
					marker = "* "
				}

				fmt.Printf("%sclaude-%-20s %s  [%s]\n", marker, name, desc, status)
			}

			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show sync status of all profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			profilesBase := internal.ProfilesBaseDir(configPath)
			diverged := internal.CheckDivergence(cfg, profilesBase)

			if len(diverged) == 0 {
				fmt.Println("All profiles are in sync with source.")
				return nil
			}

			fmt.Println("Diverged files:")
			for _, d := range diverged {
				fmt.Printf("  %s\n", d.Details)
			}
			fmt.Println("\nRun 'cpm install --sync' to re-sync.")

			return nil
		},
	}
}

func direnvCmd() *cobra.Command {
	var shellFlag string
	cmd := &cobra.Command{
		Use:   "direnv <profile>",
		Short: "Print .envrc snippet for a profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			shell, err := internal.ParseShell(shellFlag)
			if err != nil {
				return err
			}

			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			name := args[0]
			if _, ok := cfg.Profiles[name]; !ok {
				available := strings.Join(sortedKeys(cfg.Profiles), ", ")
				return fmt.Errorf("unknown profile %q (available: %s)", name, available)
			}

			profilesBase := internal.ProfilesBaseDir(configPath)
			profileDir := filepath.Join(profilesBase, name)

			fmt.Print(internal.GenerateDirenvSnippet(name, profileDir, shell))
			return nil
		},
	}

	cmd.Flags().StringVar(&shellFlag, "shell", "", "target shell: bash, zsh, or powershell (default: auto by OS)")
	return cmd
}

func useCmd() *cobra.Command {
	var shellFlag string
	cmd := &cobra.Command{
		Use:   "use <profile>",
		Short: "Switch the current shell to a profile (use with eval)",
		Long:  "Switch the current shell to a profile.\nPOSIX:      eval \"$(cpm use <profile>)\"\nPowerShell: cpm use <profile> | Invoke-Expression",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			shell, err := internal.ParseShell(shellFlag)
			if err != nil {
				return err
			}

			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			profile, ok := cfg.Profiles[name]
			if !ok {
				// Try .claude-profile auto-detection if "auto" is passed
				if name == "auto" {
					detected, err := internal.DetectProfileFile(".")
					if err != nil {
						return fmt.Errorf("no .claude-profile found in current or parent directories")
					}
					name = detected
					profile, ok = cfg.Profiles[name]
					if !ok {
						return fmt.Errorf("profile %q from .claude-profile not found in config", name)
					}
				} else {
					available := strings.Join(sortedKeys(cfg.Profiles), ", ")
					return fmt.Errorf("unknown profile %q (available: %s)", name, available)
				}
			}

			profilesBase := internal.ProfilesBaseDir(configPath)
			profileDir := filepath.Join(profilesBase, name)

			fmt.Print(internal.GenerateUseOutput(name, profileDir, profile, shell))
			return nil
		},
	}

	cmd.Flags().StringVar(&shellFlag, "shell", "", "target shell: bash, zsh, or powershell (default: auto by OS)")
	return cmd
}

func whichCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "which",
		Short: "Show the currently active profile",
		Run: func(cmd *cobra.Command, args []string) {
			profile := internal.CurrentProfile()
			configDir := internal.CurrentConfigDir()

			if profile != "" {
				fmt.Printf("Profile:    %s\n", profile)
				fmt.Printf("Config dir: %s\n", configDir)
				fmt.Printf("Source:     environment (CLAUDE_PROFILE)\n")
				fmt.Printf("Command:    claude-%s\n", profile)
				return
			}

			// Try to detect from .claude-profile file
			detected, err := internal.DetectProfileFile(".")
			if err == nil {
				fmt.Printf("Profile:    %s\n", detected)
				fmt.Printf("Source:     .claude-profile\n")
				fmt.Printf("Command:    claude-%s\n", detected)
				fmt.Println("\nNote: profile detected from .claude-profile but not active in this shell.")
				fmt.Println("Run: eval \"$(cpm use " + detected + ")\"")
				fmt.Println("Or add to .zshrc: eval \"$(cpm hook)\"")
				return
			}

			fmt.Println("No active profile.")
			fmt.Println("  - No CLAUDE_PROFILE env var set")
			fmt.Println("  - No .claude-profile file found in current or parent directories")
		},
	}
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Interactive setup wizard for config.toml",
		RunE: func(cmd *cobra.Command, args []string) error {
			return internal.RunInit(configPath)
		},
	}
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose issues with profiles and configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			profilesBase := internal.ProfilesBaseDir(configPath)
			checks := internal.RunDoctor(cfg, profilesBase, configPath)

			fmt.Print("cpm doctor\n\n")
			internal.PrintChecks(checks)

			hasErrors := false
			hasDrift := false
			for _, c := range checks {
				switch c.Status {
				case "error":
					hasErrors = true
				case "drift":
					hasDrift = true
				}
			}

			if hasErrors {
				fmt.Println("\nSome checks failed. Fix the issues above.")
			} else {
				fmt.Println("\nAll checks passed.")
			}

			// Orphan directories and config.toml/mcpServers drift each
			// warrant attention before the next `cpm sync --apply` silently
			// overwrites them (design spec §4: "Exit non-zero on any drift
			// or orphan"). Deliberately a SEPARATE signal from the
			// pre-existing hasErrors path above, which this leaves
			// unchanged (cpm doctor has never exited non-zero for those).
			if hasDrift {
				return fmt.Errorf("drift or orphan detected -- see [DRFT] entries above")
			}

			return nil
		},
	}
}

// syncCmd is `cpm sync` (design spec §4): renders every declared profile's
// [base]+delta and, for manage_mcp="fleet-render" profiles, applies the
// fleet repo's staged mcpServers patch. Default is dry-run (mirrors `tofu
// plan`/`apply` -- rule 1's spirit): --apply must be spelled out explicitly
// to write. Exit codes: 0 = no diff (or a successful --apply), 1 = a real
// error, 2 = drift found under --dry-run.
func syncCmd() *cobra.Command {
	var profile string
	var all bool
	var apply bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "sync [--profile <name> | --all] [--dry-run | --apply]",
		Short: "Render [base]+delta into config.toml and apply staged fleet-render patches",
		Long: "Renders every declared profile's args/env from [base] plus its own delta\n" +
			"(see the design spec's RenderProfile algorithm), splices the fleet-wide\n" +
			"[base] table into config.toml from fleet/profiles/base.toml, and -- for any\n" +
			"profile whose effective manage_mcp mode is \"fleet-render\" -- applies that\n" +
			"profile's staged mcpServers patch as an additive-only merge.\n\n" +
			"Default is --dry-run: prints the diff, writes nothing. Pass --apply to write.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !all && profile == "" {
				return fmt.Errorf("pass --profile <name> or --all")
			}
			if apply && dryRun {
				return fmt.Errorf("--apply and --dry-run are mutually exclusive")
			}

			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			report, err := internal.RunSync(cfg, configPath, internal.SyncOptions{
				Profile: profile, All: all, Apply: apply,
			})
			if err != nil {
				return err // cobra -> main() -> os.Exit(1)
			}

			internal.PrintSyncReport(report, apply)

			if report.HasDrift() && !apply {
				os.Exit(2)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&profile, "profile", "", "sync only this profile")
	cmd.Flags().BoolVar(&all, "all", false, "sync every declared profile")
	cmd.Flags().BoolVar(&apply, "apply", false, "write the computed changes (default is dry-run)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "explicit dry-run (default behavior when --apply is omitted)")
	return cmd
}

func runCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "run <profile> [claude args...]",
		Short:              "Run claude with a specific profile (one-shot)",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			claudeArgs := args[1:]

			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			profile, ok := cfg.Profiles[name]
			if !ok {
				available := strings.Join(sortedKeys(cfg.Profiles), ", ")
				return fmt.Errorf("unknown profile %q (available: %s)", name, available)
			}

			profilesBase := internal.ProfilesBaseDir(configPath)
			profileDir := filepath.Join(profilesBase, name)

			claudePath, argv, env, err := internal.BuildRunInvocation(name, profileDir, profile, claudeArgs)
			if err != nil {
				return err
			}

			return internal.RunClaude(claudePath, argv, env)
		},
	}
}

func handoffCmd() *cobra.Command {
	var prompt, name string

	cmd := &cobra.Command{
		Use:   "handoff <session-id> <from-profile> <to-profile>",
		Short: "Move a background session to another profile's account (stop, then --bg --resume)",
		Long: "Stops the background session on <from-profile>, then re-dispatches its conversation\n" +
			"as a new background session on <to-profile> via `claude --bg --resume`. Use when one\n" +
			"account's usage limits are exhausted and the work must continue on another.\n" +
			"Accepts the full session UUID or the 8-char short id shown by `claude agents`.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID, fromName, toName := args[0], args[1], args[2]

			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			fromProfile, ok := cfg.Profiles[fromName]
			if !ok {
				return fmt.Errorf("unknown profile %q (available: %s)", fromName, strings.Join(sortedKeys(cfg.Profiles), ", "))
			}
			toProfile, ok := cfg.Profiles[toName]
			if !ok {
				return fmt.Errorf("unknown profile %q (available: %s)", toName, strings.Join(sortedKeys(cfg.Profiles), ", "))
			}

			// An empty name is resolved inside HandoffSession: the origin
			// session's own name (jobs/<short>/state.json), else handoff-<short>.
			profilesBase := internal.ProfilesBaseDir(configPath)
			return internal.HandoffSession(
				fromName, filepath.Join(profilesBase, fromName), fromProfile,
				toName, filepath.Join(profilesBase, toName), toProfile,
				sessionID, prompt, name,
			)
		},
	}
	cmd.Flags().StringVar(&prompt, "prompt", "Continue the previous task from exactly where it left off. Ignore hook housekeeping notices.", "prompt for the re-dispatched session's first turn")
	cmd.Flags().StringVar(&name, "name", "", "name for the new background session (default: the origin session's name, else handoff-<short-id>)")
	return cmd
}

func cloneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clone <source-profile> <new-profile>",
		Short: "Clone an existing profile (without credentials)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			sourceName := args[0]
			targetName := args[1]

			if _, ok := cfg.Profiles[sourceName]; !ok {
				available := strings.Join(sortedKeys(cfg.Profiles), ", ")
				return fmt.Errorf("unknown source profile %q (available: %s)", sourceName, available)
			}

			profilesBase := internal.ProfilesBaseDir(configPath)
			return internal.CloneProfile(sourceName, targetName, profilesBase, cfg.SourceDir, configPath, cfg)
		},
	}
}

func promptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prompt",
		Short: "Print current profile name for shell prompt (PS1/starship)",
		Run: func(cmd *cobra.Command, args []string) {
			p := internal.PromptString()
			if p != "" {
				fmt.Print(p)
			}
		},
	}
}

func credentialsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "credentials",
		Short: "Show credential status for all profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			profilesBase := internal.ProfilesBaseDir(configPath)
			names := sortedKeys(cfg.Profiles)

			for _, name := range names {
				profileDir := filepath.Join(profilesBase, name)
				account, expired, err := internal.GetCredentialInfo(profileDir)

				if err != nil {
					fmt.Printf("  claude-%-20s %s\n", name, err)
				} else {
					// The real credentials file never carries an account
					// identifier (see GetCredentialInfo's doc comment) — prefer
					// the profile's own configured email over the sentinel.
					if account == "(unknown account)" {
						if p := cfg.Profiles[name]; p != nil && p.Email != "" {
							account = p.Email
						}
					}
					status := "valid"
					if expired {
						status = "EXPIRED"
					}
					fmt.Printf("  claude-%-20s %s  [%s]\n", name, account, status)
				}
			}

			return nil
		},
	}
}

func hookCmd() *cobra.Command {
	var shellFlag string
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Print shell hook for auto-switching via .claude-profile files",
		Long:  "Print shell hook for auto-switching.\nPOSIX:      add to your .zshrc/.bashrc: eval \"$(cpm hook)\"\nPowerShell: add to your $PROFILE:        cpm hook --shell powershell | Out-String | Invoke-Expression",
		RunE: func(cmd *cobra.Command, args []string) error {
			shell, err := internal.ParseShell(shellFlag)
			if err != nil {
				return err
			}
			fmt.Print(internal.GenerateShellHook(shell))
			return nil
		},
	}

	cmd.Flags().StringVar(&shellFlag, "shell", "", "target shell: bash, zsh, or powershell (default: auto by OS)")
	return cmd
}

func linkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link <profile>",
		Short: "Create .claude-profile in current directory (like .nvmrc)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}

			if _, ok := cfg.Profiles[name]; !ok {
				available := strings.Join(sortedKeys(cfg.Profiles), ", ")
				return fmt.Errorf("unknown profile %q (available: %s)", name, available)
			}

			cwd, err := os.Getwd()
			if err != nil {
				return err
			}

			if err := internal.LinkProfile(cwd, name); err != nil {
				return err
			}

			fmt.Printf("Linked profile %q to %s\n", name, cwd)
			fmt.Println("\nTo auto-switch, add to your .zshrc:")
			fmt.Println("  eval \"$(cpm hook)\"")

			return nil
		},
	}
}

func unlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink",
		Short: "Remove .claude-profile from current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}

			if err := internal.UnlinkProfile(cwd); err != nil {
				return err
			}

			fmt.Println("Removed .claude-profile")
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Print(banner)
			fmt.Printf("  Version: %s\n  Commit:  %s\n", internal.Version, internal.Commit)

			latest, err := internal.CheckLatestVersion()
			if err == nil && latest != "" && latest != "v"+internal.Version && latest != internal.Version {
				fmt.Printf("\nNew version available: %s (current: %s)\n", latest, internal.Version)
				fmt.Println("Run 'cpm upgrade' to update.")
			}
		},
	}
}

func upgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade cpm to the latest version from GitHub Releases",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				// Fallback to default bin dir
				home, _ := os.UserHomeDir()
				return internal.Upgrade(filepath.Join(home, ".local", "bin"))
			}
			return internal.Upgrade(cfg.BinDir)
		},
	}
}

func cloudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Sync settings across machines via git",
		Long:  "Synchronize Claude Code settings, plugins, skills, and commands across devices\nusing a private git repository.",
	}

	cmd.AddCommand(cloudInitCmd())
	cmd.AddCommand(cloudPushCmd())
	cmd.AddCommand(cloudPullCmd())
	cmd.AddCommand(cloudStatusCmd())
	cmd.AddCommand(cloudRemoteCmd())

	return cmd
}

func cloudInitCmd() *cobra.Command {
	var remote string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize cloud sync repo",
		Long:  "Initialize a local git repo for syncing settings.\nIf --remote points to an existing repo, it will be cloned instead.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return internal.CloudInit(configPath, remote)
		},
	}

	cmd.Flags().StringVar(&remote, "remote", "", "git remote URL (e.g. git@github.com:user/claude-settings.git)")

	return cmd
}

func cloudPushCmd() *cobra.Command {
	var message string

	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push local settings to cloud repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			return internal.CloudPush(configPath, message)
		},
	}

	cmd.Flags().StringVarP(&message, "message", "m", "", "custom commit message")

	return cmd
}

func cloudPullCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Pull settings from cloud repo and apply locally",
		RunE: func(cmd *cobra.Command, args []string) error {
			return internal.CloudPull(configPath, dryRun)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change without applying")

	return cmd
}

func cloudStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show cloud sync status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return internal.CloudStatus(configPath)
		},
	}
}

func cloudRemoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remote <url>",
		Short: "Set or update the git remote URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return internal.CloudRemote(configPath, args[0])
		},
	}
}

func addCmd() *cobra.Command {
	var from string
	var fleet bool
	var login bool
	var authMode string
	var authEnvPairs []string

	cmd := &cobra.Command{
		Use:   "add <email> <alias>",
		Short: "Add a new Claude account profile (composes from [base], or clones a template)",
		Long: "Add a new Claude account as an isolated profile.\n\n" +
			"With --auth-mode oauth|api_key (design spec §3/§4), composes the new profile\n" +
			"from [base] + a declared [profiles.<alias>.auth] -- pass --auth-env\n" +
			"KEY=VALUE (repeatable) for the carrier env (e.g. ANTHROPIC_API_KEY=${VAR}).\n" +
			"This is the schema cpm sync/cpm doctor understand; TemplateProfileName\n" +
			"selection is not used on this path.\n\n" +
			"Without --auth-mode, clones args/env from a Max template (the legacy path:\n" +
			"fleet.default_template, or the first Max profile, or --from).\n\n" +
			"Either way: appends to config.toml, sets up the profile dir + launcher, then\n" +
			"runs 'claude auth login --email <email>' (interactive TTY only; --login=false\n" +
			"or --fleet/SSH just prints it) and verifies via 'claude auth status'.\n" +
			"Credentials are never copied. --fleet (legacy path only) also adds the\n" +
			"account on every peer.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			email, alias := args[0], args[1]
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			loginNow := login && isInteractiveTTY()

			if authMode != "" {
				if fleet {
					return fmt.Errorf("--auth-mode with --fleet is not yet supported; add locally on each peer for now")
				}
				authEnv, err := parseAuthEnvPairs(authEnvPairs)
				if err != nil {
					return err
				}
				return internal.AddProfileWithAuth(cfg, configPath, email, alias, authMode, authEnv, loginNow)
			}
			if fleet {
				return internal.AddProfileToFleet(cfg, configPath, email, alias, from, loginNow)
			}
			return internal.AddProfile(cfg, configPath, email, alias, from, loginNow)
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "template profile to clone args/env from (legacy path; default: fleet.default_template or first Max profile)")
	cmd.Flags().BoolVar(&fleet, "fleet", false, "also add this account on every configured fleet peer over SSH (legacy --from path only)")
	cmd.Flags().BoolVar(&login, "login", true, "run 'claude auth login' after add when stdin is a TTY (--login=false to skip; auto-skipped under --fleet/SSH)")
	cmd.Flags().StringVar(&authMode, "auth-mode", "", "compose from [base]+auth instead of cloning a template: \"oauth\" or \"api_key\"")
	cmd.Flags().StringArrayVar(&authEnvPairs, "auth-env", nil, "KEY=VALUE for the profile's [auth.env] (repeatable; requires --auth-mode)")
	return cmd
}

// parseAuthEnvPairs turns repeated --auth-env KEY=VALUE flags into a map, for
// AddProfileWithAuth's authEnv parameter.
func parseAuthEnvPairs(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--auth-env %q: want KEY=VALUE", p)
		}
		out[k] = v
	}
	return out, nil
}

// isInteractiveTTY reports whether stdin is a terminal, so an interactive browser
// sign-in can run. False under --fleet/SSH/CI, where the add flow prints the
// sign-in command instead of launching it.
func isInteractiveTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func fleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Manage the multi-machine profile fleet (peers over SSH)",
		Long:  "Reconcile the set of account profiles across machines declared under\n[fleet.peers] in config.toml. Each machine materializes profiles from its\nown template, so OS/filesystem conventions are respected automatically.",
	}
	cmd.AddCommand(fleetStatusCmd())
	cmd.AddCommand(fleetSyncCmd())
	cmd.AddCommand(fleetCredsCmd())
	return cmd
}

func channelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "Message a running Claude session by profile alias",
		Long: "A Claude Code channel is an MCP server the session spawns, listening on a\n" +
			"local port; messaging a session is an HTTP POST to that port. Each profile\n" +
			"gets one stable port DERIVED from config (never stored), so any process that\n" +
			"reads the same config can reach any session -- including across accounts.\n\n" +
			"The target session must be running with its channel loaded; a channel does\n" +
			"not exist while the session is down, and a message sent then is lost.",
	}
	cmd.AddCommand(channelSendCmd())
	cmd.AddCommand(channelStatusCmd())
	cmd.AddCommand(channelInstallCmd())
	cmd.AddCommand(channelServeCmd())
	return cmd
}

func channelInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <alias>",
		Short: "Register the channel in a profile so its sessions can receive messages",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			alias := args[0]
			base := internal.ProfilesBaseDir(configPath)
			if err := internal.ChannelInstall(cfg, base, alias); err != nil {
				return err
			}
			port, _ := internal.ChannelPort(cfg, alias)
			fmt.Printf("registered %q in profile %s (http://127.0.0.1:%d/mcp)\n",
				internal.ChannelServerName, alias, port)
			fmt.Printf("\nStart the server:  cpm channel serve %s\n", alias)
			fmt.Printf("Then launch a session with the channel loaded:\n")
			fmt.Printf("  claude-%s --dangerously-load-development-channels server:%s\n",
				alias, internal.ChannelServerName)
			return nil
		},
	}
}

func channelServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve <alias>",
		Short: "Run the channel server for a profile on its derived port",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			alias := args[0]
			port, err := internal.ChannelPort(cfg, alias)
			if err != nil {
				return err
			}
			script, err := internal.ChannelServerScript()
			if err != nil {
				return err
			}
			fmt.Printf("starting channel %q for %s on 127.0.0.1:%d\n", internal.ChannelServerName, alias, port)
			c := exec.Command("node", script)
			c.Env = append(os.Environ(),
				fmt.Sprintf("PORT=%d", port),
				fmt.Sprintf("CHANNEL_NAME=%s", internal.ChannelServerName),
			)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			return c.Run()
		},
	}
}

func channelSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <alias> <message>",
		Short: "Push a message into the named profile's running session",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			alias := args[0]
			msg := strings.Join(args[1:], " ")
			if err := internal.SendToProfile(cfg, alias, msg); err != nil {
				return err
			}
			port, _ := internal.ChannelPort(cfg, alias)
			fmt.Printf("sent to %s (127.0.0.1:%d)\n", alias, port)
			return nil
		},
	}
}

func channelStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show each profile's channel port and whether anything is bound",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			states := internal.ChannelStatus(cfg)
			if len(states) == 0 {
				fmt.Println("no profiles configured")
				return nil
			}
			for _, s := range states {
				mark := "-"
				note := "no session listening"
				if s.Listening {
					mark = "*"
					note = "listening"
				}
				fmt.Printf("  %s %-10s %d  %s\n", mark, s.Alias, s.Port, note)
			}
			fmt.Println("\nA port bound with no session running is an orphaned channel subprocess.")
			return nil
		},
	}
}

func fleetStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show peer reachability and profile-set diffs",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			return internal.FleetStatus(cfg, configPath)
		},
	}
}

func fleetSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Reconcile the profile set across all fleet machines",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			return internal.FleetSync(cfg, configPath)
		},
	}
}

// fleetCredsCmd is the `cpm fleet creds` parent: three verbs under the
// existing `fleet` group, deliberately NOT a top-level `cpm creds` (which
// would sit confusingly beside the existing top-level `cpm credentials` —
// local-only status, unrelated). With no subcommand it behaves as `status`.
func fleetCredsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "creds [alias...]",
		Short: "Sync .credentials.json across fleet peers (explicit, one-shot; never automatic)",
		Long: "Move a Claude account's OAuth credentials between this machine and a fleet\n" +
			"peer over an SSH stdin/stdout pipe -- never through argv, never through the\n" +
			"git-backed cloud channel. With no subcommand, `cpm fleet creds` behaves as\n" +
			"`cpm fleet creds status`.\n\n" +
			"This is a one-shot handoff, not replication: each account has a \"home\"\n" +
			"machine at any moment, and push/pull moves it. Two machines sharing one\n" +
			"refresh-token chain WILL race the next time either refreshes (access tokens\n" +
			"live ~8h) -- whichever refreshes first rotates the shared refresh token dead\n" +
			"and the other machine's session silently breaks. Recovery is a re-push/pull\n" +
			"from whichever machine still holds a live chain, not a browser login. Run\n" +
			"`cpm fleet creds status` first to see fingerprint divergence before it bites.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			return internal.FleetCredsStatus(cfg, configPath, args, internal.CredSyncOpts{})
		},
	}
	cmd.AddCommand(fleetCredsStatusCmd())
	cmd.AddCommand(fleetCredsPushCmd())
	cmd.AddCommand(fleetCredsPullCmd())
	cmd.AddCommand(fleetCredsVerifyCmd())
	return cmd
}

func fleetCredsStatusCmd() *cobra.Command {
	var peers []string
	cmd := &cobra.Command{
		Use:   "status [alias...]",
		Short: "Read-only credential matrix: local vs every peer, per profile",
		Long: "Prints, per profile alias, this machine's credential state and every\n" +
			"reachable peer's, plus a lineage fingerprint (fp) -- a 12-hex-char prefix of\n" +
			"sha256(refreshToken), never the secret itself: a 48-bit prefix of SHA-256\n" +
			"over a high-entropy token is not invertible. Same fp on two machines means\n" +
			"the same token chain (safe); different fp means two independent logins\n" +
			"racing (the fight condition) -- flagged as DIVERGENT LINEAGE. Comparisons use\n" +
			"the server-issued expiresAt, never mtime (immune to clock skew between\n" +
			"machines). Never writes anything, local or remote.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			return internal.FleetCredsStatus(cfg, configPath, args, internal.CredSyncOpts{Peers: peers})
		},
	}
	cmd.Flags().StringArrayVar(&peers, "peer", nil, "narrow to this peer (repeatable)")
	return cmd
}

func fleetCredsPushCmd() *cobra.Command {
	var peers []string
	var all, yes, force, dryRun, includeMCP bool
	cmd := &cobra.Command{
		Use:   "push [alias...]",
		Short: "Push local credentials to peer(s) -- \"/login here, propagate there\"",
		Long: "Sends this machine's .credentials.json for the given profile(s) to every\n" +
			"reachable peer (or --peer, repeatable, to narrow), over an SSH stdin pipe\n" +
			"(umask 077; cat > tmp && chmod 600 && mv -f) -- never argv, never scp, never\n" +
			"base64 on the wire. Only claudeAiOauth is overwritten on the peer; the\n" +
			"peer's own mcpOAuth grants and unknown top-level keys are preserved unless\n" +
			"--include-mcp is passed.\n\n" +
			"Refuses rather than half-working: a peer whose session holds a strictly\n" +
			"NEWER token, or a source whose refresh token has already expired, is refused\n" +
			"unless you pass --force (distinct from --yes on purpose); a peer missing the\n" +
			"profile directory is refused with a `cpm fleet sync` hint; the shared\n" +
			"\"default\"/~/.claude identity is always refused; pushing to a Windows peer is\n" +
			"never supported (use `cpm fleet creds pull` there instead). --dry-run prints\n" +
			"the full plan and writes nothing. --yes is required when stdin is not a\n" +
			"terminal (dispatch/background), otherwise the command errors instead of\n" +
			"hanging on a prompt.\n\n" +
			"Exactly one of alias args or --all is required.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			return internal.FleetCredsPush(cfg, configPath, args, internal.CredSyncOpts{
				Peers: peers, All: all, Yes: yes, Force: force, DryRun: dryRun, IncludeMCP: includeMCP,
			})
		},
	}
	cmd.Flags().StringArrayVar(&peers, "peer", nil, "push only to this peer (repeatable; default: every reachable peer)")
	cmd.Flags().BoolVar(&all, "all", false, "every locally-authenticated profile that passes the guards (mutually exclusive with alias args; required together, one or the other)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "assume yes to the routine overwrite prompt; required when stdin is not a TTY")
	cmd.Flags().BoolVar(&force, "force", false, "override a hard refusal (newer peer, dead source token, MCP clobber) -- distinct from --yes")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the full plan and exit 0; no SSH writes")
	cmd.Flags().BoolVar(&includeMCP, "include-mcp", false, "also transport mcpOAuth (default off -- would clobber the peer's own third-party MCP grants)")
	return cmd
}

func fleetCredsPullCmd() *cobra.Command {
	var peers []string
	var all, yes, force, dryRun, includeMCP bool
	cmd := &cobra.Command{
		Use:   "pull [alias...]",
		Short: "Pull credentials from one peer into this machine -- the Windows-destination route",
		Long: "Reads .credentials.json for the given profile(s) from ONE named peer (SSH\n" +
			"stdout capture, stdout/stderr kept separate so a banner line can never splice\n" +
			"into the JSON) and writes it locally with an atomic temp+rename at 0600.\n" +
			"--peer is required whenever more than one peer is configured -- a pull has no\n" +
			"sensible \"merge from all\" semantics.\n\n" +
			"This is the only supported way to bring a Windows machine's profile up to\n" +
			"date from a peer: `push` TO a Windows peer is refused outright (piping data on\n" +
			"stdin while also passing a PowerShell script through two shells' quoting\n" +
			"layers has a silent-corruption failure mode on a credentials file), so\n" +
			"pushing FROM Windows and pulling ON Windows are the two routes into a Windows\n" +
			"box. Same guards as push (newer local copy, dead source, MCP clobber, missing\n" +
			"local profile dir, shared identity, --yes required off a TTY).\n\n" +
			"Exactly one of alias args or --all is required.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			return internal.FleetCredsPull(cfg, configPath, args, internal.CredSyncOpts{
				Peers: peers, All: all, Yes: yes, Force: force, DryRun: dryRun, IncludeMCP: includeMCP,
			})
		},
	}
	cmd.Flags().StringArrayVar(&peers, "peer", nil, "pull from this peer (required when more than one peer is configured)")
	cmd.Flags().BoolVar(&all, "all", false, "every profile configured locally (mutually exclusive with alias args)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "assume yes to the routine overwrite prompt; required when stdin is not a TTY")
	cmd.Flags().BoolVar(&force, "force", false, "override a hard refusal (newer local copy, dead source token, MCP clobber) -- distinct from --yes")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the full plan and exit 0; no local writes")
	cmd.Flags().BoolVar(&includeMCP, "include-mcp", false, "also transport mcpOAuth (default off)")
	return cmd
}

func fleetCredsVerifyCmd() *cobra.Command {
	var peer string
	cmd := &cobra.Command{
		Use:   "verify <alias>",
		Short: "Run a real headless auth check on a peer to prove a pushed file actually works",
		Long: "Runs `claude-<alias> -p \"reply with the single word OK\" --output-format\n" +
			"text` on the named peer over non-interactive SSH -- no GUI, no keychain\n" +
			"unlock available. A response proves the profile's .credentials.json file\n" +
			"alone is authenticating that session: cpm never reads, writes, or unlocks\n" +
			"the macOS Keychain (v1 scope is file-store only), so this is the concrete,\n" +
			"live check that the pushed file -- not a stale Keychain item -- is what the\n" +
			"peer's session actually used.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if peer == "" {
				return fmt.Errorf("--peer is required")
			}
			cfg, err := internal.LoadConfig(configPath)
			if err != nil {
				return err
			}
			return internal.FleetCredsVerify(cfg, configPath, peer, args[0])
		},
	}
	cmd.Flags().StringVar(&peer, "peer", "", "peer to verify against (required)")
	return cmd
}

func sortedKeys(m map[string]*internal.Profile) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
