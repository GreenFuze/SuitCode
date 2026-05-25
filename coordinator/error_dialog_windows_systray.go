//go:build windows && systray

package main

import (
	"syscall"
	"unsafe"
)

// Windows MessageBox flag constants.
const (
	mbOK        = 0x00000000
	mbIconError = 0x00000010
)

// showErrorDialog displays a native Windows MessageBox with an error icon.
// Used in the windowsgui coordinator build where there is no console to write to.
// Blocks until the user dismisses the dialog.
func showErrorDialog(msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	messageBoxW := user32.NewProc("MessageBoxW")

	text, _ := syscall.UTF16PtrFromString(msg)
	title, _ := syscall.UTF16PtrFromString("SuitCode — Already Running")

	_, _, _ = messageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(title)),
		mbOK|mbIconError,
	)
}
