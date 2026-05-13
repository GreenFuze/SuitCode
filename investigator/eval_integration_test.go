package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GreenFuze/SuitCode/investigator/eval"
)

// ──────────────────────────────────────────────────────────────────────────────
// Eval suite wrappers
//
// Each function runs one eval suite as a Go test, with each scenario becoming
// a named sub-test. This makes failures granular and integrates eval coverage
// into the standard `go test` workflow.
//
// Run all: go test ./investigator/ -run TestEvalSuite -v -timeout 120s
// Run one: go test ./investigator/ -run TestEvalSuite_GoProvider -v -timeout 60s
// Skip:    go test ./investigator/ -short ./investigator/
// ──────────────────────────────────────────────────────────────────────────────

func TestEvalSuite_Smoke(t *testing.T) {
	skipIfShort(t, "smoke eval requires full investigator startup and 3-run determinism check")
	runEvalSuite(t, eval.SuiteSmoke)
}

func TestEvalSuite_GoProvider(t *testing.T) {
	skipIfShort(t, "go-provider eval requires go/packages load and import-graph scoring")
	runEvalSuite(t, eval.SuiteGoProvider)
}

func TestEvalSuite_ContextReduction(t *testing.T) {
	skipIfShort(t, "context-reduction eval requires full investigator and multiple context runs")
	runEvalSuite(t, eval.SuiteContextReduction)
}

func TestEvalSuite_GoProviderSymbols(t *testing.T) {
	skipIfShort(t, "go-provider-symbols eval requires gopls subprocess to be available")
	runEvalSuite(t, eval.SuiteGoProviderSymbols)
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// runEvalSuite executes all scenarios in the given suite and reports each one
// as a named sub-test. The parent test fails if any scenario fails.
func runEvalSuite(t *testing.T, suite eval.SuiteID) {
	t.Helper()

	inv := sharedInv(t)
	runner := eval.NewRunner(inv, inv.repoPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	run, err := runner.Run(ctx, suite)
	if err != nil {
		t.Fatalf("eval runner failed for suite %q: %v", suite, err)
	}

	t.Logf("suite %q: %d/%d passed in %s",
		suite,
		run.Summary.Passed,
		run.Summary.Total,
		run.FinishedAt.Sub(run.StartedAt).Round(time.Millisecond),
	)

	for _, result := range run.Results {
		result := result // capture for sub-test closure

		t.Run(scenarioSubtestName(result.ScenarioName), func(t *testing.T) {
			if result.Passed {
				// Log summary metrics for observability even on pass.
				for _, m := range result.Metrics {
					t.Logf("metric %-50s value=%-8.2f passed=%v  %s",
						m.Name, m.Value, m.Passed, m.Detail)
				}
				return
			}

			// Scenario failed — report each failed metric clearly.
			for _, m := range result.Metrics {
				if !m.Passed {
					t.Errorf("FAIL metric %q: %s", m.Name, m.Detail)
				} else {
					t.Logf("pass metric %q: %s", m.Name, m.Detail)
				}
			}
			for _, note := range result.Notes {
				t.Logf("note: %s", note)
			}
			t.Fail()
		})
	}
}

// scenarioSubtestName converts a scenario name to a valid Go sub-test name by
// replacing slashes (which Go uses as sub-test separators) and collapsing spaces.
func scenarioSubtestName(name string) string {
	name = strings.ReplaceAll(name, "/", "|")
	name = strings.ReplaceAll(name, " ", "_")
	return fmt.Sprintf("%s", name)
}
