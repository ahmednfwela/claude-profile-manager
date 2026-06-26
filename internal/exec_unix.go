//go:build !windows

package internal

import "syscall"

// RunClaude replaces the current process image with claude (Unix exec).
// It does not return on success.
func RunClaude(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
