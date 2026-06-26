//go:build windows

package internal

import (
	"os"
	"os/exec"
	"path/filepath"
)

// linkDir creates a link at dst pointing to src. On Windows it prefers a
// directory junction (mklink /J), which a normal user can create with no
// elevation and no Developer Mode. It falls back to a real symlink (works when
// Developer Mode is on) and finally to a recursive copy so the operation never
// hard-fails on a locked-down host. All five shared dirs cpm links are
// directories, so a junction is always the correct primitive.
func linkDir(src, dst string) error {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		absSrc = src
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		absDst = dst
	}

	// mklink requires dst not to exist; callers only invoke linkDir for absent dst.
	if err := exec.Command("cmd", "/c", "mklink", "/J", absDst, absSrc).Run(); err == nil {
		return nil
	}
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	return copyTree(src, dst)
}

// copyTree recursively copies src into dst (last-resort fallback when no link
// type can be created).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return cloudCopyFile(path, target)
	})
}
