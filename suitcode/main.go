// Package main is the suitcode thin client — the ONLY user and agent interface.
//
// suitcode communicates with the coordinator daemon (port :7878 by default).
// If the coordinator is not running, suitcode auto-starts it. The coordinator
// in turn spawns per-project investigator processes on demand.
//
// Usage:
//
//	suitcode <repo-path> <command> [flags]
//
// The first argument MUST be a path (relative or absolute) to the repository.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/GreenFuze/SuitCode/calllog"
	"github.com/GreenFuze/SuitCode/core/config"
	cfeatures "github.com/GreenFuze/SuitCode/core/features"
)

const defaultCoordinatorURL = "http://127.0.0.1:7878"

// logCalls is set by the --log-calls persistent flag. When true, every feature
// call prints a compact one-line metric summary to stderr even when --format json
// is used. Useful for monitoring agent sessions without polluting JSON output.
var logCalls bool

// outputFile is set by the --output persistent flag. When non-empty, JSON
// output is written to this file instead of stdout. This lets agents avoid
// PowerShell's broken pipe behaviour: instead of piping suitcode into
// ConvertFrom-Json, write to a temp file and read it afterwards.
//   suitcode . context --files foo.go --format json --output result.json
//   $r = Get-Content result.json | ConvertFrom-Json
var outputFile string

// usage is the full reference shown for --help / -h / usage / bare invocation.
const usage = `SuitCode — deterministic repository intelligence for coding agents

Connects to a per-project investigator daemon (auto-started on first use) to
answer structural questions about a repository: import graphs, related files,
test mappings, context capsules, blast-radius analysis, and verification plans.

USAGE:
  suitcode <repo-path> <command> [flags]

  <repo-path> is always the first argument. It may be relative (".", "../x")
  or absolute. SuitCode never defaults to the current directory.

WORKFLOW:
  Run "warmup" once at the start of a session to load the import graph and
  start gopls. All other commands work without it, but return richer results
  once the investigator is fully initialized (~30–90 s on first run).

    suitcode . warmup

COMMANDS:
  status           Show whether the coordinator and investigator are running
                   and at what readiness level.

  warmup           Ensure the investigator is fully initialized (import
                   graph + gopls ready). Blocks until ready. Idempotent —
                   safe to call even if already warm.

  repo-overview    High-level map of the repository: detected languages,
                   build systems, top-level layout, and notable directories.

  explain-file     Role, imports, reverse-dependents, related tests, and
                   exported symbols for a single file.
                     --path <file>  [required]

  symbols          Symbols defined in a specific file: functions, types,
                   variables, constants. Uses gopls for Go, Roslyn for C#
                   (when available). Returns a "not_implemented" limitation
                   for languages without a symbol server.
                     --path <file>  [required]
                     --filter <pattern>  case-insensitive substring match

  related          Files most related to a seed file, ranked by import-graph
                   proximity and naming heuristics.
                     --path <file>  [required]

  tests            Test files and ready-to-run test commands relevant to a
                   source file or to all files changed since a git ref.
                     --path <file>        (tests for one file)
                     --from <git-ref>     (tests for all changes since ref)

  impact           Blast-radius analysis for a set of changed files:
                   downstream importers, affected tests, risky interface
                   boundaries, generated-file warnings.
                     --files <f1,f2,...>  or  --from <git-ref>  [one required]

  context          Compile a token-budgeted context capsule for a set of seed
                   files. This is the primary "gather context before editing"
                   command. Candidates are scored and ranked by relevance, then
                   trimmed to fit the budget.
                     --files <f1,f2,...>  or  --from <git-ref>  [one required]
                     --budget <tokens>    (default 8000)

  failure-context  Extract structured context from a build or test failure log:
                   suspected source files, test names, and a bounded context
                   capsule for the failure site.
                     --log <path-to-failure-output>  [required]

  verify-plan      Generate a verification checklist (build, test, vet, lint
                   commands) that covers the changed files.
                     --files <f1,f2,...>  or  --from <git-ref>  [one required]

  metrics          Show or export per-call timing and token-budget statistics.
    summary          Condensed session overview (errors, warnings, latency,
                     token budget, compression ratio). ~15 lines. Copy-paste
                     this to transfer analytics across air-gapped machines.
                       --last N   limit to the most recent N records (0 = all)
    show             Per-call table of recent calls (--last N, default 50)
    export           Package the call log as a shareable zip

OUTPUT:
  By default every command prints a one-line summary to stdout and emits
  timing/budget/hash diagnostics to stderr.

  Pass --format json to receive the full structured response on stdout.
  Agents should use --format json for programmatic use — the JSON contains
  all evidence, provenance, metrics, and limitation notices.

  NOTE: SuitCode writes progress lines to stderr and JSON to stdout.
  In bash/zsh, suppress stderr when piping JSON:
    suitcode . context --files f.go --format json 2>/dev/null | jq .

  In PowerShell, piping may fail ("pipe being closed"). Use --output instead:
    suitcode . context --files f.go --format json --output result.json
    $r = Get-Content result.json | ConvertFrom-Json

GLOBAL FLAGS:
  --log-calls     Print a compact metric line to stderr after each feature call,
                  even in --format json mode. Useful for monitoring an agent
                  session: suitcode . context --files ... --format json --log-calls

  --output <file> Write --format json output to a file instead of stdout.
                  Avoids PowerShell "pipe being closed" errors. The file is
                  created/truncated; its parent directory must already exist.

EXAMPLES:
  # Pre-warm once per coding session (do this first)
  suitcode /path/to/repo warmup

  # Understand the overall repository structure
  suitcode . repo-overview --format json

  # Gather bounded context before editing a file
  suitcode . context --files internal/auth/token.go --budget 8000

  # Gather context for everything changed since branching from main
  suitcode . context --from main --budget 12000 --format json

  # Explain what a specific file does
  suitcode . explain-file --path internal/auth/token.go --format json

  # Find tests to run after changing a file
  suitcode . tests --path internal/auth/token.go

  # Find all tests affected by commits since main
  suitcode . tests --from main --format json

  # Understand the blast radius of a set of changes
  suitcode . impact --files internal/auth/token.go,internal/auth/session.go

  # Diagnose a failing CI run (save the log output to a file first)
  suitcode . failure-context --log /tmp/ci-failure.txt --format json

  # Generate a pre-PR verification checklist
  suitcode . verify-plan --from main --format json

Run 'suitcode <repo-path> <command> --help' for per-command flags.
`

// ──────────────────────────────────────────────────────────────────────────────
// Entry point
// ──────────────────────────────────────────────────────────────────────────────

// isHelpArg reports whether the argument is a recognised help/usage request.
func isHelpArg(s string) bool {
	switch s {
	case "-h", "--help", "help", "usage":
		return true
	}
	return false
}

func main() {
	// Bare invocation or explicit help request → print usage cleanly and exit 0.
	if len(os.Args) < 2 || isHelpArg(os.Args[1]) {
		fmt.Print(usage)
		os.Exit(0)
	}

	// Fail fast if the first arg looks like a flag rather than a path.
	if strings.HasPrefix(os.Args[1], "-") {
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

	// ── Subdirectory warning ──────────────────────────────────────────────────
	//
	// Walk up the directory tree looking for a .git directory. If one is found
	// at a parent of repoPath the user is most likely running from a
	// sub-directory of a larger project; warn so they understand what project
	// root SuitCode is using. This does NOT abort — the user may intentionally
	// run from a sub-project directory (e.g. a monorepo package).
	if gitRoot := findGitRoot(repoPath); gitRoot != "" && gitRoot != repoPath {
		fmt.Fprintf(os.Stderr,
			"warning: using %q as project root, but .git is at %q\n"+
				"         if this is unintentional, run: suitcode %s <command>\n",
			repoPath, gitRoot, gitRoot)
	}

	cobra.MousetrapHelpText = ""

	// Splice the directory out of os.Args so cobra sees [progname, subcommand, ...flags].
	os.Args = append(os.Args[:1], os.Args[2:]...)

	cmd := newRootCmd(repoPath)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd builds the cobra command tree.
func newRootCmd(repoPath string) *cobra.Command {
	root := &cobra.Command{
		Use:           "suitcode",
		Short:         "SuitCode — local repository intelligence for coding agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Show full usage when the user supplies a valid repo-path but no subcommand.
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Print(usage)
		},
	}

	// --log-calls: persistent flag that enables per-call metric lines on stderr.
	// Works alongside both --format json (agent use) and the default brief output.
	root.PersistentFlags().BoolVar(&logCalls, "log-calls", false,
		"print a compact metric line to stderr after each feature call (useful for monitoring agent sessions)")

	// --output: redirect JSON to a file instead of stdout, avoiding PowerShell
	// pipe issues. The file is created/truncated; parent directory must exist.
	root.PersistentFlags().StringVar(&outputFile, "output", "",
		"write --format json output to this file instead of stdout (avoids PowerShell pipe issues)")

	root.AddCommand(
		newStatusCmd(repoPath),
		newWarmupCmd(repoPath),
		newRepoOverviewCmd(repoPath),
		newExplainFileCmd(repoPath),
		newSymbolsCmd(repoPath),
		newRelatedCmd(repoPath),
		newTestsCmd(repoPath),
		newImpactCmd(repoPath),
		newContextCmd(repoPath),
		newFailureContextCmd(repoPath),
		newVerifyPlanCmd(repoPath),
		newMetricsCmd(repoPath),
	)

	return root
}

// ──────────────────────────────────────────────────────────────────────────────
// status
// ──────────────────────────────────────────────────────────────────────────────

func newStatusCmd(repoPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show coordinator + investigator readiness status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Ensure the coordinator is running before querying health.
			stopCoord := logProgress("connecting to coordinator...")
			err := ensureCoordinator(defaultCoordinatorURL)
			stopCoord()
			if err != nil {
				return err
			}

			client := NewCoordinatorClient(defaultCoordinatorURL, repoPath)

			stopHealth := logProgress("checking coordinator health...")
			health, err := client.GetHealth(cmd.Context())
			stopHealth()
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}

			fmt.Printf("Coordinator: OK (projects=%d)\n", health.Projects)
			fmt.Printf("Project:     %s\n", repoPath)
			return nil
		},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// warmup
// ──────────────────────────────────────────────────────────────────────────────

func newWarmupCmd(repoPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "warmup",
		Short: "Pre-warm the investigator (spawns it and waits for level 3 readiness)",
		Long: `Spawns the investigator for this project and waits until it reaches
readiness level 3 (import graph + gopls ready). This can take 30–90 seconds
on first run. Subsequent calls return immediately.

Run this before a coding session to avoid cold-start latency on the first
agent invocation.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			start := time.Now()

			stopWarm := logProgress("waiting for investigator to warm up and reach level 3 readiness (import graph + gopls)...")
			err = client.Warmup(cmd.Context())
			stopWarm()
			if err != nil {
				return fmt.Errorf("warmup: %w", err)
			}

			elapsed := time.Since(start).Round(time.Millisecond)
			logf("warmup complete in %s", elapsed)
			fmt.Printf("investigator warm (took %s) · project: %s\n", elapsed, repoPath)
			return nil
		},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature commands — proxy to coordinator
// ──────────────────────────────────────────────────────────────────────────────

func newRepoOverviewCmd(repoPath string) *cobra.Command {
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "repo-overview",
		Short: "Repository structure and technology overview",
		RunE: func(cmd *cobra.Command, _ []string) error {
			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			req := cfeatures.RepoOverviewRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
			}

			stopFeature := logProgress("computing repo-overview...")
			resp, err := client.RepoOverview(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.RepoOverviewResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Repository overview: %d files · %d languages · %d build systems\n",
					resp.TotalFiles, len(resp.Languages), len(resp.BuildSystems))
			}, func() {
				printCallLog("repo-overview", resp.BaseFeatureResponse, 0, 0, 0)
			})
		},
	}

	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget (0 = default)")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
	return cmd
}

func newExplainFileCmd(repoPath string) *cobra.Command {
	var path string
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "explain-file [path]",
		Short: "Explain a file's role, imports, tests, and relationships",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Accept the file path as a positional argument when --path is not set.
			if path == "" && len(args) > 0 {
				path = args[0]
			}
			if path == "" {
				return fmt.Errorf("--path is required (or pass the file path as a positional argument, e.g. explain-file src/Foo.cs)")
			}

			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			req := cfeatures.ExplainFileRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
			}

			stopFeature := logProgress(fmt.Sprintf("computing explain-file for %s...", path))
			resp, err := client.ExplainFile(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.ExplainFileResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("File explanation: %s · %d tokens\n", filepath.Base(path), resp.Metrics.Budget.Used)
			}, func() {
				printCallLog("explain-file", resp.BaseFeatureResponse, 0, 0, 0)
			})
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "file path to explain [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
	return cmd
}

func newSymbolsCmd(repoPath string) *cobra.Command {
	var path string
	var filter string
	var format string

	cmd := &cobra.Command{
		Use:   "symbols [path]",
		Short: "List symbols defined in a specific file (functions, types, variables, constants)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Accept the file path as a positional argument when --path is not set.
			if path == "" && len(args) > 0 {
				path = args[0]
			}
			if path == "" {
				return fmt.Errorf("--path is required (or pass the file path as a positional argument, e.g. symbols src/Foo.cs)")
			}

			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			// Reuse ExplainFile — it already fetches symbols via the language providers.
			req := cfeatures.ExplainFileRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
			}

			stopFeature := logProgress(fmt.Sprintf("fetching symbols for %s...", path))
			resp, err := client.ExplainFile(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			// Apply optional case-insensitive substring filter to the symbol list.
			symbols := resp.Symbols
			if filter != "" {
				filterLower := strings.ToLower(filter)
				filtered := symbols[:0]
				for _, s := range symbols {
					if strings.Contains(strings.ToLower(s.Name), filterLower) {
						filtered = append(filtered, s)
					}
				}
				symbols = filtered
			}

			if format == "json" {
				// Emit only the symbols-relevant fields to keep the output focused.
				type symbolsOutput struct {
					FilePath             string                       `json:"file_path"`
					Language             string                       `json:"language"`
					Symbols              []cfeatures.SymbolInfo       `json:"symbols"`
					ExternalDependencies []cfeatures.ExternalDependency `json:"external_dependencies,omitempty"`
					Limitations          []any                        `json:"limitations,omitempty"`
				}

				// Build limitations from the base response.
				var lims []any
				for _, l := range resp.BaseFeatureResponse.Limitations {
					lims = append(lims, l)
				}

				out := symbolsOutput{
					FilePath:             resp.FilePath,
					Language:             resp.Language,
					Symbols:              symbols,
					ExternalDependencies: resp.ExternalDependencies,
					Limitations:          lims,
				}
				if logCalls {
					printCallLog("symbols", resp.BaseFeatureResponse, 0, 0, 0)
				}
				return writeJSON(out)
			}

			// Brief (non-JSON) mode: print each matching symbol name to stdout.
			printProgress(resp.BaseFeatureResponse)

			if len(symbols) == 0 {
				// When no symbols but NuGet packages are present, surface them.
				if len(resp.ExternalDependencies) > 0 {
					fmt.Printf("No source symbols found. Project NuGet packages:")
					for _, dep := range resp.ExternalDependencies {
						if dep.Version != "" {
							fmt.Printf(" %s@%s", dep.Name, dep.Version)
						} else {
							fmt.Printf(" %s", dep.Name)
						}
					}
					fmt.Println()
				} else {
					fmt.Printf("No symbols found in %s\n", filepath.Base(path))
				}
			} else {
				for _, s := range symbols {
					if s.Kind != "" {
						fmt.Printf("%s %s\n", s.Kind, s.Name)
					} else {
						fmt.Println(s.Name)
					}
				}
			}

			if logCalls {
				printCallLog("symbols", resp.BaseFeatureResponse, 0, 0, 0)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "file path to list symbols for [required]")
	cmd.Flags().StringVar(&filter, "filter", "", "case-insensitive substring filter for symbol names")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief list)")
	return cmd
}

func newRelatedCmd(repoPath string) *cobra.Command {
	var path string
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "related",
		Short: "Find files related to a given file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return fmt.Errorf("--path is required")
			}

			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			req := cfeatures.RelatedRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
			}

			stopFeature := logProgress(fmt.Sprintf("computing related files for %s...", path))
			resp, err := client.Related(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.RelatedResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Related files: %d found · %d tokens\n", len(resp.RelatedFiles), resp.Metrics.Budget.Used)
			}, func() {
				printCallLog("related", resp.BaseFeatureResponse, 0, 0, 0)
			})
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "source file [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
	return cmd
}

func newTestsCmd(repoPath string) *cobra.Command {
	var path string
	var from string
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "tests",
		Short: "Find tests relevant to a source file or change",
		RunE: func(cmd *cobra.Command, _ []string) error {
			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			req := cfeatures.TestsRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
				DiffRef:  from,
			}

			stopFeature := logProgress("computing relevant tests...")
			resp, err := client.Tests(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.TestsResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Relevant tests: %d found · %d tokens\n", len(resp.RelevantTests), resp.Metrics.Budget.Used)
			}, func() {
				printCallLog("tests", resp.BaseFeatureResponse, 0, 0, 0)
			})
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "source file to find tests for")
	cmd.Flags().StringVar(&from, "from", "", "git ref: find tests for files changed since this ref")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
	return cmd
}

func newImpactCmd(repoPath string) *cobra.Command {
	var from string
	var files string
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "impact",
		Short: "Blast radius analysis for a set of changes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" && files == "" {
				return fmt.Errorf("--from or --files is required")
			}

			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			req := cfeatures.ImpactRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				GitRef: from,
			}
			if files != "" {
				req.FilePaths = splitComma(files)
			}

			stopFeature := logProgress("computing blast radius...")
			resp, err := client.Impact(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.ImpactResponse) {
				printProgress(resp.BaseFeatureResponse)

				// Distinguish import-graph-backed results from proximity heuristic.
				// When the no_import_graph limitation is present, impacted_files are
				// same-directory neighbours only — not real downstream importers.
				hasImportGraph := true
				for _, lim := range resp.Limitations {
					if lim.Kind == "no_import_graph" {
						hasImportGraph = false
						break
					}
				}

				if hasImportGraph {
					fmt.Printf("Impact: %d downstream files · %d tokens\n",
						len(resp.ImpactedFiles), resp.Metrics.Budget.Used)
				} else {
					fmt.Printf("Impact: %d nearby files (proximity heuristic; no import graph — see limitations) · %d tokens\n",
						len(resp.ImpactedFiles), resp.Metrics.Budget.Used)
				}
			}, func() {
				printCallLog("impact", resp.BaseFeatureResponse, 0, 0, 0)
			})
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "git ref: analyse changes since this ref")
	cmd.Flags().StringVar(&files, "files", "", "comma-separated list of changed file paths")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
	return cmd
}

func newContextCmd(repoPath string) *cobra.Command {
	var files string
	var from string
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "context",
		Short: "Compile a bounded context capsule for a set of files",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if files == "" && from == "" {
				return fmt.Errorf("--files or --from is required")
			}

			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			req := cfeatures.ContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				DiffRef: from,
			}
			if files != "" {
				req.Files = splitComma(files)
			}

			stopFeature := logProgress("compiling context capsule...")
			resp, err := client.Context(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.ContextResponse) {
				printProgress(resp.BaseFeatureResponse)

				// Check whether the structurally-related set exceeds the budget.
				overBudget := false
				for _, lim := range resp.Limitations {
					if lim.Kind == "over_budget" {
						overBudget = true
						break
					}
				}

				if overBudget {
					overage := resp.Metrics.Budget.Used - resp.Metrics.Budget.Requested
					overagePct := 0
					if resp.Metrics.Budget.Requested > 0 {
						overagePct = int(float64(overage) / float64(resp.Metrics.Budget.Requested) * 100)
					}
					fmt.Printf("Context capsule: %d files · %d tokens (%d%% over %d token budget — all structurally related files included)\n",
						resp.FilesIncluded, resp.Metrics.Budget.Used, overagePct, resp.Metrics.Budget.Requested)
				} else {
					saved := int((1 - resp.CompressionRatio) * 100)
					fmt.Printf("Context capsule: %d files · %d/%d tokens (%d%% saved)\n",
						resp.FilesIncluded, resp.Metrics.Budget.Used, resp.Metrics.Budget.Requested, saved)
				}

				// Print per-file inclusion summary so agents can verify capsule contents.
				for _, f := range resp.Files {
					fmt.Printf("  %-8s %-6d tok  %s\n", f.Role, f.TokenEstimate, f.RelPath)
				}

				// Print rejected files (read errors only — no budget rejections now).
				for _, r := range resp.Capsule.Rejections {
					fmt.Printf("  error                 %s — %s\n", r.Candidate.File.RelPath, r.Reason)
				}
			}, func() {
				printCallLog("context", resp.BaseFeatureResponse, resp.FilesIncluded, resp.FilesConsidered, resp.CompressionRatio)
			})
		},
	}

	cmd.Flags().StringVar(&files, "files", "", "comma-separated seed file paths")
	cmd.Flags().StringVar(&from, "from", "", "git ref: use changed files as seeds")
	cmd.Flags().IntVar(&budget, "budget", 8000, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
	return cmd
}

func newFailureContextCmd(repoPath string) *cobra.Command {
	var logPath string
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "failure-context",
		Short: "Extract useful context from a failure log",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if logPath == "" {
				return fmt.Errorf("--log is required")
			}

			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			req := cfeatures.FailureContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				LogPath: logPath,
			}

			stopFeature := logProgress(fmt.Sprintf("extracting failure context from %s...", logPath))
			resp, err := client.FailureContext(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.FailureContextResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Failure context: %d suspected files · %d tokens\n", len(resp.SuspectedFiles), resp.Metrics.Budget.Used)
			}, func() {
				printCallLog("failure-context", resp.BaseFeatureResponse, 0, 0, 0)
			})
		},
	}

	cmd.Flags().StringVar(&logPath, "log", "", "path to a file containing the failure output [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
	return cmd
}

func newVerifyPlanCmd(repoPath string) *cobra.Command {
	var from string
	var files string
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "verify-plan",
		Short: "Generate a verification plan for a set of changes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" && files == "" {
				return fmt.Errorf("--from or --files is required")
			}

			stopCoord := logProgress("connecting to coordinator...")
			client, err := readyClient(repoPath)
			stopCoord()
			if err != nil {
				return err
			}

			req := cfeatures.VerifyPlanRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				GitRef: from,
			}
			if files != "" {
				req.FilePaths = splitComma(files)
			}

			stopFeature := logProgress("generating verification plan...")
			resp, err := client.VerifyPlan(cmd.Context(), req)
			stopFeature()
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.VerifyPlanResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Verification plan: %d commands · %d tokens\n", len(resp.Commands), resp.Metrics.Budget.Used)
			}, func() {
				printCallLog("verify-plan", resp.BaseFeatureResponse, 0, 0, 0)
			})
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "git ref: plan verification for changes since this ref")
	cmd.Flags().StringVar(&files, "files", "", "comma-separated list of changed file paths")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// metrics (reads .suitcode/calls.jsonl directly — no coordinator needed)
// ──────────────────────────────────────────────────────────────────────────────

func newMetricsCmd(repoPath string) *cobra.Command {
	metricsCmd := &cobra.Command{
		Use:   "metrics",
		Short: "Show or export per-call metrics from .suitcode/calls.jsonl",
	}
	metricsCmd.AddCommand(newMetricsSummaryCmd(repoPath))
	metricsCmd.AddCommand(newMetricsShowCmd(repoPath))
	metricsCmd.AddCommand(newMetricsExportCmd(repoPath))
	return metricsCmd
}

func newMetricsSummaryCmd(repoPath string) *cobra.Command {
	var last int

	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Print a condensed, copy-pasteable session summary (errors, warnings, avg latency, token budget)",
		Long: `Aggregates the call log by feature and prints a compact summary block.

Designed for transferring session analytics across air-gapped machines: run
this command, copy the ~15-line output, and paste it into a GitHub issue or
share it with the SuitCode dev team.

Fields:
  calls   — total feature invocations
  err     — calls that failed to produce a response (recorded for visibility)
  warn    — calls with Limitation notices (degraded quality: heuristic fallbacks,
             unresolved imports, etc.)
  avg_ms  — mean wall-clock latency in milliseconds
  avg_tok — mean token budget used (only for features that consume a budget)
  ratio   — mean compression ratio (files-in-context vs. files-in-repo)`,
		RunE: func(_ *cobra.Command, _ []string) error {
			clog, err := calllog.New(repoPath)
			if err != nil {
				return fmt.Errorf("metrics summary: %w", err)
			}
			return clog.PrintAggregateSummary(os.Stdout, last)
		},
	}

	cmd.Flags().IntVar(&last, "last", 0, "limit to the most recent N records (0 = all)")
	return cmd
}

func newMetricsShowCmd(repoPath string) *cobra.Command {
	var last int

	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print a tabular summary of recent feature calls",
		RunE: func(_ *cobra.Command, _ []string) error {
			clog, err := calllog.New(repoPath)
			if err != nil {
				return fmt.Errorf("metrics show: %w", err)
			}

			if err := clog.PrintSummary(os.Stdout, last); err != nil {
				return fmt.Errorf("metrics show: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&last, "last", 50, "number of most recent records to show (0 = all)")
	return cmd
}

func newMetricsExportCmd(repoPath string) *cobra.Command {
	var outputPath string

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Package the call log into a shareable zip (no code content)",
		RunE: func(_ *cobra.Command, _ []string) error {
			clog, err := calllog.New(repoPath)
			if err != nil {
				return fmt.Errorf("metrics export: %w", err)
			}

			if outputPath == "" {
				outputPath = filepath.Join(repoPath, config.SuitCodeDir, "metrics.zip")
			}

			if err := clog.Export(outputPath); err != nil {
				return fmt.Errorf("metrics export: %w", err)
			}

			fmt.Printf("Metrics exported → %s\n", outputPath)
			fmt.Println("(contains relative paths and numeric metrics only — no code content)")
			return nil
		},
	}

	cmd.Flags().StringVar(&outputPath, "output", "", "output zip path (default: .suitcode/metrics.zip)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// Response printing helpers
// ──────────────────────────────────────────────────────────────────────────────

// printFeatureResult is a generic helper that handles JSON pass-through and
// brief-summary rendering for any typed feature response. T is inferred from
// the brief function's parameter. The brief callback is responsible for calling
// printProgress and emitting its own one-line summary to stdout.
//
// logCallFn is called after the response is handled (in both json and brief
// modes) when the --log-calls flag is set. Pass nil to skip per-call logging.
func printFeatureResult[T any](resp *T, format string, brief func(*T), logCallFn func()) error {
	if format == "json" {
		if logCalls && logCallFn != nil {
			logCallFn()
		}
		return writeJSON(resp)
	}
	brief(resp) // brief already calls printProgress which logs progress
	if logCalls && logCallFn != nil {
		logCallFn()
	}
	return nil
}

// knownFormatNames is the set of strings that are valid --format values.
// Used to detect the common mistake of writing --output json instead of --format json.
var knownFormatNames = map[string]bool{"json": true, "markdown": true, "md": true}

// writeJSON pretty-prints any value as indented JSON. When --output is set the
// output goes to that file (created/truncated); otherwise it goes to stdout.
// Using --output sidesteps PowerShell's "pipe being closed" error when piping
// into ConvertFrom-Json: write to a file and read it back instead.
func writeJSON(v any) error {
	out := os.Stdout
	if outputFile != "" {
		// Warn when --output looks like a format name — a common mistake is
		// writing "--output json" when "--format json" was intended.
		if knownFormatNames[strings.ToLower(outputFile)] {
			fmt.Fprintf(os.Stderr,
				"warn: --output %q looks like a format name, not a file path.\n"+
					"  Output is being written to a file literally named %q.\n"+
					"  If you meant formatted output on stdout, use: --format %s\n"+
					"  If you meant formatted output to a file, use: --format %s --output <filename>\n",
				outputFile, outputFile, outputFile, outputFile)
		}

		f, err := os.Create(outputFile)
		if err != nil {
			return fmt.Errorf("--output: cannot create %q: %w", outputFile, err)
		}
		defer f.Close()
		out = f
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// printProgress logs timing, budget, and limitation info to stderr so agents
// can see feature call progress without it polluting stdout.
func printProgress(base cfeatures.BaseFeatureResponse) {
	m := base.Metrics
	hash := m.DeterministicHash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	logf("done in %dms · budget %d/%d · hash %s",
		m.Timing.DurationMs, m.Budget.Used, m.Budget.Requested, hash)

	if base.IsPartial {
		logf("partial result — response may be incomplete")
	}
	for _, lim := range base.Limitations {
		logf("limitation/%s: %s", lim.Kind, lim.Message)
	}
}

// printCallLog writes a compact one-liner to stderr for the --log-calls flag.
// Format: [call] <feature> <ms>ms tok=<used>/<budget> [files=<in>/<total>] [ratio=<x>×] [warn=<n>]
// Called only when logCalls == true.
func printCallLog(feature string, base cfeatures.BaseFeatureResponse, filesIn, filesTotal int, compressionRatio float64) {
	m := base.Metrics
	line := fmt.Sprintf("[call] %-16s %4dms  tok=%d/%d",
		feature, m.Timing.DurationMs, m.Budget.Used, m.Budget.Requested)

	if filesTotal > 0 {
		line += fmt.Sprintf("  files=%d/%d", filesIn, filesTotal)
	}
	if filesTotal > 0 && compressionRatio > 0 {
		line += fmt.Sprintf("  ratio=%.1f×", 1.0/compressionRatio)
	}
	if n := len(base.Limitations); n > 0 {
		line += fmt.Sprintf("  warn=%d", n)
	}
	logf("%s", line)
}

// ──────────────────────────────────────────────────────────────────────────────
// General helpers
// ──────────────────────────────────────────────────────────────────────────────

// logf writes a timestamped message to stderr with the [sc client] prefix.
func logf(format string, args ...any) {
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[sc client] %s %s\n", ts, msg)
}

// logProgress prints banner immediately to stderr, then spawns a goroutine that
// logs "still waiting... (Xs)" every 5 seconds. Call the returned stop function
// when the operation completes. stop is idempotent and safe to call once.
//
// This gives agents a continuous liveness signal while waiting for slow
// operations (coordinator startup, investigator warmup, large graph traversals).
func logProgress(banner string) (stop func()) {
	done := make(chan struct{})
	var once sync.Once

	// Print the initial banner immediately so the agent sees what we're doing.
	logf("%s", banner)

	go func() {
		const interval = 5 * time.Second
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		start := time.Now()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				elapsed := int(time.Since(start).Seconds())
				logf("still waiting... (%ds)", elapsed)
			}
		}
	}()

	return func() {
		once.Do(func() { close(done) })
	}
}

// findGitRoot walks up from dir looking for a directory that contains a .git
// entry (file or directory). Returns the absolute path of the git root, or ""
// if no .git is found before the filesystem root. Used to warn when the user
// is running SuitCode from a sub-directory of a larger git repository.
func findGitRoot(dir string) string {
	for {
		// Check for .git — could be a directory or a worktree file.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding .git.
			return ""
		}
		dir = parent
	}
}

func splitComma(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
