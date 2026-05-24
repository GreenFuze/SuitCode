//go:build !windows

package coordinator

import (
	"os/exec"
	"syscall"
)

// SetProcAttrDetached configures cmd to start in a new session on Unix-like
// systems. This ensures the coordinator survives after the calling process exits.
func SetProcAttrDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
