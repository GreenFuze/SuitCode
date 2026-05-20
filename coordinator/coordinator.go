package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Coordinator is the system-wide HTTP server that routes suitcode client
// requests to the appropriate per-project investigator process.
// It owns the Registry and the http.Server lifecycle.
type Coordinator struct {
	port     int
	registry *Registry
	server   *http.Server
}

// NewCoordinator creates a Coordinator that will listen on the given port and
// use invBinary to spawn investigator processes.
func NewCoordinator(port int, invBinary string) *Coordinator {
	c := &Coordinator{
		port:     port,
		registry: NewRegistry(invBinary),
	}

	r := chi.NewRouter()

	// Middleware.
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(5 * time.Minute))

	// Coordinator-level health and management routes.
	r.Get("/api/v1/health", c.handleHealth)
	r.Get("/api/v1/projects", c.handleProjects)
	r.Post("/api/v1/warmup", c.handleWarmup)

	// All other routes are proxied to the relevant investigator.
	r.HandleFunc("/api/v1/*", c.proxyToInvestigator)

	c.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return c
}

// Start begins listening and blocks until the server exits.
func (c *Coordinator) Start() error {
	ln, err := net.Listen("tcp", c.server.Addr)
	if err != nil {
		return fmt.Errorf("coordinator: listen on %s: %w", c.server.Addr, err)
	}
	logf("listening on http://localhost:%d", c.port)
	return c.server.Serve(ln)
}

// Shutdown gracefully stops the coordinator and all managed investigators.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	logf("shutting down...")
	c.registry.Shutdown()
	return c.server.Shutdown(ctx)
}

// ──────────────────────────────────────────────────────────────────────────────
// Handlers
// ──────────────────────────────────────────────────────────────────────────────

// handleHealth reports coordinator status.
func (c *Coordinator) handleHealth(w http.ResponseWriter, r *http.Request) {
	procs := c.registry.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"version":    "v1",
		"projects":   len(procs),
	})
}

// handleProjects lists all active investigator processes.
func (c *Coordinator) handleProjects(w http.ResponseWriter, r *http.Request) {
	procs := c.registry.List()

	type projectInfo struct {
		ProjectPath string `json:"project_path"`
		Port        int    `json:"port"`
		StartedAt   string `json:"started_at"`
	}

	infos := make([]projectInfo, 0, len(procs))
	for _, p := range procs {
		infos = append(infos, projectInfo{
			ProjectPath: p.ProjectPath,
			Port:        p.Port,
			StartedAt:   p.StartedAt.Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"projects": infos,
	})
}

// handleWarmup spawns an investigator for the requested project and waits
// until it reaches ReadinessLevel3 (import graph loaded). Designed for
// user-initiated pre-warming before agent work begins.
func (c *Coordinator) handleWarmup(w http.ResponseWriter, r *http.Request) {
	projectPath := r.Header.Get(projectPathHeader)
	if projectPath == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s header is required", projectPathHeader))
		return
	}

	logf("warmup requested for %s", projectPath)

	proc, err := c.registry.Warmup(r.Context(), projectPath)
	if err != nil {
		logf("warmup failed for %s: %v", projectPath, err)
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}

	logf("warmup complete for %s (port %d)", projectPath, proc.Port)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"project_path": proc.ProjectPath,
		"port":         proc.Port,
		"started_at":   proc.StartedAt.Format(time.RFC3339),
	})
}

// proxyToInvestigator forwards a feature request to the appropriate investigator.
// Reads X-Suitcode-Project to identify the target, spawning an investigator if
// necessary. Streams the response body directly to avoid buffering large results.
func (c *Coordinator) proxyToInvestigator(w http.ResponseWriter, r *http.Request) {
	projectPath := r.Header.Get(projectPathHeader)
	if projectPath == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s header is required", projectPathHeader))
		return
	}

	proc, err := c.registry.GetOrSpawn(r.Context(), projectPath)
	if err != nil {
		logf("failed to get investigator for %s: %v", projectPath, err)
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("investigator unavailable: %w", err))
		return
	}

	// Build the target URL by forwarding the path as-is to the investigator.
	targetURL := proc.BaseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	// Create a new request to the investigator.
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("building proxy request: %w", err))
		return
	}

	// Copy relevant headers forward.
	for _, header := range []string{"Content-Type", "Accept"} {
		if v := r.Header.Get(header); v != "" {
			proxyReq.Header.Set(header, v)
		}
	}

	logf("proxy %s %s → investigator port %d", r.Method, r.URL.Path, proc.Port)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(proxyReq)
	if err != nil {
		logf("proxy error: %v", err)
		writeError(w, http.StatusBadGateway, fmt.Errorf("investigator unreachable: %w", err))
		return
	}

	// Write status and headers before touching the body.
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)

	// Stream body only when present — a nil body is valid for e.g. 204 No Content.
	if resp.Body == nil {
		return
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil {
		logf("warn: streaming response body: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP helpers
// ──────────────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		logf("warn: json encode: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
