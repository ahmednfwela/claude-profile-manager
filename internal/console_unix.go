//go:build !windows

package internal

import "os/exec"

// hideIfHeadless is a no-op on Unix: child processes never allocate display
// windows, so there is nothing to suppress.
func hideIfHeadless(_ *exec.Cmd) {}
