//go:build !systray

// Package main is the SuitCode system-tray companion.
//
// This stub is compiled when the systray build tag is absent (e.g. on headless
// servers). It prints an informative message and exits so that
// "go install ./..." always produces a binary without requiring CGo or system
// tray libraries on machines that don't have a desktop environment.
//
// To build the real tray binary:
//
//	go build -tags systray ./tray
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "suitcode-tray: this binary was built without the systray tag and cannot show a tray icon.")
	fmt.Fprintln(os.Stderr, "  To build the desktop version: go build -tags systray ./tray")
	os.Exit(1)
}
