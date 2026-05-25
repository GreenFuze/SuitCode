//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideChildWindow sets CREATE_NO_WINDOW so the spawned investigator process does
// not open a visible console window. The coordinator is a windowsgui binary (no
// console of its own), so without this flag the OS allocates a new console
// window for every console-subsystem child it spawns.
func hideChildWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow,
	}
}
