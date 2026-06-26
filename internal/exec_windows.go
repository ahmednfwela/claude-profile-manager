//go:build windows

package internal

import (
	"errors"
	"os"
	"os/exec"
)

// RunClaude launches claude as a child process on Windows (which has no exec
// that replaces the image) wiring stdio straight through and propagating the
// child's exit code so a non-zero claude exit surfaces to the shell — matching
// the Unix syscall.Exec semantics as closely as possible. On success it exits
// the process directly; it only returns when the child fails to start.
func RunClaude(path string, argv, env []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	err := cmd.Run()
	if err == nil {
		os.Exit(0)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	return err // failed to start (e.g. binary missing)
}
