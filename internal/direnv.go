package internal

import "fmt"

// GenerateDirenvSnippet returns the .envrc snippet for a profile. direnv has no
// first-class Windows/PowerShell story, so for PowerShell we print a clear note
// pointing at `cpm use` instead of emitting bash that would not run.
func GenerateDirenvSnippet(name, profileDir string, shell Shell) string {
	if shell == ShellPowerShell {
		return fmt.Sprintf(`# direnv is not supported on Windows / PowerShell.
# Switch to this profile in the current session instead:
#   cpm use %s --shell powershell | Invoke-Expression
`, name)
	}
	return fmt.Sprintf(`# Claude Code profile: %s
# Add this to your .envrc file
export CLAUDE_CONFIG_DIR="%s"
export CLAUDE_PROFILE="%s"
`, name, profileDir, name)
}
