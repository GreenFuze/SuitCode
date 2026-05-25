// Package main is the entry point for the SuitCode coordinator service.
//
// The coordinator is a system-wide daemon (one per machine) that:
//   - Listens on a fixed port (:7878 by default)
//   - Maintains a registry of per-project investigator processes
//   - Spawns investigators on demand when a suitcode client request arrives
//   - Proxies feature requests to the correct investigator using the
//     X-Suitcode-Project header for routing
//   - Displays a system-tray icon when built with -tags systray
//
// Usage:
//
//	coordinator [--port 7878] [--investigator /path/to/investigator]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
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

	// Refuse to start if another coordinator is already running on this port.
	// On Windows GUI builds showErrorDialog pops a native MessageBox; on all
	// other builds logf writes to stderr.
	if running, existingURL := checkAlreadyRunning(*port); running {
		msg := fmt.Sprintf(
			"A SuitCode coordinator is already running at %s.\n\nOnly one instance can run at a time.",
			existingURL,
		)
		logf("FATAL: %s", msg)
		showErrorDialog(msg)
		os.Exit(1)
	}

	// Resolve the investigator binary path.
	inv, err := resolveInvestigatorBinary(*invBinary)
	if err != nil {
		logf("FATAL: cannot find investigator binary: %v", err)
		os.Exit(1)
	}
	logf("using investigator binary: %s", inv)

	coord := NewCoordinator(*port, inv)
	coordinatorURL := fmt.Sprintf("http://127.0.0.1:%d", *port)

	// Context driven by OS signals so both the HTTP server and the tray react
	// consistently to SIGINT / SIGTERM.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start the HTTP server in the background. ErrServerClosed is expected
	// during clean shutdown and is filtered out.
	serverDone := make(chan error, 1)
	go func() {
		if err := coord.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverDone <- err
		}
		close(serverDone)
	}()

	// runTray blocks on the main goroutine when built with -tags systray.
	// On headless builds it returns immediately. In both cases it calls
	// cancel() before returning so the shutdown path below always runs.
	runTray(ctx, coordinatorURL, cancel)

	// Wait for either context cancellation (signal or tray quit) or a fatal
	// server startup failure.
	select {
	case <-ctx.Done():
		// Normal shutdown — signal received or tray dismissed.
	case err := <-serverDone:
		if err != nil {
			logf("FATAL: server error: %v", err)
			os.Exit(1)
		}
		// Server exited cleanly without a signal.
		return
	}

	logf("shutting down...")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := coord.Shutdown(shutCtx); err != nil {
		logf("shutdown error: %v", err)
	}

	// Wait for the server goroutine to finish so we don't exit with open
	// resources. ErrServerClosed is already filtered inside the goroutine.
	<-serverDone
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

