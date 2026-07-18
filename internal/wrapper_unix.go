//go:build !windows

package internal

import (
	"fmt"
	"sort"
	"strings"
)

// bypassCasePattern renders runBypass (defined in exec_run.go — the single
// source of truth also used by BuildRunInvocation/`cpm run`) as a sorted
// "a|b|c" bash case pattern. Deriving it from the map instead of hand-copying
// a second literal is the fix: the bash list previously stood still at the
// original 11 subcommands while runBypass grew to include the hidden
// daemon-session ones (attach/logs/stop/respawn/daemon) plus
// gateway/project/remote-control/ultrareview (fix(run) 2026-07-15), so a
// decorated `claude-<profile> stop <id>` was parsed as a PROMPT instead of
// bypassing to the real subcommand.
func bypassCasePattern() string {
	names := make([]string, 0, len(runBypass))
	for name := range runBypass {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// shellQuote wraps s in single quotes for safe inclusion in the generated
// bash script, escaping any embedded single quotes. profile.Args are baked
// into the script as literals (unlike "$@", which bash forwards at runtime),
// so an unquoted arg containing whitespace or a shell metacharacter would
// either split into multiple words or break the script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// LauncherFileName is the on-PATH launcher name for a profile.
func LauncherFileName(name string) string { return "claude-" + name }

// GenerateLauncher returns the launcher script body for a profile. On Unix this
// is the self-contained bash wrapper; cpmPath is unused here.
func GenerateLauncher(name, profileDir string, profile *Profile, cpmPath string) string {
	return GenerateWrapper(name, profileDir, profile)
}

func GenerateWrapper(name string, profileDir string, profile *Profile) string {
	var b strings.Builder

	b.WriteString("#!/usr/bin/env bash\n")
	b.WriteString(marker + "\n")
	b.WriteString(fmt.Sprintf("# Profile: %s — %s\n", name, profile.Description))
	b.WriteString("set -euo pipefail\n\n")

	// Unset inherited CLAUDE_*/ANTHROPIC_* env vars
	b.WriteString("# Unset inherited CLAUDE_*/ANTHROPIC_* env vars to avoid interference\n")
	b.WriteString("while IFS= read -r varname; do\n")
	b.WriteString("  unset \"$varname\"\n")
	b.WriteString("done < <(compgen -v | grep -E \"^(CLAUDE_|ANTHROPIC_)\")\n\n")

	b.WriteString(fmt.Sprintf("export CLAUDE_CONFIG_DIR=\"%s\"\n", profileDir))
	b.WriteString(fmt.Sprintf("export CLAUDE_PROFILE=\"%s\"\n", name))

	// Custom env vars
	if len(profile.Env) > 0 {
		keys := make([]string, 0, len(profile.Env))
		for k := range profile.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(fmt.Sprintf("export %s=\"%s\"\n", k, profile.Env[k]))
		}
	}

	b.WriteString("\n")

	// Subcommands bypass
	b.WriteString(fmt.Sprintf("case \"${1:-}\" in\n  %s) exec claude \"$@\" ;;\nesac\n\n", bypassCasePattern()))

	// Model handling
	if profile.Model != "" {
		b.WriteString("# Default model (overridden if --model is passed on command line)\n")
		b.WriteString("has_model=false\n")
		b.WriteString("for arg in \"$@\"; do\n")
		b.WriteString("  case \"$arg\" in\n")
		b.WriteString("    --model|--model=*) has_model=true; break ;;\n")
		b.WriteString("  esac\n")
		b.WriteString("done\n\n")
	}

	// Build exec command
	var cmdParts []string
	cmdParts = append(cmdParts, "exec claude")
	for _, d := range profile.AddDirs {
		expanded := ExpandPath(d)
		cmdParts = append(cmdParts, fmt.Sprintf("--add-dir \"%s\"", expanded))
	}

	if profile.Model != "" {
		cmdParts = append(cmdParts, fmt.Sprintf("$([ \"$has_model\" = false ] && echo '--model %s')", profile.Model))
	}

	// Profile-level extra flags (e.g. --dangerously-skip-permissions), mirroring
	// BuildRunInvocation's ordering in exec_run.go: add-dirs, model, args, then
	// the caller's own argv. The bypass case above returns before this point,
	// so bypassed subcommands never receive them.
	for _, a := range profile.Args {
		cmdParts = append(cmdParts, shellQuote(a))
	}

	cmdParts = append(cmdParts, "\"$@\"")
	b.WriteString(strings.Join(cmdParts, " \\\n  ") + "\n")

	return b.String()
}
