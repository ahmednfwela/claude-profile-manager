//go:build windows

package internal

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW from the Windows process-creation flags:
// the child gets no console at all instead of allocating a fresh visible one.
const createNoWindow = 0x08000000

// hasConsole reports whether this process is attached to a console window.
// Under a hidden scheduled task (or any headless invocation) there is none —
// child console processes would then each allocate a fresh visible window
// unless spawned with CREATE_NO_WINDOW.
func hasConsole() bool {
	h, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	return h != 0
}

// hideIfHeadless marks cmd to run without allocating a console window, but
// only when cpm itself has no console: interactive runs keep full stdio
// passthrough and inherit the parent console unchanged.
func hideIfHeadless(cmd *exec.Cmd) {
	if hasConsole() {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
