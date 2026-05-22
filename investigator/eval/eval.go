// Package eval provides types and a runner for SuitCode evaluation suites.
// Evaluation measures token/context reduction proxies, determinism, and budget
// compliance. It is a first-class product concern, not a testing afterthought.
package eval

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// SuiteID identifies a named evaluation suite.
type SuiteID string

const (
	SuiteSmoke            SuiteID = "smoke"
	SuiteContextReduction SuiteID = "context-reduction"
	SuiteGoProvider       SuiteID = "go-provider"
)

// ScenarioKind categorises what an eval scenario tests.
type ScenarioKind string

const (
	KindDeterminism        ScenarioKind = "determinism"
	KindBudgetCompliance   ScenarioKind = "budget_compliance"
	KindContextCompression ScenarioKind = "context_compression"
	KindGoldenFiles        ScenarioKind = "golden_files"
	KindGoldenTests        ScenarioKind = "golden_tests"
	// KindGoldenSymbols checks that GetSymbols returns expected symbol names.
	KindGoldenSymbols ScenarioKind = "golden_symbols"
)

// EvalScenario is a static definition of one evaluation case.
type EvalScenario struct {
	ID          string
	Suite       SuiteID
	Kind        ScenarioKind
	Name        string
	Description string
	// Feature is the SuitCode feature being evaluated (e.g. "repo-overview").
	Feature string
	// Expectation carries the per-kind evaluation parameters.
	Expectation EvalExpectation
}

// EvalExpectation carries the per-kind check parameters for an EvalScenario.
type EvalExpectation struct {
	// Determinism: number of repeat runs that must produce identical hashes.
	RepeatCount int
	// BudgetCompliance: the token budget the response must stay within.
	BudgetLimit int
	// ContextCompression: minimum compression ratio (capsule/evidence ≤ threshold).
	MaxCompressionRatio float64
	// GoldenFiles/SeedFiles: seed files passed to Context() when running a
	// KindGoldenFiles scenario. Distinct from ExpectedFiles (which names files
	// the capsule must contain).
	SeedFiles []string
	// GoldenFiles: paths that must appear in the response.
	ExpectedFiles []string
	// GoldenFiles: paths that must NOT appear in the response.
	ForbiddenFiles []string
	// GoldenTests: test names that must appear in the response.
	ExpectedTests []string
	// GoldenSymbols: symbol names that must appear in GetSymbols() result
	// for SeedFiles[0].
	ExpectedSymbols []string
}

// EvalMetric is a single measurement produced during scenario execution.
type EvalMetric struct {
	Name   string
	Value  float64
	Passed bool
	Detail string
}

// EvalResult is the outcome of one scenario execution.
type EvalResult struct {
	ScenarioID  string
	ScenarioName string
	Passed      bool
	Metrics     []EvalMetric
	// Notes contains human-readable observations from the run.
	Notes []string
}

// EvalSummary aggregates pass/fail counts for a suite run.
type EvalSummary struct {
	Total   int
	Passed  int
	Failed  int
	Skipped int
	// Score = Passed / Total.
	Score float64
}

// EvalRun is the record of a complete suite execution.
type EvalRun struct {
	ID         string
	Suite      SuiteID
	RepoPath   string
	StartedAt  time.Time
	FinishedAt time.Time
	Results    []EvalResult
	Summary    EvalSummary
}

// EvalReport wraps an EvalRun with rendered narrative.
type EvalReport struct {
	Run     EvalRun
	Verdict string // "PASSED", "FAILED", "PARTIAL"
}

// ──────────────────────────────────────────────────────────────────────────────
// Factories
// ──────────────────────────────────────────────────────────────────────────────

// newEvalResult constructs an EvalResult pre-populated with the scenario's
// identity fields and Passed = true. Check methods call this instead of
// writing the same struct literal six times.
func newEvalResult(sc EvalScenario) EvalResult {
	return EvalResult{
		ScenarioID:   sc.ID,
		ScenarioName: sc.Name,
		Passed:       true,
	}
}

// newEvalRun constructs a fresh EvalRun with a unique ID and the current time
// as StartedAt. FinishedAt and Summary are filled in by Runner.Run once all
// scenarios have executed.
func newEvalRun(suite SuiteID, repoPath string) *EvalRun {
	return &EvalRun{
		ID:        fmt.Sprintf("eval-%s-%d", suite, time.Now().UnixMilli()),
		Suite:     suite,
		RepoPath:  repoPath,
		StartedAt: time.Now(),
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// EvalRun methods
// ──────────────────────────────────────────────────────────────────────────────

// PrintReport writes a human-readable Markdown report to w.
func (r *EvalRun) PrintReport(w io.Writer) {
	verdict := "PASSED"
	if r.Summary.Failed > 0 {
		verdict = "FAILED"
	}

	fmt.Fprintf(w, "# Eval Report: %s\n\n", r.Suite)
	fmt.Fprintf(w, "**Verdict:** %s  \n", verdict)
	fmt.Fprintf(w, "**Score:** %d/%d scenarios passed (%.0f%%)\n\n",
		r.Summary.Passed, r.Summary.Total, r.Summary.Score*100)
	fmt.Fprintf(w, "| Scenario | Result | Details |\n")
	fmt.Fprintf(w, "|----------|--------|--------|\n")

	for _, res := range r.Results {
		icon := "✓"
		if !res.Passed {
			icon = "✗"
		}

		var parts []string
		for _, m := range res.Metrics {
			parts = append(parts, m.Detail)
		}
		parts = append(parts, res.Notes...)
		details := strings.Join(parts, "; ")

		resultLabel := map[bool]string{true: "passed", false: "failed"}[res.Passed]
		fmt.Fprintf(w, "| %s %s | %s | %s |\n", icon, res.ScenarioName, resultLabel, details)
	}

	fmt.Fprintf(w, "\n_Run ID: %s · %s → %s_\n",
		r.ID,
		r.StartedAt.Format("15:04:05"),
		r.FinishedAt.Format("15:04:05"))
}
