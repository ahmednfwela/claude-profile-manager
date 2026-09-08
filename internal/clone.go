package internal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RenderClonePlaceholderBlock renders a `[profiles.<alias>]` stanza with an
// EMPTY `[profiles.<alias>.auth]` placeholder (mode left blank, a TODO
// comment) -- design spec §1/§4: a freshly-cloned profile is born
// "valid-but-incomplete", never silently unrendered. The placeholder is
// guaranteed to round-trip through LoadConfig with a non-nil Profile.Auth
// (an explicit `mode = ""` line, not just a bare table header) so `cpm
// doctor` and a human both have something concrete to find and fill in.
func RenderClonePlaceholderBlock(alias string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n# %s -- cloned via `cpm clone`. Fill in [profiles.%s.auth] below,\n", alias, alias)
	fmt.Fprintf(&b, "# then run `cpm sync --profile %s`.\n", alias)
	fmt.Fprintf(&b, "[profiles.%s]\n", alias)
	fmt.Fprintf(&b, "description = %s\n", tomlValue(alias))
	fmt.Fprintf(&b, "\n[profiles.%s.auth]\n", alias)
	b.WriteString("# TODO: set mode to \"oauth\" or \"api_key\", then add a matching\n")
	fmt.Fprintf(&b, "# [profiles.%s.auth.env] table with the carrier env var(s).\n", alias)
	b.WriteString("mode = \"\"\n")
	return b.String()
}

// CloneProfile copies a source profile's mutable files (settings.local.json,
// CLAUDE.md, ...) and re-links its shared directories -- cheap directory-level
// bootstrap, unchanged from before this feature. It ALSO appends a
// [profiles.<targetName>] stanza (with the auth placeholder above) to
// config.toml, closing the gap that used to leave a cloned profile
// config.toml-blind until someone remembered to hand-add it (design spec
// §4). configPath must point at a real, already-valid config.toml -- the
// append reuses add.go's acquireConfigLock/writeFileAtomic machinery, so a
// concurrent `cpm add`/`cpm clone` cannot race and silently drop either
// process's appended block.
func CloneProfile(sourceName, targetName, profilesBase, sourceDir, configPath string, cfg *Config) error {
	srcDir := filepath.Join(profilesBase, sourceName)
	dstDir := filepath.Join(profilesBase, targetName)

	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		return fmt.Errorf("source profile %q not installed (run cpm install first)", sourceName)
	}

	if _, err := os.Stat(dstDir); err == nil {
		return fmt.Errorf("target profile %q already exists", targetName)
	}

	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("cannot create target directory: %w", err)
	}

	// Copy mutable files from the source profile (not from ~/.claude)
	for _, filename := range copyFiles {
		src := filepath.Join(srcDir, filename)
		dst := filepath.Join(dstDir, filename)

		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}

		if err := cloneCopyFile(src, dst); err != nil {
			return fmt.Errorf("cannot copy %s: %w", filename, err)
		}
		fmt.Printf("  copied %s\n", filename)
	}

	// Re-create symlinks pointing to the original source dir
	for _, dirname := range symlinkDirs {
		target := filepath.Join(sourceDir, dirname)
		link := filepath.Join(dstDir, dirname)

		if _, err := os.Stat(target); os.IsNotExist(err) {
			continue
		}

		if err := linkDir(target, link); err != nil {
			return fmt.Errorf("cannot link %s: %w", dirname, err)
		}
		fmt.Printf("  linked %s/ -> %s\n", dirname, target)
	}

	block := RenderClonePlaceholderBlock(targetName)
	if err := appendProfileBlock(configPath, block); err != nil {
		return fmt.Errorf("cannot append [profiles.%s] to config: %w", targetName, err)
	}

	fmt.Printf("\nProfile %q cloned from %q.\n", targetName, sourceName)
	fmt.Println("Note: credentials are NOT cloned — authenticate with: claude-" + targetName)
	fmt.Printf("Appended [profiles.%s] to %s with an empty [profiles.%s.auth] placeholder.\n", targetName, ExpandPath(configPath), targetName)
	fmt.Printf("Fill in [profiles.%s.auth], then run: cpm sync --profile %s\n", targetName, targetName)
	fmt.Println("Then run 'cpm install' to generate the wrapper script.")

	return nil
}

func cloneCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
