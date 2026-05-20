// Package main is the entry point for the SuitCode investigator daemon.
//
// The investigator is a long-running per-project process that owns gopls,
// the import graph, and the file index for one repository. Its primary mode
// is "serve": it exposes an HTTP API and keeps providers warm between calls.
//
// The coordinator spawns investigators automatically; end users and agents
// do not interact with the investigator directly.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// usage is the top-level help text shown when the directory argument is missing.
const usage = `SuitCode Investigator — per-project repository intelligence daemon

USAGE:
  investigator <repo-path> <command> [flags]

ARGUMENTS:
  repo-path   Path to the repository to analyse (required).
              May be relative (../myrepo) or absolute (/home/user/myrepo).

COMMANDS:
  status             Show readiness status for the repository
  repo-overview      Repository structure and technology overview
  explain-file       Explain a file's role, imports, tests, and relationships
  related            Find files related to a given file
  tests              Find tests relevant to a source file or change
  impact             Blast radius analysis for a set of changes
  context            Compile a bounded context capsule for a set of files
  failure-context    Extract useful context from a failure log
  verify-plan        Generate a verification plan for a set of changes
  serve              Start the HTTP API server for this repository (primary mode)
  eval               Run evaluation suites against this repository
  metrics            Show or export per-call metrics from .suitcode/calls.jsonl

Run 'investigator <repo-path> <command> --help' for per-command flags.

EXAMPLES:
  investigator . status
  investigator /path/to/myrepo repo-overview --budget 3000
  investigator . explain-file --path internal/server/main.go
  investigator . context --files internal/foo.go,internal/bar.go --budget 8000
  investigator . serve --port 54321
  investigator . metrics show
`

func main() {
	// Pre-processing: strip and validate the mandatory <repo-path> first argument
	// before cobra sees os.Args.
	if len(os.Args) < 2 || strings.HasPrefix(os.Args[1], "-") {
		fmt.Fprint(os.Stderr, usage)
		fmt.Fprintln(os.Stderr, "error: repo-path is required as the first argument")
		os.Exit(1)
	}

	rawPath := os.Args[1]
	repoPath, err := filepath.Abs(rawPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot resolve path %q: %v\n", rawPath, err)
		os.Exit(1)
	}

	info, err := os.Stat(repoPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error: directory does not exist: %s\n", repoPath)
		} else {
			fmt.Fprintf(os.Stderr, "error: cannot access %s: %v\n", repoPath, err)
		}
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: %s is a file, not a directory\n", repoPath)
		os.Exit(1)
	}

	// Disable cobra's Windows "double-click" nag.
	cobra.MousetrapHelpText = ""

	// Splice the directory out of os.Args so cobra sees only
	// [progname, subcommand, ...flags].
	os.Args = append(os.Args[:1], os.Args[2:]...)

	cmd := NewRootCmd(repoPath)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
