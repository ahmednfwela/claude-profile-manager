package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// readMCPServersDoc reads profileDir/.claude.json (or an empty document if
// the file is simply absent -- a profile that has never been installed) and
// returns the whole decoded document plus its mcpServers subtable.
func readMCPServersDoc(profileDir string) (doc map[string]any, servers map[string]any, err error) {
	cfgPath := filepath.Join(profileDir, ".claude.json")
	doc = map[string]any{}
	raw, rerr := os.ReadFile(cfgPath)
	switch {
	case rerr == nil:
		if uerr := json.Unmarshal(raw, &doc); uerr != nil {
			return nil, nil, fmt.Errorf("%s is not valid JSON (refusing to overwrite): %w", cfgPath, uerr)
		}
	case os.IsNotExist(rerr):
		// no file yet -- doc stays {}
	default:
		return nil, nil, fmt.Errorf("read %s: %w", cfgPath, rerr)
	}
	servers, _ = doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	return doc, servers, nil
}

// mergeServers computes the additive-only merge of patch into servers: every
// key in patch is added or updated; every key NOT in patch -- including one
// this process never wrote itself, such as ChannelInstall's "cpm-channel"
// entry -- is carried through unchanged. Never a full-map replace (see
// MergeMCPServersPatch's doc comment for why that distinction is the whole
// point).
func mergeServers(servers, patch map[string]any) (merged map[string]any, changed bool) {
	merged = make(map[string]any, len(servers)+len(patch))
	for k, v := range servers {
		merged[k] = v
	}
	for k, v := range patch {
		if existing, ok := merged[k]; ok {
			e, _ := json.Marshal(existing)
			n, _ := json.Marshal(v)
			if string(e) == string(n) {
				continue
			}
		}
		merged[k] = v
		changed = true
	}
	return merged, changed
}

// MergeMCPServersPatch additive-merges patch's keys into profileDir's
// .claude.json mcpServers map: every key in patch is set (added or updated),
// and every key NOT in patch is left exactly as it was. This is deliberately
// NEVER a full-map replace (design spec §5): two independent writers already
// share this file today (SyncMCPServers's legacy mirror, and
// ChannelInstall's cpm-channel self-registration -- internal/channel.go),
// and a full-map replace would silently drop whichever one didn't run most
// recently. See TestMergeMCPServersPatchPreservesChannelInstallEntry.
//
// An empty/nil patch is a no-op that never touches the filesystem (a profile
// with nothing staged to merge is not a failure, and must not create a
// .claude.json where none exists).
func MergeMCPServersPatch(profileDir string, patch map[string]any) (changed bool, err error) {
	if len(patch) == 0 {
		return false, nil
	}
	doc, servers, err := readMCPServersDoc(profileDir)
	if err != nil {
		return false, err
	}
	merged, changed := mergeServers(servers, patch)
	if !changed {
		return false, nil
	}
	doc["mcpServers"] = merged

	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return false, err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(profileDir, ".claude.json"), append(out, '\n'), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// DiffMCPServersPatch computes what MergeMCPServersPatch WOULD change,
// without writing anything -- the dry-run half of the same merge contract,
// used by `cpm sync --dry-run` and `cpm doctor`'s drift report. before/after
// are pretty-printed JSON of the mcpServers map alone (not the whole
// document), so a diff highlights only what actually changed.
func DiffMCPServersPatch(profileDir string, patch map[string]any) (before, after string, changed bool, err error) {
	_, servers, err := readMCPServersDoc(profileDir)
	if err != nil {
		return "", "", false, err
	}
	beforeJSON, _ := json.MarshalIndent(servers, "", "  ")
	if len(patch) == 0 {
		return string(beforeJSON), string(beforeJSON), false, nil
	}
	merged, changed := mergeServers(servers, patch)
	afterJSON, _ := json.MarshalIndent(merged, "", "  ")
	return string(beforeJSON), string(afterJSON), changed, nil
}
