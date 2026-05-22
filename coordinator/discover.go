package main

import (
	"path/filepath"
	"strconv"
	"strings"
)

// discoveredInvestigator holds the information parsed from a running
// investigator process found during an OS process scan.
type discoveredInvestigator struct {
	ProjectPath string
	Port        int
}

// parseInvestigatorArgs attempts to extract project path and port from a
// process's argument list. Returns (result, true) on success.
//
// Expected command-line form:
//
//	[/path/to/]investigator[.exe] <project-path> serve --port <n>
func parseInvestigatorArgs(args []string) (discoveredInvestigator, bool) {
	// Strip trailing empty elements left by null-terminated cmdline reads.
	for len(args) > 0 && args[len(args)-1] == "" {
		args = args[:len(args)-1]
	}

	// Need at least: binary project-path "serve" "--port" portNum
	if len(args) < 5 {
		return discoveredInvestigator{}, false
	}

	// args[0] must be a path whose base name is "investigator" or "investigator.exe".
	base := strings.ToLower(filepath.Base(args[0]))
	if base != "investigator" && base != "investigator.exe" {
		return discoveredInvestigator{}, false
	}

	// args[2] must be the "serve" subcommand.
	if args[2] != "serve" {
		return discoveredInvestigator{}, false
	}

	// Find --port anywhere in args[3:].
	projectPath := args[1]
	for i := 3; i < len(args)-1; i++ {
		if args[i] == "--port" {
			port, err := strconv.Atoi(args[i+1])
			if err != nil || port <= 0 || port > 65535 {
				return discoveredInvestigator{}, false
			}
			return discoveredInvestigator{ProjectPath: projectPath, Port: port}, true
		}
	}

	return discoveredInvestigator{}, false
}

// ──────────────────────────────────────────────────────────────────────────────
// Registry reattach
// ──────────────────────────────────────────────────────────────────────────────

// restoreFromProcessScan scans the OS process list for running investigator
// processes and re-registers those that pass a health-check. This allows the
// coordinator to reattach to investigators that survived its own crash.
//
// Re-attached entries have a nil Cmd — the coordinator can route to them and
// health-check them, but cannot kill them on Shutdown. If one exits, the next
// GetOrSpawn call will detect it as unhealthy and spawn a fresh one.
func (r *Registry) restoreFromProcessScan() {
	discovered, err := scanForInvestigators()
	if err != nil {
		logf("warn: process scan: %v", err)
		return
	}
	if len(discovered) == 0 {
		return
	}

	logf("process scan found %d investigator candidate(s) — health-checking...", len(discovered))

	restored := 0
	for _, d := range discovered {
		// Skip any path that is already registered (defensive; shouldn't happen
		// on a fresh startup).
		r.mu.RLock()
		_, exists := r.processes[d.ProjectPath]
		r.mu.RUnlock()
		if exists {
			continue
		}

		// Cmd is nil — the coordinator did not spawn this process.
		proc := NewInvestigatorProcess(d.ProjectPath, d.Port, nil)

		if !r.isHealthy(proc) {
			logf("skip %s (port %d): investigator not responding", d.ProjectPath, d.Port)
			continue
		}

		r.mu.Lock()
		r.processes[d.ProjectPath] = proc
		r.mu.Unlock()

		logf("reattached to investigator for %s at port %d", d.ProjectPath, d.Port)
		restored++
	}

	if restored > 0 {
		logf("reattached to %d investigator(s) from previous run", restored)
	}
}
