package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GreenFuze/SuitCode/calllog"
	"github.com/GreenFuze/SuitCode/core/config"
	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
	"github.com/GreenFuze/SuitCode/investigator/eval"
	"github.com/GreenFuze/SuitCode/investigator/output"
)

// NewRootCmd builds the cobra command tree. repoPath has already been
// validated as an existing directory by main().
func NewRootCmd(repoPath string) *cobra.Command {
	root := &cobra.Command{
		Use:   "investigator",
		Short: "SuitCode Investigator — per-project repository intelligence daemon",
		Long: `SuitCode Investigator — per-project repository intelligence daemon

Analyse a repository and produce compact, evidence-backed answers that reduce
the context a developer or coding agent needs to load manually.

The first argument to every invocation must be the repository path:

  investigator <repo-path> <command> [flags]

repo-path may be relative (e.g. ".") or absolute.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newStatusCmd(repoPath),
		newRepoOverviewCmd(repoPath),
		newExplainFileCmd(repoPath),
		newRelatedCmd(repoPath),
		newTestsCmd(repoPath),
		newImpactCmd(repoPath),
		newContextCmd(repoPath),
		newFailureContextCmd(repoPath),
		newVerifyPlanCmd(repoPath),
		newServeCmd(repoPath),
		newEvalCmd(repoPath),
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
		Short: "Show readiness status for the repository",
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("warming investigator...")
			if err := inv.Warm(cmd.Context()); err != nil {
				logf("warn: warm failed: %v", err)
			}

			st := inv.Status()
			fmt.Printf("Repository:  %s\n", st.RepoPath)
			fmt.Printf("Readiness:   %s\n", st.ReadinessDesc)
			if st.LastWarmedAt != nil {
				fmt.Printf("Last warmed: %s (%dms)\n", st.LastWarmedAt.Format("15:04:05"), st.WarmDurationMs)
			} else {
				fmt.Printf("Last warmed: never\n")
			}
			fmt.Println()
			fmt.Println("Providers:")
			for _, p := range st.Providers {
				icon := "✓"
				if !p.Ready {
					icon = "✗"
				}
				fmt.Printf("  %s  %-12s  %s\n", icon, p.ProviderID, p.Summary)
			}
			return nil
		},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// repo-overview
// ──────────────────────────────────────────────────────────────────────────────

func newRepoOverviewCmd(repoPath string) *cobra.Command {
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "repo-overview",
		Short: "Repository structure and technology overview",
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("computing repository overview...")

			resp, err := inv.RepoOverview(cmd.Context(), cfeatures.RepoOverviewRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath,
					Budget:   budget,
					Format:   cfeatures.OutputFormat(format),
				},
			})
			if err != nil {
				return fmt.Errorf("repo-overview: %w", err)
			}

			artifactPath, aerr := saveResult(repoPath, "repo-overview", resp)
			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)

			if format == "json" {
				return renderJSON(resp)
			}
			if format == "markdown" {
				return output.WriteRepoOverview(os.Stdout, resp)
			}

			// Brief summary (default).
			fmt.Printf("Repository overview: %d files · %d languages · %d build systems\n",
				resp.TotalFiles, len(resp.Languages), len(resp.BuildSystems))
			printArtifactPath(artifactPath, aerr)
			return nil
		},
	}

	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget (0 = default)")
	cmd.Flags().StringVar(&format, "format", "", "output format: markdown or json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// explain-file
// ──────────────────────────────────────────────────────────────────────────────

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

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("explaining file %s...", path)

			resp, err := inv.ExplainFile(cmd.Context(), cfeatures.ExplainFileRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath,
					Budget:   budget,
					Format:   cfeatures.OutputFormat(format),
				},
				FilePath: path,
			})
			if err != nil {
				return fmt.Errorf("explain-file: %w", err)
			}

			artifactPath, aerr := saveResult(repoPath, "explain-file", resp)
			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)

			if format == "json" {
				return renderJSON(resp)
			}
			if format == "markdown" {
				return output.WriteExplainFile(os.Stdout, resp)
			}

			fmt.Printf("File explanation: %s · %d tokens\n",
				filepath.Base(path), resp.Metrics.Budget.Used)
			printArtifactPath(artifactPath, aerr)
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "file path to explain (relative to repo root or absolute) [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: markdown or json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// related
// ──────────────────────────────────────────────────────────────────────────────

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

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("finding files related to %s...", path)

			resp, err := inv.Related(cmd.Context(), cfeatures.RelatedRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
			})
			if err != nil {
				return fmt.Errorf("related: %w", err)
			}

			artifactPath, aerr := saveResult(repoPath, "related", resp)
			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)

			if format == "json" {
				return renderJSON(resp)
			}
			if format == "markdown" {
				return output.WriteRelated(os.Stdout, resp)
			}

			fmt.Printf("Related files: %d found · %d tokens\n",
				len(resp.RelatedFiles), resp.Metrics.Budget.Used)
			printArtifactPath(artifactPath, aerr)
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "source file (relative to repo root or absolute) [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: markdown or json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// tests
// ──────────────────────────────────────────────────────────────────────────────

func newTestsCmd(repoPath string) *cobra.Command {
	var path string
	var from string
	var budget int
	var format string

	cmd := &cobra.Command{
		Use:   "tests",
		Short: "Find tests relevant to a source file or change",
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("finding relevant tests...")

			resp, err := inv.Tests(cmd.Context(), cfeatures.TestsRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
				DiffRef:  from,
			})
			if err != nil {
				return fmt.Errorf("tests: %w", err)
			}

			artifactPath, aerr := saveResult(repoPath, "tests", resp)
			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)

			if format == "json" {
				return renderJSON(resp)
			}
			if format == "markdown" {
				return output.WriteTests(os.Stdout, resp)
			}

			fmt.Printf("Relevant tests: %d found · %d tokens\n",
				len(resp.RelevantTests), resp.Metrics.Budget.Used)
			printArtifactPath(artifactPath, aerr)
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "source file to find tests for")
	cmd.Flags().StringVar(&from, "from", "", "git ref: find tests for files changed since this ref")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: markdown or json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// impact
// ──────────────────────────────────────────────────────────────────────────────

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

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("computing impact analysis...")

			req := cfeatures.ImpactRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				GitRef: from,
			}
			if files != "" {
				req.FilePaths = splitComma(files)
			}

			resp, err := inv.Impact(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("impact: %w", err)
			}

			artifactPath, aerr := saveResult(repoPath, "impact", resp)
			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)

			if format == "json" {
				return renderJSON(resp)
			}
			if format == "markdown" {
				return output.WriteImpact(os.Stdout, resp)
			}

			fmt.Printf("Impact: %d downstream files · %d tokens\n",
				len(resp.ImpactedFiles), resp.Metrics.Budget.Used)
			printArtifactPath(artifactPath, aerr)
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "git ref: analyse changes since this ref")
	cmd.Flags().StringVar(&files, "files", "", "comma-separated list of changed file paths")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: markdown or json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// context
// ──────────────────────────────────────────────────────────────────────────────

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

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("compiling context capsule...")

			req := cfeatures.ContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				DiffRef: from,
			}
			if files != "" {
				req.Files = splitComma(files)
			}

			resp, err := inv.Context(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("context: %w", err)
			}

			artifactPath, aerr := saveResult(repoPath, "context", resp)
			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)

			if format == "json" {
				return renderJSON(resp)
			}
			if format == "markdown" {
				return output.WriteContext(os.Stdout, resp)
			}

			saved := int((1 - resp.CompressionRatio) * 100)
			fmt.Printf("Context capsule: %d files · %d/%d tokens (%d%% saved)\n",
				resp.FilesIncluded,
				resp.Metrics.Budget.Used,
				resp.Metrics.Budget.Requested,
				saved)
			printArtifactPath(artifactPath, aerr)
			return nil
		},
	}

	cmd.Flags().StringVar(&files, "files", "", "comma-separated seed file paths (relative to repo root or absolute)")
	cmd.Flags().StringVar(&from, "from", "", "git ref: use changed files as seeds")
	cmd.Flags().IntVar(&budget, "budget", 8000, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: markdown or json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// failure-context
// ──────────────────────────────────────────────────────────────────────────────

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

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("extracting failure context from %s...", logPath)

			resp, err := inv.FailureContext(cmd.Context(), cfeatures.FailureContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				LogPath: logPath,
			})
			if err != nil {
				return fmt.Errorf("failure-context: %w", err)
			}

			artifactPath, aerr := saveResult(repoPath, "failure-context", resp)
			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)

			if format == "json" {
				return renderJSON(resp)
			}
			if format == "markdown" {
				return output.WriteFailureContext(os.Stdout, resp)
			}

			fmt.Printf("Failure context: %d suspected files · %d tokens\n",
				len(resp.SuspectedFiles), resp.Metrics.Budget.Used)
			printArtifactPath(artifactPath, aerr)
			return nil
		},
	}

	cmd.Flags().StringVar(&logPath, "log", "", "path to a file containing the failure output [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: markdown or json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// verify-plan
// ──────────────────────────────────────────────────────────────────────────────

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

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("generating verification plan...")

			req := cfeatures.VerifyPlanRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				GitRef: from,
			}
			if files != "" {
				req.FilePaths = splitComma(files)
			}

			resp, err := inv.VerifyPlan(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("verify-plan: %w", err)
			}

			artifactPath, aerr := saveResult(repoPath, "verify-plan", resp)
			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)

			if format == "json" {
				return renderJSON(resp)
			}
			if format == "markdown" {
				return output.WriteVerifyPlan(os.Stdout, resp)
			}

			fmt.Printf("Verification plan: %d commands · %d tokens\n",
				len(resp.Commands), resp.Metrics.Budget.Used)
			printArtifactPath(artifactPath, aerr)
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "git ref: plan verification for changes since this ref")
	cmd.Flags().StringVar(&files, "files", "", "comma-separated list of changed file paths")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "", "output format: markdown or json (default: brief summary)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// serve
// ──────────────────────────────────────────────────────────────────────────────

func newServeCmd(repoPath string) *cobra.Command {
	var port int
	var coordinatorURL string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP API server for this repository",
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}

			logf("warming investigator on startup...")
			if err := inv.Warm(cmd.Context()); err != nil {
				logf("warn: warm failed: %v", err)
			}

			logf("starting HTTP server on :%d for %s", port, repoPath)
			srv := NewServer(inv, port, coordinatorURL)
			return srv.ListenAndServe()
		},
	}

	cmd.Flags().IntVar(&port, "port", 7878, "TCP port to listen on")
	cmd.Flags().StringVar(&coordinatorURL, "coordinator-url", "",
		"base URL of the coordinator that spawned this investigator (e.g. http://127.0.0.1:7878)")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// eval
// ──────────────────────────────────────────────────────────────────────────────

func newEvalCmd(repoPath string) *cobra.Command {
	evalCmd := &cobra.Command{
		Use:   "eval",
		Short: "Run evaluation suites and report results",
	}
	evalCmd.AddCommand(newEvalRunCmd(repoPath))
	return evalCmd
}

func newEvalRunCmd(repoPath string) *cobra.Command {
	var suite string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute an evaluation suite",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if suite == "" {
				return fmt.Errorf("--suite is required (available: smoke, context-reduction, go-provider, go-provider-symbols)")
			}

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			logf("warming investigator for eval...")
			if err := inv.Warm(cmd.Context()); err != nil {
				logf("warn: warm failed: %v", err)
			}

			logf("running eval suite %q...", suite)

			runner := eval.NewRunner(inv, repoPath)
			run, err := runner.Run(cmd.Context(), eval.SuiteID(suite))
			if err != nil {
				return fmt.Errorf("eval run: %w", err)
			}

			run.PrintReport(os.Stdout)

			if run.Summary.Failed > 0 {
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&suite, "suite", "", "suite name: smoke or context-reduction [required]")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// metrics
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
			return clog.PrintSummary(os.Stdout, last)
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
// Shared helpers
// ──────────────────────────────────────────────────────────────────────────────

// buildInvestigator creates and attaches a ProjectInvestigator.
func buildInvestigator(ctx context.Context, repoPath string) (*ProjectInvestigator, error) {
	logf("attaching to %s...", repoPath)
	inv, err := NewProjectInvestigator(ctx, repoPath)
	if err != nil {
		return nil, fmt.Errorf("initialising investigator: %w", err)
	}
	return inv, nil
}

// saveResult writes the response JSON to .suitcode/<feature>/<timestamp>.json
// and returns the relative path for display. Non-fatal — callers should display
// the error but not abort the command.
func saveResult(repoPath, feature string, v any) (string, error) {
	dir := filepath.Join(repoPath, config.SuitCodeDir, feature)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("save result: mkdir %q: %w", dir, err)
	}

	ts := time.Now().UTC().Format("20060102T150405")
	filename := ts + ".json"
	absPath := filepath.Join(dir, filename)

	f, err := os.Create(absPath)
	if err != nil {
		return "", fmt.Errorf("save result: create %q: %w", absPath, err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("save result: encode: %w", err)
	}

	// Return relative path for display (relative to cwd if possible, else relative to repo).
	relPath := filepath.Join(config.SuitCodeDir, feature, filename)
	return relPath, nil
}

// printArtifactPath prints the artifact file path to stdout (or a warning on error).
func printArtifactPath(path string, err error) {
	if err != nil {
		logf("warn: could not save result: %v", err)
	} else if path != "" {
		fmt.Printf("Saved → %s\n", path)
	}
}

// printProgress writes a summary of limitations and metrics to stderr.
func printProgress(isPartial bool, limitations []provider.Limitation, m cfeatures.FeatureMetrics) {
	if isPartial {
		logf("partial result: response may be incomplete")
	}
	for _, lim := range limitations {
		logf("limitation/%s: %s", lim.Kind, lim.Message)
	}
	logf("done in %dms · budget %d/%d · hash %s",
		m.Timing.DurationMs, m.Budget.Used, m.Budget.Requested, shortHashStr(m.DeterministicHash))
}

func shortHashStr(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// renderJSON writes v as indented JSON to stdout.
func renderJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// splitComma splits a comma-separated string into trimmed non-empty parts.
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

