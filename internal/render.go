package internal

import (
	"fmt"
	"strings"
)

// RenderedProfile is RenderProfile's pure output: the effective args/env/model
// for a profile after merging [base] (+ its per-GOOS overlay), the profile's
// [auth] material, and its own declared env/args delta. Design spec §3.
type RenderedProfile struct {
	Args  []string
	Env   map[string]string
	Model string
}

// RenderProfile computes a profile's fully-merged args/env/model:
//
//	effective_model = profile.model  if profile.model != ""  else  base.model
//	effective_args  = base.args ++ profile.args
//	                ++ ["--model", effective_model]  (only if effective_model != "")
//	effective_env   = base.env
//	                ⊕ base.env.os_overlay.<goos>   (right-biased)
//	                ⊕ profile.auth.env             (the ONLY place auth material enters)
//	                ⊕ profile.env                  (declared delta)
//
// goos selects the os_overlay.<goos> overlay (pass runtime.GOOS in production
// code; a literal string in tests) so this stays a pure, filesystem-free,
// cross-platform-testable function -- no global state, no I/O. The output
// still carries base.toml's `~/...` placeholders verbatim; ResolveRendered
// turns them into this machine's paths.
//
// Returns an error if cfg has no such profile, or if a key is declared in
// BOTH profile.auth.env and profile.env: auth keys are reserved, and a
// collision there is a render-time error, never a silent last-writer-wins
// (design spec §3; the exact reserved-key allowlist is design spec OQ-2,
// still open -- this collision check does not depend on that allowlist, it
// simply refuses ANY key that both tables declare).
func RenderProfile(cfg *Config, name, goos string) (RenderedProfile, error) {
	p, ok := cfg.Profiles[name]
	if !ok {
		return RenderedProfile{}, fmt.Errorf("unknown profile %q", name)
	}
	if p == nil {
		p = &Profile{}
	}

	base := cfg.Base
	common := base.BaseEnvCommon()
	overlay := base.BaseEnvOverlay(goos)

	var authEnv map[string]string
	if p.Auth != nil {
		authEnv = p.Auth.Env
	}
	for k := range authEnv {
		if _, collide := p.Env[k]; collide {
			return RenderedProfile{}, fmt.Errorf(
				"profile %q: %q is declared in both [profiles.%s.auth.env] and [profiles.%s.env] -- auth keys are reserved, remove it from one",
				name, k, name, name)
		}
	}

	env := make(map[string]string, len(common)+len(overlay)+len(authEnv)+len(p.Env))
	for k, v := range common {
		env[k] = v
	}
	for k, v := range overlay {
		env[k] = v
	}
	for k, v := range authEnv {
		env[k] = v
	}
	for k, v := range p.Env {
		env[k] = v
	}

	effectiveModel := p.Model
	if effectiveModel == "" && base != nil {
		effectiveModel = base.Model
	}

	var baseArgs []string
	if base != nil {
		baseArgs = base.Args
	}
	args := make([]string, 0, len(baseArgs)+len(p.Args)+2)
	args = append(args, baseArgs...)
	args = append(args, p.Args...)
	if effectiveModel != "" {
		args = append(args, "--model", effectiveModel)
	}

	return RenderedProfile{Args: args, Env: env, Model: effectiveModel}, nil
}

// RenderOptions carries the two machine-specific inputs ResolveRendered
// needs. They are injected rather than read from the OS so RenderProfile +
// ResolveRendered stay pure and cross-platform-testable; production callers
// pass os.UserHomeDir() and an os.Stat-backed FileExists.
type RenderOptions struct {
	Home       string
	FileExists func(path string) bool
}

// appendSystemPromptFlag is the claude flag whose file operand base.toml
// declares as a `~/...` placeholder (the owner-written lane-authority grant).
const appendSystemPromptFlag = "--append-system-prompt-file"

// ResolveRendered applies fleet/profiles/base.toml's placeholder contract
// (that file's own PLACEHOLDERS section) to a rendered profile, producing the
// values this machine can actually execute:
//
//   - every arg and env value beginning with "~/" gets the "~" replaced by
//     opts.Home, forward-slashed ("C:/Users/<u>/..." on Windows), matching
//     how onboard renders the same paths into a first-enrolled config.toml;
//   - an "--append-system-prompt-file <path>" pair (or its "=<path>" form)
//     whose resolved path does not exist is dropped entirely: claude fails
//     hard on a missing prompt file, and that grant file is owner-written,
//     so a machine without it must launch without the flag rather than not
//     launch at all -- the same safety net onboard/lib/steps/profiles.js
//     already enforces at enrollment.
//
// The [base] table itself is never resolved (RenderBaseBlock keeps it
// verbatim so fleet-doctor F10 can compare it to base.toml); resolution is
// a per-machine, per-launch concern and lives here only.
func ResolveRendered(rp RenderedProfile, opts RenderOptions) RenderedProfile {
	exists := opts.FileExists
	if exists == nil {
		exists = func(string) bool { return true }
	}
	out := RenderedProfile{
		Model: rp.Model,
		Args:  make([]string, 0, len(rp.Args)),
		Env:   make(map[string]string, len(rp.Env)),
	}
	for k, v := range rp.Env {
		out.Env[k] = expandHomePlaceholder(v, opts.Home)
	}
	for i := 0; i < len(rp.Args); i++ {
		a := rp.Args[i]
		switch {
		case a == appendSystemPromptFlag && i+1 < len(rp.Args):
			p := expandHomePlaceholder(rp.Args[i+1], opts.Home)
			i++ // the operand is consumed with its flag, kept or dropped together
			if exists(p) {
				out.Args = append(out.Args, a, p)
			}
		case strings.HasPrefix(a, appendSystemPromptFlag+"="):
			p := expandHomePlaceholder(strings.TrimPrefix(a, appendSystemPromptFlag+"="), opts.Home)
			if exists(p) {
				out.Args = append(out.Args, appendSystemPromptFlag+"="+p)
			}
		default:
			out.Args = append(out.Args, expandHomePlaceholder(a, opts.Home))
		}
	}
	return out
}

// expandHomePlaceholder replaces a leading "~/" with home (forward-slashed,
// no trailing slash); any other value is returned unchanged. An empty home
// leaves the placeholder alone rather than producing a path rooted at "/".
//
// Backslashes are converted explicitly rather than via filepath.ToSlash: that
// helper only rewrites the HOST OS separator, so a Windows home rendered on
// Linux/macOS (a fleet-side renderer, a cross-platform test) would keep its
// backslashes and never match the "C:/Users/<u>/..." form the fleet expects.
// The output is the same on every GOOS for the same input, which is what
// makes ResolveRendered a pure function rather than a per-platform one.
func expandHomePlaceholder(v, home string) string {
	if home == "" || !strings.HasPrefix(v, "~/") {
		return v
	}
	return strings.TrimRight(strings.ReplaceAll(home, `\`, "/"), "/") + v[1:]
}
