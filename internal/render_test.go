package internal

import (
	"reflect"
	"sort"
	"testing"
)

// sortedStrings is a tiny test helper so slice-equality assertions below
// don't depend on map/merge iteration order.
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestRenderProfileBaseAndDeltaMerge(t *testing.T) {
	cfg := &Config{
		Base: &Base{
			Model: "sonnet",
			Args:  []string{"--dangerously-skip-permissions"},
			Env: map[string]any{
				"ENABLE_TOOL_SEARCH": "true",
				"os_overlay": map[string]any{
					"windows": map[string]any{
						"CLAUDE_CODE_USE_POWERSHELL_TOOL": "1",
					},
				},
			},
		},
		Profiles: map[string]*Profile{
			"alibaba1": {
				Args: []string{"--extra-flag"},
				Auth: &ProfileAuth{
					Mode: "api_key",
					Env: map[string]string{
						"ANTHROPIC_BASE_URL": "${BDAYA_ALIBABA_BASE_URL}",
						"ANTHROPIC_API_KEY":  "${BDAYA_ALIBABA_API_KEY}",
					},
				},
				Env: map[string]string{
					"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
				},
			},
		},
	}

	rendered, err := RenderProfile(cfg, "alibaba1", "linux")
	if err != nil {
		t.Fatalf("RenderProfile: %v", err)
	}

	wantArgs := []string{"--dangerously-skip-permissions", "--extra-flag", "--model", "sonnet"}
	if !reflect.DeepEqual(rendered.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", rendered.Args, wantArgs)
	}
	if rendered.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet (inherited from base)", rendered.Model)
	}

	wantEnv := map[string]string{
		"ENABLE_TOOL_SEARCH":                       "true",
		"ANTHROPIC_BASE_URL":                       "${BDAYA_ALIBABA_BASE_URL}",
		"ANTHROPIC_API_KEY":                        "${BDAYA_ALIBABA_API_KEY}",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
	if !reflect.DeepEqual(rendered.Env, wantEnv) {
		t.Errorf("Env = %v, want %v", rendered.Env, wantEnv)
	}
	// The windows-only overlay key must NOT leak into a linux render.
	if _, ok := rendered.Env["CLAUDE_CODE_USE_POWERSHELL_TOOL"]; ok {
		t.Error("linux render must not include the windows overlay key")
	}
}

func TestRenderProfilePerOSOverlay(t *testing.T) {
	cfg := &Config{
		Base: &Base{
			Env: map[string]any{
				"COMMON_KEY": "common",
				"os_overlay": map[string]any{
					"windows": map[string]any{
						"CLAUDE_CODE_USE_POWERSHELL_TOOL": "1",
					},
					"darwin": map[string]any{
						"SOME_MAC_ONLY_KEY": "mac",
					},
				},
			},
		},
		Profiles: map[string]*Profile{"p": {}},
	}

	winRendered, err := RenderProfile(cfg, "p", "windows")
	if err != nil {
		t.Fatalf("RenderProfile(windows): %v", err)
	}
	if winRendered.Env["CLAUDE_CODE_USE_POWERSHELL_TOOL"] != "1" {
		t.Errorf("windows render missing overlay key: %v", winRendered.Env)
	}
	if _, ok := winRendered.Env["SOME_MAC_ONLY_KEY"]; ok {
		t.Error("windows render must not include the darwin overlay key")
	}
	if winRendered.Env["COMMON_KEY"] != "common" {
		t.Error("windows render must still include the common key")
	}

	macRendered, err := RenderProfile(cfg, "p", "darwin")
	if err != nil {
		t.Fatalf("RenderProfile(darwin): %v", err)
	}
	if macRendered.Env["SOME_MAC_ONLY_KEY"] != "mac" {
		t.Errorf("darwin render missing overlay key: %v", macRendered.Env)
	}
	if _, ok := macRendered.Env["CLAUDE_CODE_USE_POWERSHELL_TOOL"]; ok {
		t.Error("darwin render must not include the windows overlay key")
	}
}

// A profile's own Model overrides base.model; an unset profile Model falls
// back to base.model; and when neither sets one, no --model flag is injected.
func TestRenderProfileModelPrecedence(t *testing.T) {
	cfg := &Config{
		Base: &Base{Model: "sonnet"},
		Profiles: map[string]*Profile{
			"inherits": {},
			"pinned":   {Model: "opus"},
			"nobase":   {},
		},
	}

	rendered, err := RenderProfile(cfg, "inherits", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Model != "sonnet" {
		t.Errorf("inherits: Model = %q, want sonnet", rendered.Model)
	}
	if got, want := rendered.Args, []string{"--model", "sonnet"}; !reflect.DeepEqual(got, want) {
		t.Errorf("inherits: Args = %v, want %v", got, want)
	}

	rendered, err = RenderProfile(cfg, "pinned", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Model != "opus" {
		t.Errorf("pinned: Model = %q, want opus (profile override wins)", rendered.Model)
	}

	cfgNoBase := &Config{Profiles: map[string]*Profile{"nobase": {}}}
	rendered, err = RenderProfile(cfgNoBase, "nobase", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Model != "" {
		t.Errorf("nobase: Model = %q, want empty", rendered.Model)
	}
	if len(rendered.Args) != 0 {
		t.Errorf("nobase: Args = %v, want empty (no --model injected when unset)", rendered.Args)
	}
}

// A key set in BOTH [profiles.X.auth.env] and [profiles.X.env] is a
// render-time error -- auth keys are reserved (design spec §3).
func TestRenderProfileAuthKeyCollisionIsAnError(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]*Profile{
			"bad": {
				Auth: &ProfileAuth{
					Mode: "api_key",
					Env:  map[string]string{"ANTHROPIC_API_KEY": "${X}"},
				},
				Env: map[string]string{"ANTHROPIC_API_KEY": "collides"},
			},
		},
	}
	_, err := RenderProfile(cfg, "bad", "linux")
	if err == nil {
		t.Fatal("expected an auth-key collision error, got nil")
	}
}

// ResolveRendered applies base.toml's placeholder contract per machine:
// "~/" -> home (forward-slashed), and the --append-system-prompt-file pair is
// dropped as a unit when its resolved file does not exist. The input is never
// mutated and the [base]-side placeholders are never touched here (see
// TestRenderBaseBlockIsVerbatim for that half of the contract).
func TestResolveRenderedPlaceholders(t *testing.T) {
	in := RenderedProfile{
		Args: []string{
			"--dangerously-skip-permissions",
			"--append-system-prompt-file", "~/.claude-profiles/lane-authority.md",
			"--model", "sonnet",
		},
		Env: map[string]string{
			"CLAUDE_CODE_PLUGIN_CACHE_DIR": "~/.claude/plugins",
			"PLAIN":                        "~not-a-placeholder",
		},
		Model: "sonnet",
	}
	home := `C:\Users\ahmed`

	present := ResolveRendered(in, RenderOptions{Home: home, FileExists: func(string) bool { return true }})
	wantArgs := []string{"--dangerously-skip-permissions", "--append-system-prompt-file", "C:/Users/ahmed/.claude-profiles/lane-authority.md", "--model", "sonnet"}
	if !reflect.DeepEqual(present.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", present.Args, wantArgs)
	}
	if present.Env["CLAUDE_CODE_PLUGIN_CACHE_DIR"] != "C:/Users/ahmed/.claude/plugins" || present.Env["PLAIN"] != "~not-a-placeholder" {
		t.Errorf("Env = %v", present.Env)
	}
	if present.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet", present.Model)
	}

	var asked []string
	absent := ResolveRendered(in, RenderOptions{Home: home, FileExists: func(p string) bool { asked = append(asked, p); return false }})
	wantArgs = []string{"--dangerously-skip-permissions", "--model", "sonnet"}
	if !reflect.DeepEqual(absent.Args, wantArgs) {
		t.Errorf("Args with a missing grant file = %v, want %v (flag AND operand dropped together)", absent.Args, wantArgs)
	}
	if !reflect.DeepEqual(asked, []string{"C:/Users/ahmed/.claude-profiles/lane-authority.md"}) {
		t.Errorf("FileExists must be asked about the RESOLVED path only, got %v", asked)
	}

	eqDropped := ResolveRendered(RenderedProfile{Args: []string{"--append-system-prompt-file=~/g.md", "x"}},
		RenderOptions{Home: "/home/u", FileExists: func(string) bool { return false }})
	if !reflect.DeepEqual(eqDropped.Args, []string{"x"}) {
		t.Errorf("'=' form must be dropped as one token, got %v", eqDropped.Args)
	}
	eqKept := ResolveRendered(RenderedProfile{Args: []string{"--append-system-prompt-file=~/g.md"}}, RenderOptions{Home: "/home/u"})
	if !reflect.DeepEqual(eqKept.Args, []string{"--append-system-prompt-file=/home/u/g.md"}) {
		t.Errorf("'=' form must resolve in place, got %v", eqKept.Args)
	}

	// No home known: placeholders are left alone rather than rooted at "/".
	noHome := ResolveRendered(RenderedProfile{Args: []string{"~/x"}}, RenderOptions{})
	if !reflect.DeepEqual(noHome.Args, []string{"~/x"}) {
		t.Errorf("empty Home must leave placeholders untouched, got %v", noHome.Args)
	}
	// The input must not be mutated.
	if in.Args[2] != "~/.claude-profiles/lane-authority.md" || in.Env["CLAUDE_CODE_PLUGIN_CACHE_DIR"] != "~/.claude/plugins" {
		t.Error("ResolveRendered mutated its input")
	}
}

func TestRenderProfileUnknownProfile(t *testing.T) {
	cfg := &Config{Profiles: map[string]*Profile{}}
	if _, err := RenderProfile(cfg, "ghost", "linux"); err == nil {
		t.Error("expected an error for an unknown profile")
	}
}

func TestRenderProfileDeterministicEnvOrdering(t *testing.T) {
	// RenderedProfile.Env is a map (order-free by definition), but the args
	// slice must be stable across repeated calls -- required for cpm sync's
	// diff to be non-flaky.
	cfg := &Config{
		Base:     &Base{Args: []string{"--a", "--b"}},
		Profiles: map[string]*Profile{"p": {Args: []string{"--c"}}},
	}
	first, err := RenderProfile(cfg, "p", "linux")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := RenderProfile(cfg, "p", "linux")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(sortedStrings(first.Args), sortedStrings(again.Args)) || len(first.Args) != len(again.Args) {
			t.Fatalf("Args not stable across calls: %v then %v", first.Args, again.Args)
		}
	}
}
