//go:build !windows

package oauthflow

import (
	"os/exec"
	"syscall"
)

func startDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
