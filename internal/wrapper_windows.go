//go:build windows

package internal

import "fmt"

// LauncherFileName is the on-PATH launcher name for a profile. On Windows it is
// a .cmd so a bare `claude-<name>` resolves via PATHEXT.
func LauncherFileName(name string) string { return "claude-" + name + ".cmd" }

// GenerateLauncher returns a thin .cmd that delegates entirely to `cpm run`.
// All env/token/argv handling lives in Go (cpm run); the on-PATH launcher holds
// no secrets — only the delegation line. profileDir and profile are unused here
// because cpm run reads them from config at launch time.
func GenerateLauncher(name, profileDir string, profile *Profile, cpmPath string) string {
	// CRLF line endings; %* forwards all args; absolute cpm path for robustness.
	// ":: " keeps the marker line a valid batch comment while staying greppable
	// by CleanupStaleScripts (which matches on the marker substring).
	return "@echo off\r\n" +
		":: " + marker + "\r\n" +
		fmt.Sprintf("\"%s\" run %s %%*\r\n", cpmPath, name)
}
