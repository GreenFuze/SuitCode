//go:build !windows

package main

import "os/exec"

// hideChildWindow is a no-op on non-Windows platforms (no console allocation concern).
func hideChildWindow(_ *exec.Cmd) {}
