package coordinator

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// EnsureRunning checks whether the coordinator is reachable at coordinatorURL.
// If not, it locates the coordinator binary, launches it as a detached background
// process, and waits up to 10 s for it to become healthy.
func EnsureRunning(coordinatorURL string) error {
	// Fast path: coordinator is already running.
	if IsAlive(coordinatorURL) {
		return nil
	}

	fmt.Fprintf(os.Stderr, "[sc coordinator] not found at %s — starting it now...\n", coordinatorURL)

	binary, err := FindBinary()
	if err != nil {
		return fmt.Errorf("coordinator not running and cannot find coordinator binary: %w\n"+
			"  Start it manually with: coordinator", err)
	}

	// Start coordinator as a detached background process so it outlives the caller.
	cmd := exec.Command(binary)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	SetProcAttrDetached(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting coordinator %q: %w", binary, err)
	}

	// Release the process handle — coordinator continues after the caller exits.
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "[sc coordinator] warn: release coordinator process: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "[sc coordinator] launched — waiting for health check...\n")

	// Poll up to 10 s for the coordinator to accept connections.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if IsAlive(coordinatorURL) {
			fmt.Fprintf(os.Stderr, "[sc coordinator] started successfully\n")
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("coordinator started but did not become healthy within 10s at %s", coordinatorURL)
}

// IsAlive does a quick GET /health with a 1 s timeout.
// Returns true only if the coordinator responds with HTTP 200.
func IsAlive(coordinatorURL string) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(coordinatorURL + "/api/v1/health")
	if err != nil {
		return false
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	return resp.StatusCode == http.StatusOK
}

// FindBinary locates the coordinator binary.
// Search order: sibling of the current executable → PATH.
func FindBinary() (string, error) {
	self, err := os.Executable()
	if err == nil {
		// Check next to the current binary first (typical installed layout).
		candidates := []string{
			filepath.Join(filepath.Dir(self), "coordinator"),
			filepath.Join(filepath.Dir(self), "coordinator.exe"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}

	path, err := exec.LookPath("coordinator")
	if err != nil {
		return "", fmt.Errorf("coordinator not found next to calling binary or in PATH")
	}
	return path, nil
}
