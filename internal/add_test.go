package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestValidateAlias(t *testing.T) {
	good := []string{"digrum1", "a", "A9", "my-alias", "my_alias", "z9x"}
	for _, g := range good {
		if err := ValidateAlias(g); err != nil {
			t.Errorf("ValidateAlias(%q) = %v, want nil", g, err)
		}
	}
	bad := []string{"", "-bad", "_bad", "has space", "bad/slash", "semi;colon", "$var", "back\\slash", "quote'd"}
	for _, b := range bad {
		if err := ValidateAlias(b); err == nil {
			t.Errorf("ValidateAlias(%q) = nil, want error", b)
		}
	}
}

func TestValidateEmail(t *testing.T) {
	good := []string{"claude1@digrum.com", "a.b+c@sub.example.co", "x_y@z.io"}
	for _, g := range good {
		if err := ValidateEmail(g); err != nil {
			t.Errorf("ValidateEmail(%q) = %v, want nil", g, err)
		}
	}
	bad := []string{"noat", "a@b", "a b@c.com", "a@b.com;rm -rf", "a@b.com`x`", "a@b.com$X", "a@b.com\"x", "with space@x.com"}
	for _, b := range bad {
		if err := ValidateEmail(b); err == nil {
			t.Errorf("ValidateEmail(%q) = nil, want error", b)
		}
	}
}

func TestTomlValue(t *testing.T) {
	cases := map[string]string{
		`${USERPROFILE}\.claude\plugins`: `'${USERPROFILE}\.claude\plugins'`, // backslash -> single-quoted literal
		"true":                           `"true"`,
		"128000":                         `"128000"`,
		"https://api.z.ai/api/anthropic": `"https://api.z.ai/api/anthropic"`,
	}
	for in, want := range cases {
		if got := tomlValue(in); got != want {
			t.Errorf("tomlValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderProfileBlockRoundTrip(t *testing.T) {
	env := map[string]string{
		"ENABLE_TOOL_SEARCH":              "true",
		"CLAUDE_CODE_USE_POWERSHELL_TOOL": "1",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS":   "128000",
		"CLAUDE_CODE_PLUGIN_CACHE_DIR":    `${USERPROFILE}\.claude\plugins`,
	}
	args := []string{"--dangerously-skip-permissions", "--effort", "ultracode"}
	block := RenderProfileBlock("digrum1", "claude1@digrum.com", "Claude Max — claude1@digrum.com", args, "", nil, env)

	doc := "source_dir = \"~/.claude\"\n" + block
	var cfg Config
	if err := toml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("rendered block does not parse as TOML: %v\n---\n%s", err, doc)
	}
	p := cfg.Profiles["digrum1"]
	if p == nil {
		t.Fatalf("profile digrum1 missing after parse:\n%s", doc)
	}
	if p.Email != "claude1@digrum.com" {
		t.Errorf("email = %q, want claude1@digrum.com", p.Email)
	}
	if p.Description != "Claude Max — claude1@digrum.com" {
		t.Errorf("description = %q", p.Description)
	}
	if got := p.Env["CLAUDE_CODE_PLUGIN_CACHE_DIR"]; got != `${USERPROFILE}\.claude\plugins` {
		t.Errorf("backslash path did not survive round-trip: got %q", got)
	}
	if len(p.Args) != 3 || p.Args[1] != "--effort" {
		t.Errorf("args = %v", p.Args)
	}
}

func TestAppendProfileBlock(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	initial := "source_dir = \"~/.claude\"\n\n[profiles.digrum]\ndescription = \"x\"\n[profiles.digrum.env]\nENABLE_TOOL_SEARCH = \"true\"\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	block := RenderProfileBlock("digrum1", "claude1@digrum.com", "Claude Max — claude1@digrum.com", []string{"--effort", "ultracode"}, "", nil, map[string]string{"ENABLE_TOOL_SEARCH": "true"})
	if err := appendProfileBlock(cfgPath, block); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "[profiles.digrum1]") {
		t.Errorf("appended config missing digrum1 block:\n%s", data)
	}
	// Original content must be preserved.
	if !strings.Contains(string(data), "[profiles.digrum]") {
		t.Errorf("append clobbered existing profile")
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config no longer parses after append: %v", err)
	}
	if len(cfg.Profiles) != 2 {
		t.Errorf("want 2 profiles, got %d", len(cfg.Profiles))
	}
}

func boolPtr(b bool) *bool { return &b }

func TestManageMCPEnabled(t *testing.T) {
	if !(&Config{}).ManageMCPEnabled() {
		t.Error("nil manage_mcp should default to enabled")
	}
	if !(&Config{ManageMCP: boolPtr(true)}).ManageMCPEnabled() {
		t.Error("manage_mcp=true should be enabled")
	}
	if (&Config{ManageMCP: boolPtr(false)}).ManageMCPEnabled() {
		t.Error("manage_mcp=false should be disabled")
	}
}

func TestIsMaxProfile(t *testing.T) {
	if !(&Profile{}).IsMaxProfile() {
		t.Error("empty profile should be a Max profile")
	}
	if !(&Profile{Env: map[string]string{"ENABLE_TOOL_SEARCH": "true"}}).IsMaxProfile() {
		t.Error("plain env profile should be a Max profile")
	}
	if (&Profile{Env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.z.ai"}}).IsMaxProfile() {
		t.Error("base-url profile should NOT be a Max profile")
	}
	if (&Profile{Env: map[string]string{"ANTHROPIC_AUTH_TOKEN": "x"}}).IsMaxProfile() {
		t.Error("auth-token profile should NOT be a Max profile")
	}
}

func TestTemplateProfileName(t *testing.T) {
	cfg := &Config{Profiles: map[string]*Profile{
		"bdaya":  {Env: map[string]string{"ENABLE_TOOL_SEARCH": "true"}},
		"digrum": {Env: map[string]string{"ENABLE_TOOL_SEARCH": "true"}},
		"glm":    {Env: map[string]string{"ANTHROPIC_BASE_URL": "https://api.z.ai"}},
	}}

	// default: first Max profile lexically (bdaya)
	if got, err := cfg.TemplateProfileName(""); err != nil || got != "bdaya" {
		t.Errorf("default template = %q, %v; want bdaya", got, err)
	}
	// explicit valid
	if got, err := cfg.TemplateProfileName("digrum"); err != nil || got != "digrum" {
		t.Errorf("explicit template = %q, %v; want digrum", got, err)
	}
	// explicit glm -> error (not a Max template)
	if _, err := cfg.TemplateProfileName("glm"); err == nil {
		t.Error("glm as template should error")
	}
	// fleet.default_template respected
	cfg.Fleet = &FleetConfig{DefaultTemplate: "digrum"}
	if got, err := cfg.TemplateProfileName(""); err != nil || got != "digrum" {
		t.Errorf("fleet default template = %q, %v; want digrum", got, err)
	}
	// no Max profile at all -> error
	only := &Config{Profiles: map[string]*Profile{"glm": {Env: map[string]string{"ANTHROPIC_BASE_URL": "x"}}}}
	if _, err := only.TemplateProfileName(""); err == nil {
		t.Error("no Max profile should error")
	}
}
