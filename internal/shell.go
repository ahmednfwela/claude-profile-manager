package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Shell identifies a target shell for the emitted `use`/`hook`/`direnv` output.
type Shell string

const (
	ShellBash       Shell = "bash"
	ShellZsh        Shell = "zsh"
	ShellPowerShell Shell = "powershell"
)

// DefaultShell is the shell assumed when --shell is not given: PowerShell on
// Windows, a POSIX shell elsewhere.
func DefaultShell() Shell {
	if runtime.GOOS == "windows" {
		return ShellPowerShell
	}
	return ShellBash
}

// ParseShell maps a --shell flag value to a Shell, defaulting by OS for "" / "auto".
func ParseShell(s string) (Shell, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return DefaultShell(), nil
	case "bash":
		return ShellBash, nil
	case "zsh":
		return ShellZsh, nil
	case "powershell", "pwsh":
		return ShellPowerShell, nil
	default:
		return "", fmt.Errorf("unsupported shell %q (use bash, zsh, or powershell)", s)
	}
}

// GenerateUseOutput outputs shell commands to switch the current shell to a
// profile. POSIX: eval "$(cpm use <profile>)". PowerShell: cpm use <profile> | Invoke-Expression.
func GenerateUseOutput(name string, profileDir string, profile *Profile, shell Shell) string {
	if shell == ShellPowerShell {
		return useOutputPowerShell(name, profileDir, profile)
	}
	return useOutputPosix(name, profileDir, profile)
}

func useOutputPosix(name, profileDir string, profile *Profile) string {
	var b strings.Builder

	// Unset CLAUDE_*/ANTHROPIC_* vars
	b.WriteString("unset $(env | grep -E '^(CLAUDE_|ANTHROPIC_)' | cut -d= -f1) 2>/dev/null;\n")

	fmt.Fprintf(&b, "export CLAUDE_CONFIG_DIR=\"%s\";\n", profileDir)
	fmt.Fprintf(&b, "export CLAUDE_PROFILE=\"%s\";\n", name)

	for _, k := range sortedEnvKeys(profile.Env) {
		fmt.Fprintf(&b, "export %s=\"%s\";\n", k, os.ExpandEnv(profile.Env[k]))
	}

	fmt.Fprintf(&b, "echo \"Switched to profile: %s\";\n", name)

	return b.String()
}

func useOutputPowerShell(name, profileDir string, profile *Profile) string {
	var b strings.Builder

	// Remove inherited CLAUDE_*/ANTHROPIC_* vars.
	b.WriteString("Get-ChildItem Env: | Where-Object Name -match '^(CLAUDE_|ANTHROPIC_)' | ForEach-Object { Remove-Item \"Env:$($_.Name)\" };\n")

	fmt.Fprintf(&b, "$env:CLAUDE_CONFIG_DIR='%s';\n", profileDir)
	fmt.Fprintf(&b, "$env:CLAUDE_PROFILE='%s';\n", name)

	for _, k := range sortedEnvKeys(profile.Env) {
		fmt.Fprintf(&b, "$env:%s='%s';\n", k, os.ExpandEnv(profile.Env[k]))
	}

	fmt.Fprintf(&b, "Write-Host 'Switched to profile: %s';\n", name)

	return b.String()
}

// CurrentProfile returns the name of the currently active profile from env.
func CurrentProfile() string {
	return os.Getenv("CLAUDE_PROFILE")
}

// CurrentConfigDir returns the currently active CLAUDE_CONFIG_DIR from env.
func CurrentConfigDir() string {
	return os.Getenv("CLAUDE_CONFIG_DIR")
}

// DetectProfileFile walks up from the given directory looking for .claude-profile
func DetectProfileFile(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}

	for {
		candidate := filepath.Join(dir, ".claude-profile")
		if data, err := os.ReadFile(candidate); err == nil {
			name := strings.TrimSpace(string(data))
			if name != "" {
				return name, nil
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("no .claude-profile file found")
}

// GenerateShellHook generates a shell hook function for auto-switching on cd.
func GenerateShellHook(shell Shell) string {
	if shell == ShellPowerShell {
		return shellHookPowerShell()
	}
	return shellHookPosix()
}

func shellHookPosix() string {
	return `# cpm auto-switch hook — add to your .zshrc or .bashrc:
#   eval "$(cpm hook)"
_cpm_auto_switch() {
  local profile_file=""
  local dir="$PWD"
  while [ "$dir" != "/" ]; do
    if [ -f "$dir/.claude-profile" ]; then
      profile_file="$dir/.claude-profile"
      break
    fi
    dir="$(dirname "$dir")"
  done
  if [ -n "$profile_file" ]; then
    local target
    target="$(cat "$profile_file" | tr -d '[:space:]')"
    if [ -n "$target" ] && [ "$target" != "${CLAUDE_PROFILE:-}" ]; then
      eval "$(cpm use "$target" 2>/dev/null)"
      echo "[cpm] using profile: $target"
    fi
  elif [ -n "${CLAUDE_PROFILE:-}" ]; then
    unset CLAUDE_CONFIG_DIR CLAUDE_PROFILE
    unset $(env | grep -E '^(CLAUDE_|ANTHROPIC_)' | cut -d= -f1) 2>/dev/null
    echo "[cpm] profile unset (no .claude-profile found)"
  fi
}
if [ -n "$ZSH_VERSION" ]; then
  autoload -Uz add-zsh-hook
  add-zsh-hook chpwd _cpm_auto_switch
else
  _cpm_original_cd() { builtin cd "$@" && _cpm_auto_switch; }
  alias cd='_cpm_original_cd'
fi
_cpm_auto_switch
`
}

// shellHookPowerShell wraps the prompt function (PowerShell has no chpwd) so the
// active profile auto-switches based on the nearest .claude-profile file.
// Add to $PROFILE:  cpm hook --shell powershell | Out-String | Invoke-Expression
func shellHookPowerShell() string {
	return `# cpm auto-switch hook — add to your $PROFILE:
#   cpm hook --shell powershell | Out-String | Invoke-Expression
function global:_cpm_auto_switch {
  $d = (Get-Location).Path; $f = $null
  while ($d) {
    $c = Join-Path $d '.claude-profile'
    if (Test-Path $c) { $f = $c; break }
    $p = Split-Path $d -Parent; if ($p -eq $d) { break }; $d = $p
  }
  if ($f) {
    $t = (Get-Content $f -Raw).Trim()
    if ($t -and $t -ne $env:CLAUDE_PROFILE) { cpm use $t --shell powershell | Invoke-Expression; Write-Host "[cpm] using profile: $t" }
  } elseif ($env:CLAUDE_PROFILE) {
    Get-ChildItem Env: | Where-Object Name -match '^(CLAUDE_|ANTHROPIC_)' | ForEach-Object { Remove-Item "Env:$($_.Name)" }
    Write-Host "[cpm] profile unset"
  }
}
if (-not $global:__cpm_prompt_saved) { $global:__cpm_prompt_saved = $function:prompt }
function global:prompt { _cpm_auto_switch; if ($global:__cpm_prompt_saved) { & $global:__cpm_prompt_saved } else { "PS $($executionContext.SessionState.Path.CurrentLocation)$('>' * ($nestedPromptLevel + 1)) " } }
`
}

// LinkProfile creates a .claude-profile file in the given directory and adds it to .gitignore.
func LinkProfile(dir, profileName string) error {
	profilePath := filepath.Join(dir, ".claude-profile")
	if err := os.WriteFile(profilePath, []byte(profileName+"\n"), 0o644); err != nil {
		return fmt.Errorf("cannot write .claude-profile: %w", err)
	}

	// Add to .gitignore if it exists and doesn't already contain .claude-profile
	gitignorePath := filepath.Join(dir, ".gitignore")
	if data, err := os.ReadFile(gitignorePath); err == nil {
		lines := strings.Split(string(data), "\n")
		found := false
		for _, line := range lines {
			if strings.TrimSpace(line) == ".claude-profile" {
				found = true
				break
			}
		}
		if !found {
			entry := "\n# Claude profile (cpm)\n.claude-profile\n"
			if err := os.WriteFile(gitignorePath, append(data, []byte(entry)...), 0o644); err != nil {
				return fmt.Errorf("cannot update .gitignore: %w", err)
			}
			fmt.Println("  added .claude-profile to .gitignore")
		}
	}

	return nil
}

// UnlinkProfile removes the .claude-profile file from the given directory.
func UnlinkProfile(dir string) error {
	profilePath := filepath.Join(dir, ".claude-profile")
	if _, err := os.Stat(profilePath); os.IsNotExist(err) {
		return fmt.Errorf("no .claude-profile in current directory")
	}
	return os.Remove(profilePath)
}

func PromptString() string {
	profile := CurrentProfile()
	if profile == "" {
		return ""
	}
	return profile
}
