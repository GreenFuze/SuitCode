//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// scanForInvestigators returns all running investigator processes found on
// this Windows system. It queries WMI via PowerShell's Get-CimInstance, which
// returns the full command line for every matching process.
func scanForInvestigators() ([]discoveredInvestigator, error) {
	// Get-CimInstance returns the CommandLine property for every process whose
	// name starts with "investigator". One command line per output line.
	const script = `Get-CimInstance Win32_Process |` +
		` Where-Object { $_.Name -like 'investigator*' } |` +
		` Select-Object -ExpandProperty CommandLine`

	out, err := exec.Command(
		"powershell", "-NoProfile", "-NonInteractive", "-Command", script,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("powershell: %w", err)
	}

	var found []discoveredInvestigator
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		args := splitWindowsCmdLine(line)
		if di, ok := parseInvestigatorArgs(args); ok {
			found = append(found, di)
		}
	}

	return found, nil
}

// splitWindowsCmdLine splits a Windows command-line string into arguments,
// handling double-quoted fields that may contain spaces.
func splitWindowsCmdLine(cmd string) []string {
	var args []string
	var current strings.Builder
	inQuotes := false

	for _, r := range cmd {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case r == ' ' && !inQuotes:
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}
