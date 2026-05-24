package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
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
func (r *Registry) GetOrSpawn(ctx context.Context, projectPath string) (*InvestigatorProcess, error) {
	// Fast path: check if already running and healthy.
	r.mu.RLock()
	proc, ok := r.processes[projectPath]
	r.mu.RUnlock()

	if ok && r.isHealthy(proc) {
		return proc, nil
	}

	// Slow path: acquire write lock and spawn.
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after lock upgrade — another goroutine may have spawned it.
	proc, ok = r.processes[projectPath]
	if ok && r.isHealthy(proc) {
		return proc, nil
	}

	// Evict the stale entry. Kill it if we own the process (Cmd != nil);
	// reattached investigators (Cmd == nil) are simply de-registered.
	if ok && proc != nil {
		if proc.Cmd != nil && proc.Cmd.Process != nil {
			logf("killing stale investigator for %s (port %d)", projectPath, proc.Port)
			_ = proc.Cmd.Process.Kill()
		} else {
			logf("removing stale reattached investigator for %s (port %d)", projectPath, proc.Port)
		}
		delete(r.processes, projectPath)
	}

	// Spawn a new investigator.
	proc, err := r.spawn(ctx, projectPath)
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
func (r *Registry) Warmup(ctx context.Context, projectPath string) (*InvestigatorProcess, error) {
	proc, err := r.GetOrSpawn(ctx, projectPath)
	if err != nil {
		return nil, err
	}

	logf("waiting for investigator at %s to reach level 3 (import graph)...", projectPath)
	if err := r.waitForReadiness(ctx, proc, 3, 5*time.Minute); err != nil {
		return nil, fmt.Errorf("registry: investigator for %q warmup timed out: %w", projectPath, err)
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
// [--coordinator-url <url>].
func (r *Registry) spawn(ctx context.Context, projectPath string) (*InvestigatorProcess, error) {
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

	cmd := exec.CommandContext(ctx, r.invBinary, args...)
	// Stdio is intentionally nil — investigator writes its own [sc investigator] logs to stderr.
	// We don't capture them here; they appear in the coordinator's terminal.
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
