//go:build systray

// Package main is the SuitCode system-tray companion (desktop build).
//
// Shows coordinator status and active investigator processes in the system
// tray. Each project can be stopped from the menu. The coordinator is
// auto-started on launch if it is not already running.
//
// Build:
//
//	go build -tags systray ./tray
//
// Platform requirements:
//
//	Linux:   CGo + libayatana-appindicator3-dev (apt install libayatana-appindicator3-dev)
//	macOS:   none (AppKit via CGo, included with Xcode)
//	Windows: none (Win32 Shell_NotifyIcon via CGo, included with MSVC/mingw)
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	coord "github.com/GreenFuze/SuitCode/core/coordinator"
)

const coordinatorURL = "http://127.0.0.1:7878"

func main() {
	// On Linux, fail fast when no display server is available.
	// fyne.io/systray will crash or hang without one.
	if runtime.GOOS == "linux" {
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			fmt.Fprintln(os.Stderr, "suitcode-tray: no display detected (DISPLAY and WAYLAND_DISPLAY are both unset).")
			fmt.Fprintln(os.Stderr, "  A desktop environment is required for the tray icon.")
			fmt.Fprintln(os.Stderr, "  On headless servers, build without -tags systray.")
			os.Exit(1)
		}
	}

	// Attempt to start the coordinator if it is not already running.
	// Failure is non-fatal — the tray will show "offline" status.
	if err := coord.EnsureRunning(coordinatorURL); err != nil {
		fmt.Fprintf(os.Stderr, "suitcode-tray: coordinator unavailable: %v\n", err)
		fmt.Fprintln(os.Stderr, "  The tray will show offline status. Start coordinator manually if needed.")
	}

	// Use signal-aware context so Ctrl-C / SIGTERM gracefully quit the tray.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Client with empty project path for coordinator-level calls (health, projects).
	client := coord.NewClient(coordinatorURL, "")

	tray := NewTray(ctx, client)
	tray.Run()
}
