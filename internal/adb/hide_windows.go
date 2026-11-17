//go:build windows

package adb

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps adb from flashing a console window on Windows.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
