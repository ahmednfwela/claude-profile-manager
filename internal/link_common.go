package internal

import (
	"os"
	"path/filepath"
)

// isLink reports whether dst is a symlink or a Windows junction/reparse point
// (i.e. one of the link kinds cpm creates), as opposed to a real directory.
func isLink(dst string) bool {
	fi, err := os.Lstat(dst)
	if err != nil {
		return false
	}
	return fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
}

// linkPointsTo reports whether dst is a link (symlink or junction) whose target
// resolves to src. It works for POSIX symlinks and Windows directory junctions:
// Go's os.Readlink returns the junction target on Go >= 1.23, and we fall back
// to "is a reparse point" when the target can't be read (cpm only ever links the
// same source, so an unreadable reparse point is assumed to be ours).
func linkPointsTo(dst, src string) bool {
	fi, err := os.Lstat(dst)
	if err != nil {
		return false
	}
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
		return false
	}
	if target, err := os.Readlink(dst); err == nil {
		a, _ := filepath.Abs(target)
		b, _ := filepath.Abs(src)
		return a == b
	}
	return true
}
