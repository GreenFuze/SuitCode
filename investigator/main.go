package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// usage is the top-level help text shown when the directory argument is missing
// or invalid. It is printed to stderr so it doesn't pollute stdout pipelines.
const usage = `SuitCode Investigator — local repository intelligence

USAGE:
  investigator <repo-path> <command> [flags]

ARGUMENTS:
  repo-path   Path to the repository to analyse (required).
              May be relative (../myrepo) or absolute (/home/user/myrepo).
              Unlike many CLIs, this tool does NOT default to the current
              directory — the path must be given explicitly.

COMMANDS:
  status             Show investigator readiness for the repository
  repo-overview      Repository structure and technology overview
  explain-file       Explain a file's role, imports, tests, and relationships
  related            Find files related to a given file
  tests              Find tests relevant to a source file or change
  impact             Blast radius analysis for a set of changes
  context            Compile a bounded context capsule for a set of files
  failure-context    Extract useful context from a failure log
  verify-plan        Generate a verification plan for a set of changes
  serve              Start the HTTP API server for this repository
  eval               Run evaluation suites against this repository

Run 'investigator <repo-path> <command> --help' for per-command flags.

EXAMPLES:
  investigator . status
  investigator /path/to/myrepo repo-overview --budget 3000
  investigator . explain-file --path internal/server/main.go
  investigator . context --files internal/foo.go,internal/bar.go --budget 8000
  investigator . eval run --suite smoke
  investigator . serve --port 7878
`

func main() {
	// Pre-processing: strip and validate the mandatory <repo-path> first argument
	// before cobra sees os.Args. This allows cobra to use a plain subcommand name
	// in position 1 without confusing it with a directory path.

	if len(os.Args) < 2 || strings.HasPrefix(os.Args[1], "-") {
		fmt.Fprint(os.Stderr, usage)
		fmt.Fprintln(os.Stderr, "error: repo-path is required as the first argument")
		os.Exit(1)
	}

	// Resolve the path early so errors are clear and unambiguous.
	rawPath := os.Args[1]
	repoPath, err := filepath.Abs(rawPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot resolve path %q: %v\n", rawPath, err)
		os.Exit(1)
	}

	// Validate it is an existing directory.
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

	// Disable cobra's Windows "double-click" nag — this is a developer CLI,
	// always invoked from a terminal. The mousetrap dep stays (cobra owns it)
	// but the behaviour is neutralised.
	cobra.MousetrapHelpText = ""

	// Splice the directory out of os.Args so cobra sees only
	// [progname, subcommand, ...flags].
	os.Args = append(os.Args[:1], os.Args[2:]...)

	// Hand off to the cobra command tree.
	cmd := NewRootCmd(repoPath)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
