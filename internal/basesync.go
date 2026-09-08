package internal

import (
	"fmt"
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

// RenderBaseBlock wraps block -- fleet/profiles/base.toml's [base] table
// text, verbatim (FleetBase.Block) -- in the sentinel pair `cpm sync` splices
// into config.toml. Verbatim on purpose: the fleet's own fleet-doctor F10
// check compares a device's config.toml [base] against base.toml key for key
// (`~/...` placeholders and the os_overlay inline table included), so cpm
// must not re-serialise, re-order, resolve, or "improve" anything here.
// Placeholders are resolved per machine at render time (ResolveRendered),
// never in this table. Deterministic by construction: same input bytes,
// same output bytes -- required for `cpm sync --dry-run`'s diff and
// `cpm doctor`'s local-modification flag to be stable rather than flapping.
func RenderBaseBlock(block string) string {
	return baseSentinelBeginLine + "\n" + strings.TrimRight(block, " \t\r\n") + "\n" + baseSentinelEndLine + "\n"
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
