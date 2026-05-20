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

// usage is shown when the repo-path argument is missing.
const usage = `SuitCode — local repository intelligence for coding agents

USAGE:
  suitcode <repo-path> <command> [flags]

ARGUMENTS:
  repo-path   Path to the repository to analyse (required).
              May be relative (../myrepo) or absolute (/home/user/myrepo).
              Unlike many CLIs, this tool does NOT default to the current
              directory — the path must be given explicitly.

COMMANDS:
  status             Show coordinator + investigator readiness status
  warmup             Pre-warm the investigator (spawns it and waits for level 3)
  repo-overview      Repository structure and technology overview
  explain-file       Explain a file's role, imports, tests, and relationships
  related            Find files related to a given file
  tests              Find tests relevant to a source file or change
  impact             Blast radius analysis for a set of changes
  context            Compile a bounded context capsule for a set of files
  failure-context    Extract useful context from a failure log
  verify-plan        Generate a verification plan for a set of changes
  metrics            Show or export per-call metrics from .suitcode/calls.jsonl

Run 'suitcode <repo-path> <command> --help' for per-command flags.

EXAMPLES:
  suitcode . status
  suitcode . warmup
  suitcode /path/to/myrepo repo-overview --budget 3000
  suitcode . context --files internal/foo.go,internal/bar.go --budget 8000
  suitcode . metrics show
`

// ──────────────────────────────────────────────────────────────────────────────
// Entry point
// ──────────────────────────────────────────────────────────────────────────────

func main() {
	// Pre-processing: strip and validate the mandatory <repo-path> argument.
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

			fmt.Printf("Coordinator: OK (projects=%v)\n", health["projects"])
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
			if err := ensureCoordinator(defaultCoordinatorURL); err != nil {
				return err
			}
			client := NewCoordinatorClient(defaultCoordinatorURL, repoPath)

			logf("requesting warmup for %s (may take up to 90 seconds)...", repoPath)
			fmt.Printf("Warming up investigator for %s...\n", repoPath)
			fmt.Printf("This loads the Go import graph and starts gopls. Please wait.\n\n")

			start := time.Now()
			if err := client.PostWarmup(cmd.Context()); err != nil {
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

			raw, err := client.Post(cmd.Context(), "repo-overview", req)
			if err != nil {
				return err
			}

			return printFeatureResult(raw, format, func(v map[string]any) {
				totalFiles, _ := v["total_files"].(float64)
				languages := countSlice(v, "languages")
				buildSystems := countSlice(v, "build_systems")
				fmt.Printf("Repository overview: %.0f files · %d languages · %d build systems\n",
					totalFiles, languages, buildSystems)
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

			raw, err := client.Post(cmd.Context(), "explain-file", req)
			if err != nil {
				return err
			}

			return printFeatureResult(raw, format, func(v map[string]any) {
				budgetUsed := budgetUsedFromMetrics(v)
				fmt.Printf("File explanation: %s · %d tokens\n", filepath.Base(path), budgetUsed)
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

			raw, err := client.Post(cmd.Context(), "related", req)
			if err != nil {
				return err
			}

			return printFeatureResult(raw, format, func(v map[string]any) {
				related := countSlice(v, "related_files")
				budgetUsed := budgetUsedFromMetrics(v)
				fmt.Printf("Related files: %d found · %d tokens\n", related, budgetUsed)
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

			raw, err := client.Post(cmd.Context(), "tests", req)
			if err != nil {
				return err
			}

			return printFeatureResult(raw, format, func(v map[string]any) {
				tests := countSlice(v, "relevant_tests")
				budgetUsed := budgetUsedFromMetrics(v)
				fmt.Printf("Relevant tests: %d found · %d tokens\n", tests, budgetUsed)
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

			raw, err := client.Post(cmd.Context(), "impact", req)
			if err != nil {
				return err
			}

			return printFeatureResult(raw, format, func(v map[string]any) {
				impacted := countSlice(v, "impacted_files")
				budgetUsed := budgetUsedFromMetrics(v)
				fmt.Printf("Impact: %d downstream files · %d tokens\n", impacted, budgetUsed)
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

			raw, err := client.Post(cmd.Context(), "context", req)
			if err != nil {
				return err
			}

			return printFeatureResult(raw, format, func(v map[string]any) {
				filesIncluded, _ := v["files_included"].(float64)
				compressionRatio, _ := v["compression_ratio"].(float64)
				saved := int((1 - compressionRatio) * 100)
				budgetUsed := budgetUsedFromMetrics(v)
				budgetRequested := budgetRequestedFromMetrics(v)
				fmt.Printf("Context capsule: %.0f files · %d/%d tokens (%d%% saved)\n",
					filesIncluded, budgetUsed, budgetRequested, saved)
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

			raw, err := client.Post(cmd.Context(), "failure-context", req)
			if err != nil {
				return err
			}

			return printFeatureResult(raw, format, func(v map[string]any) {
				suspected := countSlice(v, "suspected_files")
				budgetUsed := budgetUsedFromMetrics(v)
				fmt.Printf("Failure context: %d suspected files · %d tokens\n", suspected, budgetUsed)
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

			raw, err := client.Post(cmd.Context(), "verify-plan", req)
			if err != nil {
				return err
			}

			return printFeatureResult(raw, format, func(v map[string]any) {
				commands := countSlice(v, "commands")
				budgetUsed := budgetUsedFromMetrics(v)
				fmt.Printf("Verification plan: %d commands · %d tokens\n", commands, budgetUsed)
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

// printFeatureResult decodes a raw JSON response, emits metrics to stderr, and
// either renders as JSON or calls briefSummary for default one-line output.
func printFeatureResult(raw []byte, format string, briefSummary func(map[string]any)) error {
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Fallback: print raw bytes if not parseable JSON.
		fmt.Println(string(raw))
		return nil
	}

	// Always emit timing/budget info to stderr so agents can see progress.
	printProgress(v)

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}

	briefSummary(v)
	return nil
}

// printProgress extracts and logs timing/budget info from the metrics block.
func printProgress(v map[string]any) {
	metrics, _ := v["metrics"].(map[string]any)
	if metrics == nil {
		return
	}

	timing, _ := metrics["timing"].(map[string]any)
	budget, _ := metrics["budget"].(map[string]any)

	var durationMs float64
	if timing != nil {
		durationMs, _ = timing["duration_ms"].(float64)
	}

	var budgetUsed, budgetRequested float64
	if budget != nil {
		budgetUsed, _ = budget["used"].(float64)
		budgetRequested, _ = budget["requested"].(float64)
	}

	hash, _ := metrics["deterministic_hash"].(string)
	if len(hash) > 12 {
		hash = hash[:12]
	}

	logf("done in %.0fms · budget %.0f/%.0f · hash %s",
		durationMs, budgetUsed, budgetRequested, hash)

	if isPartial, _ := v["is_partial"].(bool); isPartial {
		logf("partial result — response may be incomplete")
	}
	if lims, ok := v["limitations"].([]any); ok {
		for _, lim := range lims {
			if limMap, ok := lim.(map[string]any); ok {
				kind, _ := limMap["kind"].(string)
				msg, _ := limMap["message"].(string)
				logf("limitation/%s: %s", kind, msg)
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// JSON extraction helpers
// ──────────────────────────────────────────────────────────────────────────────

func countSlice(v map[string]any, key string) int {
	if arr, ok := v[key].([]any); ok {
		return len(arr)
	}
	return 0
}

func budgetUsedFromMetrics(v map[string]any) int {
	if m, ok := v["metrics"].(map[string]any); ok {
		if b, ok := m["budget"].(map[string]any); ok {
			if used, ok := b["used"].(float64); ok {
				return int(used)
			}
		}
	}
	return 0
}

func budgetRequestedFromMetrics(v map[string]any) int {
	if m, ok := v["metrics"].(map[string]any); ok {
		if b, ok := m["budget"].(map[string]any); ok {
			if req, ok := b["requested"].(float64); ok {
				return int(req)
			}
		}
	}
	return 0
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
