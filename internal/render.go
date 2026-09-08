package internal

import "fmt"

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
//	                ⊕ base.env.<goos>       (right-biased)
//	                ⊕ profile.auth.env      (the ONLY place auth material enters)
//	                ⊕ profile.env           (declared delta)
//
// goos selects the [base.env.<goos>] overlay (pass runtime.GOOS in production
// code; a literal string in tests) so this stays a pure, filesystem-free,
// cross-platform-testable function -- no global state, no I/O.
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
