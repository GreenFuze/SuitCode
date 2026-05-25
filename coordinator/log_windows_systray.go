//go:build windows && systray

package main

import (
	"os"
	"path/filepath"
)

// init redirects log output to a file when running as a Windows GUI
// application (no console). The log file is truncated on each coordinator
// start so it stays small and always reflects the current session.
//
// Log location: %APPDATA%\SuitCode\coordinator.log
func init() {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return // fall back to stderr if APPDATA is unset (unusual)
	}

	dir := filepath.Join(appData, "SuitCode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	logPath := filepath.Join(dir, "coordinator.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}

	// In a windowsgui binary os.Stderr is an invalid/null handle. Using
	// MultiWriter would cause every write to fail on the stderr side, which
	// would silently swallow file writes too (io.MultiWriter short-circuits on
	// the first error). Write only to the file.
	logWriter = f
}
