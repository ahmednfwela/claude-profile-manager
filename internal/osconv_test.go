package internal

import (
	"reflect"
	"testing"
)

func TestTranslateEnvForOS_WinToUnix(t *testing.T) {
	env := map[string]string{
		"CLAUDE_CODE_PLUGIN_CACHE_DIR":    `${USERPROFILE}\.claude\plugins`,
		"CLAUDE_CODE_USE_POWERSHELL_TOOL": "1",
		"ENABLE_TOOL_SEARCH":              "true",
		"ANTHROPIC_BASE_URL":              "https://api.z.ai/api/anthropic",
	}
	got := TranslateEnvForOS(env, OSDarwin)
	want := map[string]string{
		"CLAUDE_CODE_PLUGIN_CACHE_DIR": "${HOME}/.claude/plugins",
		"ENABLE_TOOL_SEARCH":           "true",
		"ANTHROPIC_BASE_URL":           "https://api.z.ai/api/anthropic",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("win->darwin = %#v\nwant %#v", got, want)
	}
}

func TestTranslateEnvForOS_UnixToWin(t *testing.T) {
	env := map[string]string{
		"CLAUDE_CODE_PLUGIN_CACHE_DIR": "${HOME}/.claude/plugins",
		"ENABLE_TOOL_SEARCH":           "true",
	}
	got := TranslateEnvForOS(env, OSWindows)
	if got["CLAUDE_CODE_PLUGIN_CACHE_DIR"] != `${USERPROFILE}\.claude\plugins` {
		t.Errorf("unix->win path = %q", got["CLAUDE_CODE_PLUGIN_CACHE_DIR"])
	}
	if got["ENABLE_TOOL_SEARCH"] != "true" {
		t.Errorf("non-path value mangled: %q", got["ENABLE_TOOL_SEARCH"])
	}
}

func TestTranslatePathValue_NoHomeVarUnchanged(t *testing.T) {
	// A URL with slashes but no home var must not be touched when targeting Windows.
	in := "https://api.z.ai/api/anthropic"
	if got := translatePathValue(in, OSWindows); got != in {
		t.Errorf("non-home value changed: %q", got)
	}
}

func TestTranslateEnvForOS_PowershellKnob(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_USE_POWERSHELL_TOOL": "1"}
	if _, ok := TranslateEnvForOS(env, OSLinux)["CLAUDE_CODE_USE_POWERSHELL_TOOL"]; ok {
		t.Error("powershell knob should be dropped on linux")
	}
	if _, ok := TranslateEnvForOS(env, OSDarwin)["CLAUDE_CODE_USE_POWERSHELL_TOOL"]; ok {
		t.Error("powershell knob should be dropped on darwin")
	}
	if _, ok := TranslateEnvForOS(env, OSWindows)["CLAUDE_CODE_USE_POWERSHELL_TOOL"]; !ok {
		t.Error("powershell knob should be kept on windows")
	}
}
