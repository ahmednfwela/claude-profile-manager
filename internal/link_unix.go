//go:build !windows

package internal

import "os"

// linkDir creates a link at dst pointing to src. On Unix that is a symlink.
func linkDir(src, dst string) error {
	return os.Symlink(src, dst)
}
