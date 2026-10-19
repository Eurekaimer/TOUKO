//go:build !windows

package webview

import "os/exec"

func configureLoginCommand(cmd *exec.Cmd) {}
