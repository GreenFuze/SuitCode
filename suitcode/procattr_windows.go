//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// setProcAttrDetached configures cmd to start as a detached background process
// on Windows. The coordinator must survive after suitcode exits.
func setProcAttrDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008,
		HideWindow:    true,
	}
}
