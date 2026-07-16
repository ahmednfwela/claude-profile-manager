package internal

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// runBypass lists the claude subcommands that must be passed straight through
// without the profile's --add-dir/--model/--args decoration. This is the
// single source of truth: wrapper_unix.go's bypassCasePattern() renders it as
// the bash case pattern, so the two paths cannot silently drift apart again
// (a hand-copied literal there once did, missing every subcommand added here
// after the initial cut).
//
// This must cover EVERY claude subcommand, including the hidden daemon-session
// ones (attach/logs/stop/respawn/daemon): claude's CLI does not parse flags
// placed before a subcommand, so a decorated invocation like
// `claude --dangerously-skip-permissions stop <id>` is treated as a PROMPT and
// silently runs a model turn instead of the subcommand.
var runBypass = map[string]bool{
	"mcp": true, "auth": true, "doctor": true, "install": true,
	"setup-token": true, "update": true, "upgrade": true, "agents": true,
	"auto-mode": true, "plugin": true, "plugins": true,
	"attach": true, "logs": true, "stop": true, "respawn": true,
	"daemon": true, "gateway": true, "project": true,
	"remote-control": true, "ultrareview": true,
}

// BuildRunInvocation assembles everything needed to launch claude for a profile:
// the resolved claude binary path, the full argv (argv[0] is the binary path),
// and the environment. It centralizes the env filtering, ${VAR} expansion, and
// subcommand bypass so both `cpm run` and the generated launchers share one
// robust code path instead of a brittle shell script.
func BuildRunInvocation(name, profileDir string, p *Profile, claudeArgs []string) (string, []string, []string, error) {
	env := os.Environ()
	// Drop inherited CLAUDE_*/ANTHROPIC_* vars so the profile starts clean.
	filtered := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, "CLAUDE_") && !strings.HasPrefix(e, "ANTHROPIC_") {
			filtered = append(filtered, e)
		}
	}
	filtered = append(filtered,
		"CLAUDE_CONFIG_DIR="+profileDir,
		"CLAUDE_PROFILE="+name,
	)
	for _, k := range sortedEnvKeys(p.Env) {
		// os.ExpandEnv lets a config value reference another env var, e.g.
		// ANTHROPIC_AUTH_TOKEN = "${GLM_AUTH_TOKEN}" — keeps secrets out of files.
		filtered = append(filtered, k+"="+os.ExpandEnv(p.Env[k]))
	}

	claudePath, err := exec.LookPath("claude") // resolves claude.exe via PATHEXT on Windows
	if err != nil {
		return "", nil, nil, fmt.Errorf("claude not found on PATH")
	}

	argv := []string{claudePath}
	if len(claudeArgs) > 0 && runBypass[claudeArgs[0]] {
		return claudePath, append(argv, claudeArgs...), filtered, nil
	}
	for _, d := range p.AddDirs {
		argv = append(argv, "--add-dir", ExpandPath(d))
	}
	if p.Model != "" && !hasModelFlag(claudeArgs) {
		argv = append(argv, "--model", p.Model)
	}
	// Profile-level extra flags (e.g. --dangerously-skip-permissions). Injected
	// only on real claude launches — the bypass path above returns before here,
	// so management subcommands (mcp/auth/doctor/...) never receive them.
	argv = append(argv, p.Args...)
	return claudePath, append(argv, claudeArgs...), filtered, nil
}

func hasModelFlag(args []string) bool {
	for _, a := range args {
		if a == "--model" || strings.HasPrefix(a, "--model=") {
			return true
		}
	}
	return false
}

func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
