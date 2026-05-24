//go:build windows

package coordinator

import (
	"os/exec"
	"syscall"
)

// SetProcAttrDetached configures cmd to start as a detached background process
// on Windows. The coordinator must survive after the calling process exits.
func SetProcAttrDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008,
		HideWindow:    true,
	}
}
