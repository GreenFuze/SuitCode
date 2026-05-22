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

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
)

// CoordinatorHealthResponse is the decoded body of GET /api/v1/health.
type CoordinatorHealthResponse struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	Projects int    `json:"projects"`
}

// errorResponse is the decoded body of any coordinator/investigator error reply.
type errorResponse struct {
	Error string `json:"error"`
}

// CoordinatorClient sends typed HTTP requests to the coordinator on behalf of
// one project. Every request carries the X-Suitcode-Project header so the
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

// ──────────────────────────────────────────────────────────────────────────────
// HTTP mechanics (private)
// ──────────────────────────────────────────────────────────────────────────────

// post marshals reqBody as JSON and POSTs it to /api/v1/<feature>.
// All feature methods funnel through here. Returns the raw response body.
func (c *CoordinatorClient) post(ctx context.Context, feature string, reqBody any) ([]byte, error) {
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
		// Surface the error message from the JSON body when possible.
		var errResp errorResponse
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			return nil, fmt.Errorf("client: %s returned %d: %s", feature, resp.StatusCode, errResp.Error)
		}
		return nil, fmt.Errorf("client: %s returned status %d", feature, resp.StatusCode)
	}

	return respBody, nil
}

// postAndDecode POSTs a feature request and decodes the response into Resp.
// Go does not allow type parameters on methods, so this is a package-level
// generic function. The feature-name string is the only stringly-typed thing
// and it is entirely contained here — callers use the named methods below.
func postAndDecode[Resp any](c *CoordinatorClient, ctx context.Context, feature string, reqBody any) (*Resp, error) {
	raw, err := c.post(ctx, feature, reqBody)
	if err != nil {
		return nil, err
	}

	var resp Resp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("client: decode %s response: %w", feature, err)
	}
	return &resp, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature methods
// ──────────────────────────────────────────────────────────────────────────────

// GetHealth returns the coordinator's current status.
func (c *CoordinatorClient) GetHealth(ctx context.Context) (*CoordinatorHealthResponse, error) {
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

	var result CoordinatorHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode health: %w", err)
	}
	return &result, nil
}

// Warmup ensures the investigator for this project is fully initialized.
// The coordinator spawns the investigator on demand and blocks until it reaches
// level 3 readiness (import graph + gopls ready). Idempotent: an already-warm
// investigator causes an immediate return.
func (c *CoordinatorClient) Warmup(ctx context.Context) error {
	url := c.coordinatorURL + "/api/v1/warmup"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("client: build warmup request: %w", err)
	}
	req.Header.Set("X-Suitcode-Project", c.projectPath)

	logf("→ POST %s (project: %s)", url, c.projectPath)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("client: warmup: %w", err)
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
		var errResp errorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return fmt.Errorf("warmup: %s", errResp.Error)
		}
		return fmt.Errorf("warmup returned status %d", resp.StatusCode)
	}

	return nil
}

// RepoOverview returns a repository structure and technology overview.
func (c *CoordinatorClient) RepoOverview(ctx context.Context, req cfeatures.RepoOverviewRequest) (*cfeatures.RepoOverviewResponse, error) {
	return postAndDecode[cfeatures.RepoOverviewResponse](c, ctx, "repo-overview", req)
}

// ExplainFile returns a detailed explanation of a single file's role and relationships.
func (c *CoordinatorClient) ExplainFile(ctx context.Context, req cfeatures.ExplainFileRequest) (*cfeatures.ExplainFileResponse, error) {
	return postAndDecode[cfeatures.ExplainFileResponse](c, ctx, "explain-file", req)
}

// Related returns files related to a given seed file.
func (c *CoordinatorClient) Related(ctx context.Context, req cfeatures.RelatedRequest) (*cfeatures.RelatedResponse, error) {
	return postAndDecode[cfeatures.RelatedResponse](c, ctx, "related", req)
}

// Tests returns test files relevant to a source file or git diff.
func (c *CoordinatorClient) Tests(ctx context.Context, req cfeatures.TestsRequest) (*cfeatures.TestsResponse, error) {
	return postAndDecode[cfeatures.TestsResponse](c, ctx, "tests", req)
}

// Impact returns blast-radius analysis for a set of changed files.
func (c *CoordinatorClient) Impact(ctx context.Context, req cfeatures.ImpactRequest) (*cfeatures.ImpactResponse, error) {
	return postAndDecode[cfeatures.ImpactResponse](c, ctx, "impact", req)
}

// Context compiles a bounded context capsule for a set of seed files.
func (c *CoordinatorClient) Context(ctx context.Context, req cfeatures.ContextRequest) (*cfeatures.ContextResponse, error) {
	return postAndDecode[cfeatures.ContextResponse](c, ctx, "context", req)
}

// FailureContext extracts structured context from a failure log.
func (c *CoordinatorClient) FailureContext(ctx context.Context, req cfeatures.FailureContextRequest) (*cfeatures.FailureContextResponse, error) {
	return postAndDecode[cfeatures.FailureContextResponse](c, ctx, "failure-context", req)
}

// VerifyPlan generates a verification plan for a set of changed files.
func (c *CoordinatorClient) VerifyPlan(ctx context.Context, req cfeatures.VerifyPlanRequest) (*cfeatures.VerifyPlanResponse, error) {
	return postAndDecode[cfeatures.VerifyPlanResponse](c, ctx, "verify-plan", req)
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
