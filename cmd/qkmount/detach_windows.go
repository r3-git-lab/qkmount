//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach starts the command in its own process group and detached from the
// parent console.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
		HideWindow:    true,
	}
}
