package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// CoordinatorClient sends HTTP requests to the coordinator on behalf of one
// project. Every request carries the X-Suitcode-Project header so the
// coordinator can route it to the correct investigator.
type CoordinatorClient struct {
	coordinatorURL string
	projectPath    string
	httpClient     *http.Client
}

// NewCoordinatorClient creates a CoordinatorClient for the given coordinator
// URL and project path. The HTTP client has a generous timeout to accommodate
// slow first-run investigator warmups.
func NewCoordinatorClient(coordinatorURL, projectPath string) *CoordinatorClient {
	return &CoordinatorClient{
		coordinatorURL: coordinatorURL,
		projectPath:    projectPath,
		httpClient:     &http.Client{Timeout: 5 * time.Minute},
	}
}

// Post marshals reqBody as JSON and sends a POST to /api/v1/<feature>.
// Returns the raw JSON response body on success.
func (c *CoordinatorClient) Post(ctx context.Context, feature string, reqBody any) ([]byte, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("client: marshal request for %s: %w", feature, err)
	}

	url := c.coordinatorURL + "/api/v1/" + feature
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("client: build request for %s: %w", feature, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Suitcode-Project", c.projectPath)

	logf("→ POST %s (project: %s)", url, c.projectPath)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: POST %s: %w", feature, err)
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("client: %s returned nil body (status %d)", feature, resp.StatusCode)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("client: reading response for %s: %w", feature, err)
	}

	if resp.StatusCode != http.StatusOK {
		// Try to surface the error message from the JSON body.
		var errResp map[string]string
		if jsonErr := json.Unmarshal(respBody, &errResp); jsonErr == nil {
			if msg, ok := errResp["error"]; ok {
				return nil, fmt.Errorf("client: %s returned %d: %s", feature, resp.StatusCode, msg)
			}
		}
		return nil, fmt.Errorf("client: %s returned status %d", feature, resp.StatusCode)
	}

	return respBody, nil
}

// PostWarmup sends a warmup request to the coordinator for this project.
// The coordinator will spawn an investigator (if needed) and block until it
// reaches ReadinessLevel3 (import graph + gopls ready).
func (c *CoordinatorClient) PostWarmup(ctx context.Context) error {
	url := c.coordinatorURL + "/api/v1/warmup"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("client: build warmup request: %w", err)
	}
	req.Header.Set("X-Suitcode-Project", c.projectPath)

	logf("→ POST %s (project: %s)", url, c.projectPath)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("client: POST warmup: %w", err)
	}
	if resp.Body == nil {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("warmup returned status %d", resp.StatusCode)
		}
		return nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var errResp map[string]string
		if json.Unmarshal(body, &errResp) == nil {
			if msg, ok := errResp["error"]; ok {
				return fmt.Errorf("warmup: %s", msg)
			}
		}
		return fmt.Errorf("warmup returned status %d", resp.StatusCode)
	}

	return nil
}

// GetHealth checks if the coordinator is alive and returns the decoded status body.
func (c *CoordinatorClient) GetHealth(ctx context.Context) (map[string]any, error) {
	url := c.coordinatorURL + "/api/v1/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("coordinator health: empty response body (status %d)", resp.StatusCode)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode health: %w", err)
	}
	return result, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Coordinator auto-start
// ──────────────────────────────────────────────────────────────────────────────

// ensureCoordinator checks if the coordinator is reachable at coordinatorURL.
// If not, it finds the coordinator binary, launches it as a detached background
// process, then waits up to 10 s for it to become healthy.
func ensureCoordinator(coordinatorURL string) error {
	// Fast path: coordinator is already running.
	if isCoordinatorAlive(coordinatorURL) {
		return nil
	}

	logf("coordinator not running — attempting to start it...")
	fmt.Fprintf(os.Stderr, "[sc client] coordinator not found at %s — starting it now...\n", coordinatorURL)

	binary, err := findCoordinatorBinary()
	if err != nil {
		return fmt.Errorf("coordinator not running and cannot find coordinator binary: %w\n"+
			"  Start it manually with: coordinator", err)
	}

	// Start coordinator as a detached background process so it outlives suitcode.
	cmd := exec.Command(binary)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	setProcAttrDetached(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting coordinator %q: %w", binary, err)
	}
	// Release the process handle — coordinator continues after suitcode exits.
	if err := cmd.Process.Release(); err != nil {
		logf("warn: release coordinator process: %v", err)
	}

	logf("coordinator launched — waiting for health check...")

	// Poll up to 10 s for the coordinator to accept connections.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if isCoordinatorAlive(coordinatorURL) {
			logf("coordinator is healthy")
			fmt.Fprintf(os.Stderr, "[sc client] coordinator started successfully\n")
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("coordinator started but did not become healthy within 10s at %s", coordinatorURL)
}

// isCoordinatorAlive does a quick GET /health with a 1 s timeout.
func isCoordinatorAlive(coordinatorURL string) bool {
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

// findCoordinatorBinary locates the coordinator binary.
// Priority: sibling of this binary → PATH.
func findCoordinatorBinary() (string, error) {
	self, err := os.Executable()
	if err == nil {
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
		return "", fmt.Errorf("coordinator not found next to suitcode binary or in PATH")
	}
	return path, nil
}

// readyClient ensures the coordinator is running and returns a configured
// CoordinatorClient for the given project path.
func readyClient(repoPath string) (*CoordinatorClient, error) {
	if err := ensureCoordinator(defaultCoordinatorURL); err != nil {
		return nil, err
	}
	return NewCoordinatorClient(defaultCoordinatorURL, repoPath), nil
}
