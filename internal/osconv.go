package internal

import (
	"runtime"
	"strings"
)

// OS identifiers used across the fleet. These match runtime.GOOS values.
const (
	OSWindows = "windows"
	OSDarwin  = "darwin"
	OSLinux   = "linux"
)

// CurrentOS returns this machine's OS identifier (runtime.GOOS).
func CurrentOS() string { return runtime.GOOS }

// windowsOnlyEnvKeys are environment variables that are only meaningful on
// Windows and must be dropped when a profile block is materialized for a
// non-Windows peer.
var windowsOnlyEnvKeys = map[string]bool{
	"CLAUDE_CODE_USE_POWERSHELL_TOOL": true,
}

// TranslateEnvForOS rewrites a profile's env map so its path values and
// OS-only keys are correct for targetOS. It is a best-effort fallback used only
// when a peer has no local Max template to clone from; the primary fleet path
// materializes profiles from each peer's own template, which needs no
// translation. The input map is not modified; a new map is returned.
//
// Path convention translation:
//   - Windows form uses ${USERPROFILE} + backslash separators.
//   - Unix form (darwin/linux) uses ${HOME} + forward-slash separators.
//
// Only values that reference a home directory variable are path-translated, so
// non-path values (tokens, URLs, numbers, model ids) pass through untouched.
func TranslateEnvForOS(env map[string]string, targetOS string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if targetOS != OSWindows && windowsOnlyEnvKeys[k] {
			continue // drop Windows-only knobs on unix peers
		}
		out[k] = translatePathValue(v, targetOS)
	}
	return out
}

// translatePathValue converts a single env value between Windows and Unix home
// path conventions when it references a home-directory variable.
func translatePathValue(v, targetOS string) string {
	hasWin := strings.Contains(v, "${USERPROFILE}")
	hasUnix := strings.Contains(v, "${HOME}")
	if !hasWin && !hasUnix {
		return v // not a home-anchored path; leave as-is
	}

	if targetOS == OSWindows {
		if hasWin {
			return v
		}
		// ${HOME}/a/b -> ${USERPROFILE}\a\b
		v = strings.ReplaceAll(v, "${HOME}", "${USERPROFILE}")
		return strings.ReplaceAll(v, "/", "\\")
	}

	// target is unix (darwin/linux)
	if hasUnix {
		return v
	}
	// ${USERPROFILE}\a\b -> ${HOME}/a/b
	v = strings.ReplaceAll(v, "${USERPROFILE}", "${HOME}")
	return strings.ReplaceAll(v, "\\", "/")
}
