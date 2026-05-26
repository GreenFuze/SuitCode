// Package coordinator provides an HTTP client for the SuitCode coordinator
// daemon and utilities for ensuring the daemon is running.
package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
)

// Client sends typed HTTP requests to the coordinator on behalf of one project.
// Every feature request carries the X-Suitcode-Project header so the
// coordinator can route it to the correct investigator.
//
// For coordinator-level calls (health, projects) that have no associated
// project, pass an empty string as projectPath.
type Client struct {
	coordinatorURL string
	projectPath    string
	httpClient     *http.Client
}

// NewClient creates a Client for the given coordinator URL and project path.
// The HTTP client has a generous timeout to accommodate slow first-run
// investigator warmups.
func NewClient(coordinatorURL, projectPath string) *Client {
	return &Client{
		coordinatorURL: coordinatorURL,
		projectPath:    projectPath,
		httpClient:     &http.Client{Timeout: 5 * time.Minute},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP response types
// ──────────────────────────────────────────────────────────────────────────────

// HealthResponse is the decoded body of GET /api/v1/health.
type HealthResponse struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	Projects int    `json:"projects"`
}

// ProjectInfo describes one active investigator process.
type ProjectInfo struct {
	ProjectPath string `json:"project_path"`
	Port        int    `json:"port"`
	StartedAt   string `json:"started_at"`
}

// DaemonInfo describes one LSP subprocess managed by a language provider.
// Mirrors provider.DaemonInfo for cross-package JSON decoding.
type DaemonInfo struct {
	Name    string `json:"name"`
	Binary  string `json:"binary,omitempty"`
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
}

// ProviderStatusInfo describes one provider in the investigator status.
type ProviderStatusInfo struct {
	ProviderID  string `json:"provider_id"`
	DisplayName string `json:"display_name"`
	Ready       bool   `json:"ready"`
	Summary     string `json:"summary,omitempty"`
}

// InvestigatorStatus is returned by GET /api/v1/status on the investigator.
// The coordinator proxies this endpoint transparently.
type InvestigatorStatus struct {
	Repo           string               `json:"repo"`
	Readiness      int                  `json:"readiness_level"`
	ReadinessDesc  string               `json:"readiness_desc"`
	Providers      []ProviderStatusInfo `json:"providers"`
	Daemons        []DaemonInfo         `json:"daemons"`
	LastWarmedAt   string               `json:"last_warmed_at,omitempty"`
	WarmDurationMs int64                `json:"warm_duration_ms,omitempty"`
}

// ProjectsResponse is the decoded body of GET /api/v1/projects.
type ProjectsResponse struct {
	Projects []ProjectInfo `json:"projects"`
}

// ErrorResponse is the decoded body of any coordinator/investigator error reply.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP mechanics (private)
// ──────────────────────────────────────────────────────────────────────────────

// post marshals reqBody as JSON and POSTs it to /api/v1/<feature>.
// All feature methods funnel through here.
func (c *Client) post(ctx context.Context, feature string, reqBody any) ([]byte, error) {
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
		var errResp ErrorResponse
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
func postAndDecode[Resp any](c *Client, ctx context.Context, feature string, reqBody any) (*Resp, error) {
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
// Coordinator-level methods (no project routing)
// ──────────────────────────────────────────────────────────────────────────────

// GetHealth returns the coordinator's current status.
func (c *Client) GetHealth(ctx context.Context) (*HealthResponse, error) {
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

	var result HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode health: %w", err)
	}
	return &result, nil
}

// GetProjects lists all active investigator processes managed by the coordinator.
func (c *Client) GetProjects(ctx context.Context) (*ProjectsResponse, error) {
	url := c.coordinatorURL + "/api/v1/projects"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coordinator unreachable: %w", err)
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("coordinator projects: empty response body (status %d)", resp.StatusCode)
	}
	defer resp.Body.Close()

	var result ProjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode projects: %w", err)
	}
	return &result, nil
}

// Warmup ensures the investigator for this project is fully initialized.
// The coordinator spawns the investigator on demand and blocks until it reaches
// level 3 readiness (import graph + gopls ready). Idempotent.
func (c *Client) Warmup(ctx context.Context) error {
	url := c.coordinatorURL + "/api/v1/warmup"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("client: build warmup request: %w", err)
	}
	req.Header.Set("X-Suitcode-Project", c.projectPath)

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
		var errResp ErrorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return fmt.Errorf("warmup: %s", errResp.Error)
		}
		return fmt.Errorf("warmup returned status %d", resp.StatusCode)
	}

	return nil
}

// GetStatus fetches the detailed status of the investigator for this client's
// project, including provider readiness and daemon (LSP subprocess) information.
// The coordinator transparently proxies this to the investigator.
func (c *Client) GetStatus(ctx context.Context) (*InvestigatorStatus, error) {
	url := c.coordinatorURL + "/api/v1/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("client: build status request: %w", err)
	}
	req.Header.Set("X-Suitcode-Project", c.projectPath)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: GET /api/v1/status: %w", err)
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("client: status: empty response body (status %d)", resp.StatusCode)
	}
	defer resp.Body.Close()

	var result InvestigatorStatus
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("client: decode status: %w", err)
	}
	return &result, nil
}

// StopProject asks the coordinator to kill the investigator for the given
// project path. Idempotent: stopping an unknown project returns nil.
// The projectPath parameter is used as the routing header directly, making it
// safe to call from a Client with an empty projectPath field.
func (c *Client) StopProject(ctx context.Context, projectPath string) error {
	url := c.coordinatorURL + "/api/v1/projects/stop"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("client: build stop request: %w", err)
	}
	req.Header.Set("X-Suitcode-Project", projectPath)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("client: stop project: %w", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			var errResp ErrorResponse
			if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
				return fmt.Errorf("stop project: %s", errResp.Error)
			}
			return fmt.Errorf("stop project returned status %d", resp.StatusCode)
		}
	} else if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stop project returned status %d", resp.StatusCode)
	}

	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature methods (routed to the project's investigator)
// ──────────────────────────────────────────────────────────────────────────────

// RepoOverview returns a repository structure and technology overview.
func (c *Client) RepoOverview(ctx context.Context, req cfeatures.RepoOverviewRequest) (*cfeatures.RepoOverviewResponse, error) {
	return postAndDecode[cfeatures.RepoOverviewResponse](c, ctx, "repo-overview", req)
}

// ExplainFile returns a detailed explanation of a single file's role and relationships.
func (c *Client) ExplainFile(ctx context.Context, req cfeatures.ExplainFileRequest) (*cfeatures.ExplainFileResponse, error) {
	return postAndDecode[cfeatures.ExplainFileResponse](c, ctx, "explain-file", req)
}

// Related returns files related to a given seed file.
func (c *Client) Related(ctx context.Context, req cfeatures.RelatedRequest) (*cfeatures.RelatedResponse, error) {
	return postAndDecode[cfeatures.RelatedResponse](c, ctx, "related", req)
}

// Tests returns test files relevant to a source file or git diff.
func (c *Client) Tests(ctx context.Context, req cfeatures.TestsRequest) (*cfeatures.TestsResponse, error) {
	return postAndDecode[cfeatures.TestsResponse](c, ctx, "tests", req)
}

// Impact returns blast-radius analysis for a set of changed files.
func (c *Client) Impact(ctx context.Context, req cfeatures.ImpactRequest) (*cfeatures.ImpactResponse, error) {
	return postAndDecode[cfeatures.ImpactResponse](c, ctx, "impact", req)
}

// Context compiles a bounded context capsule for a set of seed files.
func (c *Client) Context(ctx context.Context, req cfeatures.ContextRequest) (*cfeatures.ContextResponse, error) {
	return postAndDecode[cfeatures.ContextResponse](c, ctx, "context", req)
}

// FailureContext extracts structured context from a failure log.
func (c *Client) FailureContext(ctx context.Context, req cfeatures.FailureContextRequest) (*cfeatures.FailureContextResponse, error) {
	return postAndDecode[cfeatures.FailureContextResponse](c, ctx, "failure-context", req)
}

// VerifyPlan generates a verification plan for a set of changed files.
func (c *Client) VerifyPlan(ctx context.Context, req cfeatures.VerifyPlanRequest) (*cfeatures.VerifyPlanResponse, error) {
	return postAndDecode[cfeatures.VerifyPlanResponse](c, ctx, "verify-plan", req)
}
