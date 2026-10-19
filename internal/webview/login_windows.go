//go:build windows

package webview

import (
	"os/exec"
	"syscall"
)

// configureLoginCommand keeps the background gh process from flashing a
// console window when TOUKO is running as a desktop app.
func configureLoginCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
