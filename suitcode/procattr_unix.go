//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setProcAttrDetached configures cmd to start in a new session on Unix-like
// systems. This ensures the coordinator survives after suitcode exits.
func setProcAttrDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
