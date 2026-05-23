package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
)

// Server wraps a ProjectInvestigator behind an HTTP API.
// It owns the http.Server and is responsible for its lifecycle.
type Server struct {
	inv            *ProjectInvestigator
	port           int
	coordinatorURL string // base URL of the coordinator that spawned us (may be empty)
	http           *http.Server
}

// NewServer constructs a Server for the given investigator, port, and optional
// coordinator URL. coordinatorURL is the base URL of the coordinator that spawned
// this investigator (e.g. "http://127.0.0.1:7878"). It is empty when the
// investigator is run directly by a developer rather than by the coordinator.
// Routes are registered immediately; the server does not start listening
// until ListenAndServe is called.
func NewServer(inv *ProjectInvestigator, port int, coordinatorURL string) *Server {
	s := &Server{inv: inv, port: port, coordinatorURL: coordinatorURL}

	r := chi.NewRouter()

	// Middleware: logging, recovery, content-type.
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(middleware.SetHeader("Content-Type", "application/json"))

	// Feature routes.
	r.Get("/api/v1/health", s.handleHealth)
	r.Post("/api/v1/repo-overview", s.handleRepoOverview)
	r.Post("/api/v1/explain-file", s.handleExplainFile)
	r.Post("/api/v1/related", s.handleRelated)
	r.Post("/api/v1/tests", s.handleTests)
	r.Post("/api/v1/impact", s.handleImpact)
	r.Post("/api/v1/context", s.handleContext)
	r.Post("/api/v1/failure-context", s.handleFailureContext)
	r.Post("/api/v1/verify-plan", s.handleVerifyPlan)

	s.http = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s
}

// ListenAndServe starts the HTTP server and blocks until it exits.
// Use Shutdown for graceful stop.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("server: listen on %s: %w", s.http.Addr, err)
	}

	logf("HTTP API listening on http://localhost:%d", s.port)

	// Log coordinator relationship so it is visible in the process's stderr.
	if s.coordinatorURL != "" {
		logf("coordinator: %s", s.coordinatorURL)
	} else {
		logf("coordinator: none (running standalone)")
	}

	return s.http.Serve(ln)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP response types
// ──────────────────────────────────────────────────────────────────────────────

// investigatorHealthPayload is the body returned by GET /api/v1/health.
type investigatorHealthPayload struct {
	OK             bool   `json:"ok"`
	Repo           string `json:"repo"`
	Ready          string `json:"ready"`
	ReadinessLevel int    `json:"readiness_level"`
	Version        string `json:"version"`
}

// errorPayload is the body returned for any error response.
type errorPayload struct {
	Error string `json:"error"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Handlers
// ──────────────────────────────────────────────────────────────────────────────

// handleHealth returns current readiness so the coordinator can poll warmup
// progress. The readiness_level integer field enables threshold checks.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	st := s.inv.Status()
	writeJSON(w, http.StatusOK, investigatorHealthPayload{
		OK:             true,
		Repo:           st.RepoPath,
		Ready:          st.ReadinessDesc,
		ReadinessLevel: int(st.Readiness),
		Version:        "v1",
	})
}

func (s *Server) handleRepoOverview(w http.ResponseWriter, r *http.Request) {
	var req cfeatures.RepoOverviewRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.RepoPath = s.inv.repoPath

	resp, err := s.inv.RepoOverview(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleExplainFile(w http.ResponseWriter, r *http.Request) {
	var req cfeatures.ExplainFileRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.RepoPath = s.inv.repoPath

	resp, err := s.inv.ExplainFile(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRelated(w http.ResponseWriter, r *http.Request) {
	var req cfeatures.RelatedRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.RepoPath = s.inv.repoPath

	resp, err := s.inv.Related(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTests(w http.ResponseWriter, r *http.Request) {
	var req cfeatures.TestsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.RepoPath = s.inv.repoPath

	resp, err := s.inv.Tests(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleImpact(w http.ResponseWriter, r *http.Request) {
	var req cfeatures.ImpactRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.RepoPath = s.inv.repoPath

	resp, err := s.inv.Impact(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	var req cfeatures.ContextRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.RepoPath = s.inv.repoPath

	resp, err := s.inv.Context(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleFailureContext(w http.ResponseWriter, r *http.Request) {
	var req cfeatures.FailureContextRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.RepoPath = s.inv.repoPath

	resp, err := s.inv.FailureContext(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleVerifyPlan(w http.ResponseWriter, r *http.Request) {
	var req cfeatures.VerifyPlanRequest
	if !decodeBody(w, r, &req) {
		return
	}
	req.RepoPath = s.inv.repoPath

	resp, err := s.inv.VerifyPlan(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP helpers
// ──────────────────────────────────────────────────────────────────────────────

// decodeBody decodes a JSON request body into dst. Returns false and writes a
// 400 response on failure; the caller should return immediately.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return false
	}
	return true
}

// writeJSON encodes v as pretty JSON and sends it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		logf("warn: json encode error: %v", err)
	}
}

// writeError sends a JSON error response.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorPayload{Error: err.Error()})
}
