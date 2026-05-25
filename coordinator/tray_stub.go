//go:build !systray

package main

import "context"

// runTray is a no-op on headless (non-systray) builds. The coordinator runs
// as a pure HTTP daemon with no tray icon.
//
// To enable the tray icon, build with:
//
//	go build -tags systray ./coordinator
func runTray(_ context.Context, _ string, _ context.CancelFunc) {}
