//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// scanForInvestigators returns all running investigator processes found on
// this Unix-like system. On Linux it reads /proc directly; on all other
// Unix-likes (macOS, BSDs) it falls back to `ps`.
func scanForInvestigators() ([]discoveredInvestigator, error) {
	if _, err := os.Stat("/proc"); err == nil {
		return scanViaProc()
	}
	return scanViaPs()
}

// scanViaProc reads /proc/<pid>/cmdline for every numeric entry in /proc.
// Each cmdline file contains the argv array with arguments separated by
// null bytes (\x00).
func scanViaProc() ([]discoveredInvestigator, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("readdir /proc: %w", err)
	}

	var found []discoveredInvestigator
	for _, entry := range entries {
		// Only numeric directory names are PID directories.
		if !entry.IsDir() {
			continue
		}
		if !isAllDigits(entry.Name()) {
			continue
		}

		cmdlineBytes, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			// Process may have exited between ReadDir and here; skip silently.
			continue
		}

		// Args are separated by null bytes; the slice may end with an empty element.
		args := strings.Split(string(cmdlineBytes), "\x00")
		if di, ok := parseInvestigatorArgs(args); ok {
			found = append(found, di)
		}
	}

	return found, nil
}

// scanViaPs runs `ps -Ao args=` which lists every process's full argument
// string (one per line, no header). Used on macOS and other non-Linux Unix
// systems that do not expose /proc.
func scanViaPs() ([]discoveredInvestigator, error) {
	// -A  — select all processes
	// -o args=  — print the full argument list; = suppresses the column header
	//             and relaxes the default column-width truncation
	out, err := exec.Command("ps", "-Ao", "args=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}

	var found []discoveredInvestigator
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if di, ok := parseInvestigatorArgs(strings.Fields(line)); ok {
			found = append(found, di)
		}
	}

	return found, nil
}

// isAllDigits reports whether s consists entirely of ASCII decimal digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, b := range s {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
