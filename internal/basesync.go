package internal

import (
	"fmt"
	"sort"
	"strings"
)

// baseSentinelBeginLine/baseSentinelEndLine delimit the CPM-MANAGED [base]
// block cpm sync writes into config.toml (design spec §2). Everything
// between them is owned by cpm and overwritten verbatim from
// fleet/profiles/base.toml on every `cpm sync --apply`; everything outside
// them -- every [profiles.*] stanza, its comments, channel_port pins -- is
// never touched by this mechanism (see SpliceBase, and its dedicated
// byte-preservation test).
const (
	baseSentinelBeginLine = "# --- BEGIN CPM-MANAGED [base] (rendered from fleet/profiles/base.toml; hand edits are overwritten on the next `cpm sync --apply` -- see `cpm doctor`) ---"
	baseSentinelEndLine   = "# --- END CPM-MANAGED [base] ---"
)

// RenderBaseBlock renders b as the literal, sentinel-delimited [base] TOML
// text cpm sync splices into config.toml. Deterministic (sorted keys) so
// repeated renders of an unchanged Base are byte-identical -- required for
// `cpm sync --dry-run`'s diff and `cpm doctor`'s local-modification flag to
// be stable rather than flapping on map-iteration order.
func RenderBaseBlock(b *Base) string {
	var buf strings.Builder
	buf.WriteString(baseSentinelBeginLine + "\n")
	buf.WriteString("[base]\n")
	if b != nil && b.Model != "" {
		fmt.Fprintf(&buf, "model = %s\n", tomlValue(b.Model))
	}
	if b != nil && len(b.Args) > 0 {
		parts := make([]string, len(b.Args))
		for i, a := range b.Args {
			parts[i] = tomlValue(a)
		}
		fmt.Fprintf(&buf, "args = [%s]\n", strings.Join(parts, ", "))
	}
	if b != nil {
		common := b.BaseEnvCommon()
		if len(common) > 0 {
			buf.WriteString("[base.env]\n")
			for _, k := range sortedEnvKeys(common) {
				fmt.Fprintf(&buf, "%s = %s\n", k, tomlValue(common[k]))
			}
		}
		for _, goos := range sortedOverlayGOOS(b) {
			overlay := b.BaseEnvOverlay(goos)
			if len(overlay) == 0 {
				continue // an empty overlay table proves the merge point exists but renders nothing
			}
			fmt.Fprintf(&buf, "[base.env.%s]\n", goos)
			for _, k := range sortedEnvKeys(overlay) {
				fmt.Fprintf(&buf, "%s = %s\n", k, tomlValue(overlay[k]))
			}
		}
	}
	buf.WriteString(baseSentinelEndLine + "\n")
	return buf.String()
}

// sortedOverlayGOOS returns the map-valued (i.e. per-GOOS overlay subtable)
// keys of b.Env, sorted, so RenderBaseBlock's section order is deterministic.
func sortedOverlayGOOS(b *Base) []string {
	if b == nil {
		return nil
	}
	var out []string
	for k, v := range b.Env {
		if _, ok := v.(map[string]any); ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ExtractBase returns the exact byte range spanning the sentinel pair
// (BEGIN line through END line, inclusive, no trailing newline), or
// ok=false if configContent has no CPM-managed [base] block yet.
func ExtractBase(configContent string) (block string, ok bool) {
	beginIdx := strings.Index(configContent, baseSentinelBeginLine)
	if beginIdx == -1 {
		return "", false
	}
	relEnd := strings.Index(configContent[beginIdx:], baseSentinelEndLine)
	if relEnd == -1 {
		return "", false
	}
	endIdx := beginIdx + relEnd + len(baseSentinelEndLine)
	return configContent[beginIdx:endIdx], true
}

// SpliceBase replaces the byte range between the CPM-managed [base] sentinel
// pair in configContent with newBlock (as rendered by RenderBaseBlock),
// leaving EVERY byte outside that range untouched -- this is the whole point
// of the mechanism (design spec §2): cpm never re-serializes config.toml as
// a TOML struct (BurntSushi/toml does not round-trip comments), it only ever
// text-splices this one owned range.
//
// If no sentinel pair is present yet, newBlock is appended (separated by a
// blank line from any existing content) rather than inserted mid-file, so a
// config.toml written before this feature existed gets its first [base]
// block without disturbing anything already there.
func SpliceBase(configContent, newBlock string) (result string, changed bool, err error) {
	newTrimmed := strings.TrimRight(newBlock, "\n")

	existing, found := ExtractBase(configContent)
	if !found {
		switch {
		case configContent == "":
			return newTrimmed + "\n", true, nil
		case strings.HasSuffix(configContent, "\n\n"):
			return configContent + newTrimmed + "\n", true, nil
		case strings.HasSuffix(configContent, "\n"):
			return configContent + "\n" + newTrimmed + "\n", true, nil
		default:
			return configContent + "\n\n" + newTrimmed + "\n", true, nil
		}
	}

	if existing == newTrimmed {
		return configContent, false, nil
	}

	beginIdx := strings.Index(configContent, baseSentinelBeginLine)
	if beginIdx == -1 {
		return "", false, fmt.Errorf("internal error: ExtractBase found a block but SpliceBase cannot re-locate its start")
	}
	endIdx := beginIdx + len(existing)
	after := endIdx
	if after < len(configContent) && configContent[after] == '\n' {
		after++ // consume exactly one trailing newline so re-splicing never accumulates blank lines
	}
	result = configContent[:beginIdx] + newTrimmed + "\n" + configContent[after:]
	return result, true, nil
}
