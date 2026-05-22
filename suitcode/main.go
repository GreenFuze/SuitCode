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
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GreenFuze/SuitCode/calllog"
	"github.com/GreenFuze/SuitCode/core/config"
	cfeatures "github.com/GreenFuze/SuitCode/core/features"
)

const defaultCoordinatorURL = "http://127.0.0.1:7878"

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
    show             Print a table of recent calls (--last N, default 50)
    export           Package the call log as a shareable zip

OUTPUT:
  By default every command prints a one-line summary to stdout and emits
  timing/budget/hash diagnostics to stderr.

  Pass --format json to receive the full structured response on stdout.
  Agents should use --format json for programmatic use — the JSON contains
  all evidence, provenance, metrics, and limitation notices.

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

	root.AddCommand(
		newStatusCmd(repoPath),
		newWarmupCmd(repoPath),
		newRepoOverviewCmd(repoPath),
		newExplainFileCmd(repoPath),
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
			if err := ensureCoordinator(defaultCoordinatorURL); err != nil {
				return err
			}
			client := NewCoordinatorClient(defaultCoordinatorURL, repoPath)

			logf("checking coordinator health...")
			health, err := client.GetHealth(cmd.Context())
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
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting warmup for %s (may take up to 90 seconds)...", repoPath)
			fmt.Printf("Warming up investigator for %s...\n", repoPath)
			fmt.Printf("This loads the Go import graph and starts gopls. Please wait.\n\n")

			start := time.Now()
			if err := client.Warmup(cmd.Context()); err != nil {
				return fmt.Errorf("warmup: %w", err)
			}

			elapsed := time.Since(start).Round(time.Millisecond)
			logf("warmup complete in %s", elapsed)
			fmt.Printf("✓ Investigator warm (took %s)\n", elapsed)
			fmt.Printf("  Project: %s\n", repoPath)
			fmt.Printf("  Import graph and gopls are ready for fast queries.\n")
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
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting repo-overview...")
			req := cfeatures.RepoOverviewRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
			}

			resp, err := client.RepoOverview(cmd.Context(), req)
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.RepoOverviewResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Repository overview: %d files · %d languages · %d build systems\n",
					resp.TotalFiles, len(resp.Languages), len(resp.BuildSystems))
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
		Use:   "explain-file",
		Short: "Explain a file's role, imports, tests, and relationships",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return fmt.Errorf("--path is required")
			}
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting explain-file for %s...", path)
			req := cfeatures.ExplainFileRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
			}

			resp, err := client.ExplainFile(cmd.Context(), req)
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.ExplainFileResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("File explanation: %s · %d tokens\n", filepath.Base(path), resp.Metrics.Budget.Used)
			})
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "file path to explain [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: json (default: brief summary)")
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
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting related for %s...", path)
			req := cfeatures.RelatedRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
			}

			resp, err := client.Related(cmd.Context(), req)
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.RelatedResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Related files: %d found · %d tokens\n", len(resp.RelatedFiles), resp.Metrics.Budget.Used)
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
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting tests...")
			req := cfeatures.TestsRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
				DiffRef:  from,
			}

			resp, err := client.Tests(cmd.Context(), req)
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.TestsResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Relevant tests: %d found · %d tokens\n", len(resp.RelevantTests), resp.Metrics.Budget.Used)
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
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting impact analysis...")
			req := cfeatures.ImpactRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				GitRef: from,
			}
			if files != "" {
				req.FilePaths = splitComma(files)
			}

			resp, err := client.Impact(cmd.Context(), req)
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.ImpactResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Impact: %d downstream files · %d tokens\n", len(resp.ImpactedFiles), resp.Metrics.Budget.Used)
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
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting context capsule...")
			req := cfeatures.ContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				DiffRef: from,
			}
			if files != "" {
				req.Files = splitComma(files)
			}

			resp, err := client.Context(cmd.Context(), req)
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.ContextResponse) {
				printProgress(resp.BaseFeatureResponse)
				saved := int((1 - resp.CompressionRatio) * 100)
				fmt.Printf("Context capsule: %d files · %d/%d tokens (%d%% saved)\n",
					resp.FilesIncluded, resp.Metrics.Budget.Used, resp.Metrics.Budget.Requested, saved)
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
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting failure-context for %s...", logPath)
			req := cfeatures.FailureContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				LogPath: logPath,
			}

			resp, err := client.FailureContext(cmd.Context(), req)
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.FailureContextResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Failure context: %d suspected files · %d tokens\n", len(resp.SuspectedFiles), resp.Metrics.Budget.Used)
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
			client, err := readyClient(repoPath)
			if err != nil {
				return err
			}

			logf("requesting verify-plan...")
			req := cfeatures.VerifyPlanRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				GitRef: from,
			}
			if files != "" {
				req.FilePaths = splitComma(files)
			}

			resp, err := client.VerifyPlan(cmd.Context(), req)
			if err != nil {
				return err
			}

			return printFeatureResult(resp, format, func(resp *cfeatures.VerifyPlanResponse) {
				printProgress(resp.BaseFeatureResponse)
				fmt.Printf("Verification plan: %d commands · %d tokens\n", len(resp.Commands), resp.Metrics.Budget.Used)
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
	metricsCmd.AddCommand(newMetricsShowCmd(repoPath))
	metricsCmd.AddCommand(newMetricsExportCmd(repoPath))
	return metricsCmd
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

			records, err := clog.LoadAll()
			if err != nil {
				return fmt.Errorf("metrics show: %w", err)
			}
			if len(records) == 0 {
				fmt.Println("No call records found in", clog.Path())
				return nil
			}

			if last > 0 && len(records) > last {
				records = records[len(records)-last:]
			}

			fmt.Printf("%-20s  %-18s  %-8s  %-12s  %-8s  %-10s\n",
				"Feature", "Time", "Files", "Budget", "Latency", "Compression")
			fmt.Println(strings.Repeat("-", 85))

			for _, r := range records {
				ts := r.TS
				if t, err := time.Parse(time.RFC3339, r.TS); err == nil {
					ts = t.Local().Format("2006-01-02 15:04")
				}
				filesCol := "-"
				if r.CandidatesTotal > 0 {
					filesCol = fmt.Sprintf("%d/%d", r.FilesIncluded, r.CandidatesTotal)
				}
				budgetCol := fmt.Sprintf("%d", r.BudgetUsed)
				if r.BudgetRequested > 0 {
					budgetCol = fmt.Sprintf("%d/%d", r.BudgetUsed, r.BudgetRequested)
				}
				compressionCol := "-"
				if r.CandidatesTotal > 0 {
					saved := int((1 - r.CompressionRatio) * 100)
					compressionCol = fmt.Sprintf("%d%%", saved)
				}

				fmt.Printf("%-20s  %-18s  %-8s  %-12s  %-8s  %-10s\n",
					truncate(r.Feature, 20), ts, filesCol, budgetCol,
					fmt.Sprintf("%dms", r.LatencyMs), compressionCol)
			}

			fmt.Printf("\n%d records  ·  %s\n", len(records), clog.Path())
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

			src := clog.Path()
			if _, err := os.Stat(src); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("metrics export: no call log found at %s", src)
				}
				return fmt.Errorf("metrics export: %w", err)
			}

			if outputPath == "" {
				outputPath = filepath.Join(repoPath, config.SuitCodeDir, "metrics.zip")
			}

			if err := zipFile(src, outputPath); err != nil {
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
func printFeatureResult[T any](resp *T, format string, brief func(*T)) error {
	if format == "json" {
		return writeJSON(resp)
	}
	brief(resp)
	return nil
}

// writeJSON pretty-prints any value as indented JSON to stdout.
func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
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

// ──────────────────────────────────────────────────────────────────────────────
// General helpers
// ──────────────────────────────────────────────────────────────────────────────

// logf writes a timestamped message to stderr with the [sc client] prefix.
func logf(format string, args ...any) {
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[sc client] %s %s\n", ts, msg)
}

func splitComma(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func zipFile(src, dst string) error {
	zf, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create zip %q: %w", dst, err)
	}
	defer zf.Close()

	w := zip.NewWriter(zf)
	defer w.Close()

	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %q: %w", src, err)
	}
	defer srcFile.Close()

	entry, err := w.Create(filepath.Base(src))
	if err != nil {
		return fmt.Errorf("zip entry: %w", err)
	}
	_, err = io.Copy(entry, srcFile)
	return err
}
