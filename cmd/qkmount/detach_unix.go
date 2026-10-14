//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach starts the command in its own session so it survives the parent.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
