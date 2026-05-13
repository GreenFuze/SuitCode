package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

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
		Short: "SuitCode investigator — local repository intelligence",
		Long: `SuitCode investigator — local repository intelligence

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
	)

	return root
}

// ──────────────────────────────────────────────────────────────────────────────
// status
// ──────────────────────────────────────────────────────────────────────────────

func newStatusCmd(repoPath string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show investigator readiness status for the repository",
		Example: `  investigator . status
  investigator /path/to/repo status`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: warming investigator...\n")
			if err := inv.Warm(cmd.Context()); err != nil {
				fmt.Fprintf(os.Stderr, "SuitCode [warn]: warm failed: %v\n", err)
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
		Example: `  investigator . repo-overview
  investigator . repo-overview --budget 3000 --format markdown
  investigator . repo-overview --format json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: computing repository overview...\n")

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

			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)
			return renderResponse(format, resp, func() error {
				return output.WriteRepoOverview(os.Stdout, resp)
			})
		},
	}

	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget (0 = default)")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown or json")
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
		Example: `  investigator . explain-file --path internal/foo/bar.go
  investigator . explain-file --path internal/foo/bar.go --budget 4000 --format json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return fmt.Errorf("--path is required")
			}

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: explaining file %s...\n", path)

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

			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)
			return renderResponse(format, resp, func() error {
				return output.WriteExplainFile(os.Stdout, resp)
			})
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "file path to explain (relative to repo root or absolute) [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown or json")
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
		Example: `  investigator . related --path internal/foo/bar.go
  investigator . related --path internal/foo/bar.go --budget 4000`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return fmt.Errorf("--path is required")
			}

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: finding files related to %s...\n", path)

			resp, err := inv.Related(cmd.Context(), cfeatures.RelatedRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				FilePath: path,
			})
			if err != nil {
				return fmt.Errorf("related: %w", err)
			}

			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)
			return renderResponse(format, resp, func() error {
				return output.WriteRelated(os.Stdout, resp)
			})
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "source file (relative to repo root or absolute) [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown or json")
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
		Example: `  investigator . tests --path internal/foo/bar.go
  investigator . tests --from main`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: finding relevant tests...\n")

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

			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)
			return renderResponse(format, resp, func() error {
				return output.WriteTests(os.Stdout, resp)
			})
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "source file to find tests for")
	cmd.Flags().StringVar(&from, "from", "", "git ref: find tests for files changed since this ref")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown or json")
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
		Example: `  investigator . impact --from main
  investigator . impact --files internal/foo.go,internal/bar.go`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" && files == "" {
				return fmt.Errorf("--from or --files is required")
			}

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: computing impact analysis...\n")

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

			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)
			return renderResponse(format, resp, func() error {
				return output.WriteImpact(os.Stdout, resp)
			})
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "git ref: analyse changes since this ref")
	cmd.Flags().StringVar(&files, "files", "", "comma-separated list of changed file paths")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown or json")
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
		Example: `  investigator . context --files internal/foo.go,internal/bar.go --budget 8000
  investigator . context --from main --budget 6000 --format json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if files == "" && from == "" {
				return fmt.Errorf("--files or --from is required")
			}

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: compiling context capsule...\n")

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

			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)
			return renderResponse(format, resp, func() error {
				return output.WriteContext(os.Stdout, resp)
			})
		},
	}

	cmd.Flags().StringVar(&files, "files", "", "comma-separated seed file paths (relative to repo root or absolute)")
	cmd.Flags().StringVar(&from, "from", "", "git ref: use changed files as seeds")
	cmd.Flags().IntVar(&budget, "budget", 8000, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown or json")
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
		Example: `  investigator . failure-context --log test-failure.txt
  investigator . failure-context --log build-error.txt --budget 6000`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if logPath == "" {
				return fmt.Errorf("--log is required")
			}

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: extracting failure context from %s...\n", logPath)

			resp, err := inv.FailureContext(cmd.Context(), cfeatures.FailureContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: repoPath, Budget: budget, Format: cfeatures.OutputFormat(format),
				},
				LogPath: logPath,
			})
			if err != nil {
				return fmt.Errorf("failure-context: %w", err)
			}

			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)
			return renderResponse(format, resp, func() error {
				return output.WriteFailureContext(os.Stdout, resp)
			})
		},
	}

	cmd.Flags().StringVar(&logPath, "log", "", "path to a file containing the failure output [required]")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown or json")
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
		Example: `  investigator . verify-plan --from main
  investigator . verify-plan --files internal/foo.go --budget 4000`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" && files == "" {
				return fmt.Errorf("--from or --files is required")
			}

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: generating verification plan...\n")

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

			printProgress(resp.IsPartial, resp.Limitations, resp.Metrics)
			return renderResponse(format, resp, func() error {
				return output.WriteVerifyPlan(os.Stdout, resp)
			})
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "git ref: plan verification for changes since this ref")
	cmd.Flags().StringVar(&files, "files", "", "comma-separated list of changed file paths")
	cmd.Flags().IntVar(&budget, "budget", 0, "maximum estimated token budget")
	cmd.Flags().StringVar(&format, "format", "markdown", "output format: markdown or json")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// serve
// ──────────────────────────────────────────────────────────────────────────────

func newServeCmd(repoPath string) *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP API server for this repository",
		Example: `  investigator . serve
  investigator . serve --port 7878`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "SuitCode: warming investigator...\n")
			if err := inv.Warm(cmd.Context()); err != nil {
				fmt.Fprintf(os.Stderr, "SuitCode [warn]: warm failed: %v\n", err)
			}

			fmt.Fprintf(os.Stderr, "SuitCode: starting HTTP server on :%d\n", port)
			srv := NewServer(inv, port)
			return srv.ListenAndServe()
		},
	}

	cmd.Flags().IntVar(&port, "port", 7878, "TCP port to listen on")
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
		Example: `  investigator . eval run --suite smoke
  investigator . eval run --suite context-reduction`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if suite == "" {
				return fmt.Errorf("--suite is required (available: smoke, context-reduction, go-provider, go-provider-symbols)")
			}

			inv, err := buildInvestigator(cmd.Context(), repoPath)
			if err != nil {
				return err
			}
			defer inv.Close()

			fmt.Fprintf(os.Stderr, "SuitCode: warming investigator for eval...\n")
			if err := inv.Warm(cmd.Context()); err != nil {
				fmt.Fprintf(os.Stderr, "SuitCode [warn]: warm failed: %v\n", err)
			}

			fmt.Fprintf(os.Stderr, "SuitCode: running eval suite %q...\n", suite)

			runner := eval.NewRunner(inv, repoPath)
			run, err := runner.Run(cmd.Context(), eval.SuiteID(suite))
			if err != nil {
				return fmt.Errorf("eval run: %w", err)
			}

			// Print report to stdout.
			printEvalReport(run)

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
// Shared helpers
// ──────────────────────────────────────────────────────────────────────────────

// buildInvestigator creates and attaches a ProjectInvestigator. It does NOT
// warm it — warming is the responsibility of commands that need warm state.
func buildInvestigator(ctx context.Context, repoPath string) (*ProjectInvestigator, error) {
	fmt.Fprintf(os.Stderr, "SuitCode: attaching to %s...\n", repoPath)
	inv, err := NewProjectInvestigator(ctx, repoPath)
	if err != nil {
		return nil, fmt.Errorf("initialising investigator: %w", err)
	}
	return inv, nil
}

// printProgress writes a summary of limitations and metrics to stderr.
func printProgress(isPartial bool, limitations []provider.Limitation, m cfeatures.FeatureMetrics) {
	if isPartial {
		fmt.Fprintf(os.Stderr, "SuitCode [partial result]: response may be incomplete\n")
	}
	for _, lim := range limitations {
		fmt.Fprintf(os.Stderr, "SuitCode [limitation/%s]: %s\n", lim.Kind, lim.Message)
	}
	fmt.Fprintf(os.Stderr, "SuitCode: done in %dms · budget %d/%d · hash %s\n",
		m.Timing.DurationMs, m.Budget.Used, m.Budget.Requested, shortHashStr(m.DeterministicHash))
}

func shortHashStr(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// renderResponse writes the response to stdout in the requested format.
// jsonFn is called for JSON output; markdownFn for markdown.
func renderResponse(format string, v any, markdownFn func() error) error {
	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(v)
	}
	return markdownFn()
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

// printEvalReport writes a human-readable eval report to stdout.
func printEvalReport(run *eval.EvalRun) {
	verdict := "PASSED"
	if run.Summary.Failed > 0 {
		verdict = "FAILED"
	}

	fmt.Printf("# Eval Report: %s\n\n", run.Suite)
	fmt.Printf("**Verdict:** %s  \n", verdict)
	fmt.Printf("**Score:** %d/%d scenarios passed (%.0f%%)\n\n",
		run.Summary.Passed, run.Summary.Total, run.Summary.Score*100)
	fmt.Printf("| Scenario | Result | Details |\n")
	fmt.Printf("|----------|--------|--------|\n")

	for _, r := range run.Results {
		icon := "✓"
		if !r.Passed {
			icon = "✗"
		}
		details := ""
		if len(r.Metrics) > 0 {
			var parts []string
			for _, m := range r.Metrics {
				parts = append(parts, m.Detail)
			}
			details = strings.Join(parts, "; ")
		}
		if len(r.Notes) > 0 {
			details += " " + strings.Join(r.Notes, "; ")
		}
		fmt.Printf("| %s %s | %s | %s |\n", icon, r.ScenarioName,
			map[bool]string{true: "passed", false: "failed"}[r.Passed],
			details)
	}

	fmt.Printf("\n_Run ID: %s · %s → %s_\n",
		run.ID,
		run.StartedAt.Format("15:04:05"),
		run.FinishedAt.Format("15:04:05"))
}
