package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// projectPathHeader is the HTTP header the suitcode client sends to identify
// which repository it wants to query.
const projectPathHeader = "X-Suitcode-Project"

// InvestigatorProcess represents one running investigator subprocess for a
// specific repository. The coordinator owns its lifecycle.
type InvestigatorProcess struct {
	ProjectPath string
	Port        int
	BaseURL     string
	Cmd         *exec.Cmd // nil when the process was reattached, not spawned by us
	StartedAt   time.Time
}

// NewInvestigatorProcess constructs a fully-populated InvestigatorProcess.
// BaseURL is derived from port so callers never duplicate the formula.
// Pass a nil cmd for reattached processes the coordinator did not spawn.
func NewInvestigatorProcess(projectPath string, port int, cmd *exec.Cmd) *InvestigatorProcess {
	return &InvestigatorProcess{
		ProjectPath: projectPath,
		Port:        port,
		BaseURL:     fmt.Sprintf("http://127.0.0.1:%d", port),
		Cmd:         cmd,
		StartedAt:   time.Now(),
	}
}

// Registry maintains the mapping from repository paths to running investigator
// processes. It spawns investigators on demand and keeps them alive.
type Registry struct {
	mu             sync.RWMutex
	processes      map[string]*InvestigatorProcess // key = abs project path
	invBinary      string
	coordinatorURL string // passed to every spawned investigator via --coordinator-url
	httpClient     *http.Client
}

// NewRegistry creates a Registry that will use invBinary to spawn investigators.
// coordinatorURL is the base URL of this coordinator (e.g. "http://127.0.0.1:7878")
// and is forwarded to each investigator so it can call back home.
// It immediately scans the OS process list for investigators that survived a
// previous coordinator run and reattaches to any that are still healthy.
func NewRegistry(invBinary, coordinatorURL string) *Registry {
	r := &Registry{
		processes:      make(map[string]*InvestigatorProcess),
		invBinary:      invBinary,
		coordinatorURL: coordinatorURL,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
	}

	// Reattach to any investigators that outlived a previous coordinator crash.
	r.restoreFromProcessScan()

	return r
}

// GetOrSpawn returns the running investigator for projectPath, spawning one if
// necessary. Waits until the investigator reports ReadinessLevel >= 1.
//
// Two path-resolution rules are applied before spawning:
//
//  1. Parent-redirect: if a running investigator already serves a parent of
//     projectPath, that investigator is returned immediately. The parent has
//     already indexed the full tree that contains projectPath.
//
//  2. Child-upgrade: if running investigators serve children of projectPath,
//     they are stopped first. A parent investigator always supersedes children.
func (r *Registry) GetOrSpawn(ctx context.Context, projectPath string) (*InvestigatorProcess, error) {
	// ── Fast path: read lock only ─────────────────────────────────────────────
	//
	// Check exact match and parent-redirect without any writes.
	r.mu.RLock()
	if proc, ok := r.processes[projectPath]; ok && r.isHealthy(proc) {
		r.mu.RUnlock()
		return proc, nil
	}
	for path, proc := range r.processes {
		if isAncestorPath(path, projectPath) && r.isHealthy(proc) {
			r.mu.RUnlock()
			logf("request for %s redirected to parent investigator at %s", projectPath, path)
			return proc, nil
		}
	}
	r.mu.RUnlock()

	// ── Slow path: write lock ─────────────────────────────────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-check exact match (another goroutine may have spawned while we waited).
	if proc, ok := r.processes[projectPath]; ok && r.isHealthy(proc) {
		return proc, nil
	}
	// Re-check parent-redirect under write lock.
	for path, proc := range r.processes {
		if isAncestorPath(path, projectPath) && r.isHealthy(proc) {
			logf("request for %s redirected to parent investigator at %s (write-lock check)", projectPath, path)
			return proc, nil
		}
	}

	// Child-upgrade: stop any investigators running at children of projectPath.
	// A parent investigator always supersedes its children.
	for path, proc := range r.processes {
		if isAncestorPath(projectPath, path) {
			logf("stopping child investigator at %s (superseded by parent %s)", path, projectPath)
			if proc.Cmd != nil && proc.Cmd.Process != nil {
				_ = proc.Cmd.Process.Kill()
			} else {
				logf("deregistering reattached child investigator at %s", path)
			}
			delete(r.processes, path)
		}
	}

	// Evict stale exact entry if present (unhealthy or dead process).
	if proc, ok := r.processes[projectPath]; ok && proc != nil {
		if proc.Cmd != nil && proc.Cmd.Process != nil {
			logf("killing stale investigator for %s (port %d)", projectPath, proc.Port)
			_ = proc.Cmd.Process.Kill()
		} else {
			logf("removing stale reattached investigator for %s (port %d)", projectPath, proc.Port)
		}
		delete(r.processes, projectPath)
	}

	// Spawn a new investigator.
	proc, err := r.spawn(projectPath)
	if err != nil {
		return nil, fmt.Errorf("registry: spawn investigator for %q: %w", projectPath, err)
	}

	r.processes[projectPath] = proc

	// Wait for at least Level1 readiness (VCS identity + file index).
	if err := r.waitForReadiness(ctx, proc, 1, 30*time.Second); err != nil {
		return nil, fmt.Errorf("registry: investigator for %q did not reach level 1: %w", projectPath, err)
	}

	return proc, nil
}

// Warmup spawns the investigator and waits until it reaches ReadinessLevel3
// (import graph loaded). Designed for user-initiated pre-warming before agent work.
// The returned process may be at a parent path if parent-redirect was applied.
func (r *Registry) Warmup(ctx context.Context, projectPath string) (*InvestigatorProcess, error) {
	proc, err := r.GetOrSpawn(ctx, projectPath)
	if err != nil {
		return nil, err
	}

	// Use the actual investigator path (may differ from projectPath on redirect).
	effectivePath := proc.ProjectPath
	logf("waiting for investigator at %s to reach level 3 (import graph)...", effectivePath)
	if err := r.waitForReadiness(ctx, proc, 3, 5*time.Minute); err != nil {
		return nil, fmt.Errorf("registry: investigator for %q warmup timed out: %w", effectivePath, err)
	}

	return proc, nil
}

// List returns all registered investigator processes (snapshot).
func (r *Registry) List() []*InvestigatorProcess {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*InvestigatorProcess, 0, len(r.processes))
	for _, proc := range r.processes {
		out = append(out, proc)
	}
	return out
}

// Stop gracefully stops the investigator for projectPath.
// If the project is not registered or the process is already dead, returns nil
// (idempotent by design — the tray can call this freely).
func (r *Registry) Stop(projectPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	proc, ok := r.processes[projectPath]
	if !ok {
		// Not registered — treat as a no-op so callers don't need to check first.
		return nil
	}

	// Remove from the registry before killing so the reaper goroutine
	// (in spawn) does not find a stale entry and log a misleading message.
	delete(r.processes, projectPath)

	if proc.Cmd != nil && proc.Cmd.Process != nil {
		logf("stopping investigator for %s (port %d)", projectPath, proc.Port)
		if err := proc.Cmd.Process.Kill(); err != nil {
			return fmt.Errorf("registry: stop investigator for %q: %w", projectPath, err)
		}
	} else {
		logf("deregistering reattached investigator for %s (port %d)", projectPath, proc.Port)
	}

	return nil
}

// Shutdown kills all running investigator processes.
func (r *Registry) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for path, proc := range r.processes {
		logf("shutting down investigator for %s (port %d)", path, proc.Port)
		if proc.Cmd != nil && proc.Cmd.Process != nil {
			_ = proc.Cmd.Process.Kill()
		}
	}
	r.processes = make(map[string]*InvestigatorProcess)
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// spawn allocates a free port and starts investigator <projectPath> serve --port <port>
// [--coordinator-url <url>]. The spawned process uses context.Background() so its
// lifetime is never tied to a caller's request context.
func (r *Registry) spawn(projectPath string) (*InvestigatorProcess, error) {
	port, err := allocFreePort()
	if err != nil {
		return nil, fmt.Errorf("alloc port: %w", err)
	}

	// Build the argument list. Always include --coordinator-url so the
	// investigator knows where to reach home.
	args := []string{projectPath, "serve", "--port", strconv.Itoa(port)}
	if r.coordinatorURL != "" {
		args = append(args, "--coordinator-url", r.coordinatorURL)
	}

	// Use context.Background() — NOT the request ctx — so the investigator's
	// lifetime is decoupled from any individual HTTP request. When a request
	// completes its context is cancelled, and exec.CommandContext would kill the
	// child immediately. Shutdown is handled explicitly via Stop/Shutdown.
	cmd := exec.CommandContext(context.Background(), r.invBinary, args...)

	// Suppress console window creation. On Windows the coordinator is a
	// windowsgui binary; without CREATE_NO_WINDOW the OS opens a new console
	// window for every console-subsystem child process it spawns.
	hideChildWindow(cmd)

	// Stdio is intentionally nil — investigator writes its own logs to stderr.
	// They do not appear anywhere in the windowsgui coordinator build (no console).
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", r.invBinary, err)
	}

	proc := NewInvestigatorProcess(projectPath, port, cmd)

	logf("spawned investigator pid=%d port=%d project=%s", cmd.Process.Pid, port, projectPath)

	// Reap the process asynchronously so it doesn't become a zombie.
	go func() {
		_ = cmd.Wait()
		logf("investigator pid=%d for %s exited", cmd.Process.Pid, projectPath)
		// Remove from registry on unexpected exit.
		r.mu.Lock()
		if existing, ok := r.processes[projectPath]; ok && existing == proc {
			delete(r.processes, projectPath)
		}
		r.mu.Unlock()
	}()

	return proc, nil
}

// isHealthy pings the investigator's /health endpoint with a short timeout.
func (r *Registry) isHealthy(proc *InvestigatorProcess) bool {
	resp, err := r.httpClient.Get(proc.BaseURL + "/api/v1/health")
	if err != nil {
		return false
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	return resp.StatusCode == http.StatusOK
}

// waitForReadiness polls the investigator's /health endpoint until it reports
// readiness_level >= minLevel, or until the deadline expires.
func (r *Registry) waitForReadiness(ctx context.Context, proc *InvestigatorProcess, minLevel int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pollInterval := 200 * time.Millisecond

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		level, err := r.readinessLevel(proc)
		if err == nil && level >= minLevel {
			logf("investigator at %s reached level %d", proc.ProjectPath, level)
			return nil
		}

		time.Sleep(pollInterval)
		// Back off slightly to avoid hammering.
		if pollInterval < 2*time.Second {
			pollInterval = time.Duration(float64(pollInterval) * 1.2)
		}
	}

	return fmt.Errorf("timed out after %s waiting for level %d", timeout, minLevel)
}

// readinessLevel fetches the current readiness_level from the investigator.
func (r *Registry) readinessLevel(proc *InvestigatorProcess) (int, error) {
	resp, err := r.httpClient.Get(proc.BaseURL + "/api/v1/health")
	if err != nil {
		return 0, err
	}
	if resp.Body == nil {
		return 0, fmt.Errorf("health: empty response body (status %d)", resp.StatusCode)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("health returned status %d", resp.StatusCode)
	}

	var body struct {
		ReadinessLevel int `json:"readiness_level"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode health: %w", err)
	}

	return body.ReadinessLevel, nil
}

// isAncestorPath reports whether ancestor is a proper parent (or grandparent,
// etc.) of descendant in the filesystem hierarchy. Both paths must be clean
// absolute paths. Uses filepath.Rel to handle platform separators correctly.
//
// Returns false when ancestor == descendant (not a *proper* ancestor).
func isAncestorPath(ancestor, descendant string) bool {
	rel, err := filepath.Rel(ancestor, descendant)
	if err != nil {
		return false
	}
	// filepath.Rel returns "." when the paths are equal, and a ".." prefixed
	// string when ancestor is not a real ancestor of descendant.
	return rel != "." && !strings.HasPrefix(rel, "..")
}

// allocFreePort asks the OS for a free TCP port by opening a listener at :0,
// recording the assigned port, then closing the listener.
func allocFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("alloc free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, fmt.Errorf("alloc free port: close: %w", err)
	}
	return port, nil
}
