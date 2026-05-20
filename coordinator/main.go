// Package main is the entry point for the SuitCode coordinator service.
//
// The coordinator is a system-wide daemon (one per machine) that:
//   - Listens on a fixed port (:7878 by default)
//   - Maintains a registry of per-project investigator processes
//   - Spawns investigators on demand when a suitcode client request arrives
//   - Proxies feature requests to the correct investigator using the
//     X-Suitcode-Project header for routing
//
// Usage:
//
//	coordinator [--port 7878] [--investigator /path/to/investigator]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	port := flag.Int("port", 7878, "TCP port to listen on")
	invBinary := flag.String("investigator", "", "path to the investigator binary (default: auto-detect)")
	flag.Parse()

	// Resolve the investigator binary path.
	inv, err := resolveInvestigatorBinary(*invBinary)
	if err != nil {
		logf("FATAL: cannot find investigator binary: %v", err)
		os.Exit(1)
	}
	logf("using investigator binary: %s", inv)

	coord := NewCoordinator(*port, inv)

	// Graceful shutdown on SIGINT / SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- coord.Start()
	}()

	select {
	case sig := <-stop:
		logf("received signal %s — shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := coord.Shutdown(ctx); err != nil {
			logf("shutdown error: %v", err)
		}
	case err := <-errCh:
		if err != nil {
			logf("FATAL: %v", err)
			os.Exit(1)
		}
	}
}

// resolveInvestigatorBinary returns the path to the investigator binary.
// Priority order:
//  1. Explicit --investigator flag.
//  2. Same directory as the coordinator binary (most common: go install both).
//  3. PATH lookup.
func resolveInvestigatorBinary(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("%q not found: %w", explicit, err)
		}
		return explicit, nil
	}

	// Check sibling of this binary.
	self, err := os.Executable()
	if err == nil {
		candidates := []string{
			filepath.Join(filepath.Dir(self), "investigator"),
			filepath.Join(filepath.Dir(self), "investigator.exe"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}

	// Fall back to PATH.
	path, err := exec.LookPath("investigator")
	if err != nil {
		return "", fmt.Errorf("investigator not found in PATH and not next to coordinator binary")
	}
	return path, nil
}

// logf writes a timestamped message to stderr with the [sc coordinator] prefix.
func logf(format string, args ...any) {
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[sc coordinator] %s %s\n", ts, msg)
}
