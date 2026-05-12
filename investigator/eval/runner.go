package eval

import (
	"context"
	"fmt"
	"time"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
)

// Investigator is the subset of ProjectInvestigator methods the eval runner
// needs. Using a narrow interface keeps the eval package decoupled from the
// main binary package.
type Investigator interface {
	RepoOverview(ctx context.Context, req cfeatures.RepoOverviewRequest) (*cfeatures.RepoOverviewResponse, error)
	ExplainFile(ctx context.Context, req cfeatures.ExplainFileRequest) (*cfeatures.ExplainFileResponse, error)
	Context(ctx context.Context, req cfeatures.ContextRequest) (*cfeatures.ContextResponse, error)
}

// Runner executes eval scenarios against a live Investigator.
type Runner struct {
	inv      Investigator
	repoPath string
}

// NewRunner creates a Runner for the given investigator and repo path.
func NewRunner(inv Investigator, repoPath string) *Runner {
	return &Runner{inv: inv, repoPath: repoPath}
}

// Run executes all scenarios in the given suite and returns a completed EvalRun.
func (r *Runner) Run(ctx context.Context, suite SuiteID) (*EvalRun, error) {
	var scenarios []EvalScenario
	switch suite {
	case SuiteSmoke:
		scenarios = SmokeScenarios(r.repoPath)
	case SuiteContextReduction:
		scenarios = ContextReductionScenarios(r.repoPath)
	default:
		return nil, fmt.Errorf("eval runner: unknown suite %q; available: smoke, context-reduction", suite)
	}

	run := &EvalRun{
		ID:        fmt.Sprintf("eval-%s-%d", suite, time.Now().UnixMilli()),
		Suite:     suite,
		RepoPath:  r.repoPath,
		StartedAt: time.Now(),
	}

	for _, sc := range scenarios {
		result := r.runScenario(ctx, sc)
		run.Results = append(run.Results, result)
	}

	run.FinishedAt = time.Now()
	run.Summary = summarise(run.Results)

	return run, nil
}

// runScenario executes a single scenario and returns its result.
func (r *Runner) runScenario(ctx context.Context, sc EvalScenario) EvalResult {
	result := EvalResult{
		ScenarioID:   sc.ID,
		ScenarioName: sc.Name,
		Passed:       true,
	}

	switch sc.Kind {
	case KindDeterminism:
		result = r.checkDeterminism(ctx, sc)
	case KindBudgetCompliance:
		result = r.checkBudgetCompliance(ctx, sc)
	case KindContextCompression:
		result = r.checkContextCompression(ctx, sc)
	default:
		result.Passed = false
		result.Notes = append(result.Notes, fmt.Sprintf("scenario kind %q not implemented in v1", sc.Kind))
	}

	return result
}

// ──────────────────────────────────────────────────────────────────────────────
// Per-kind checks
// ──────────────────────────────────────────────────────────────────────────────

func (r *Runner) checkDeterminism(ctx context.Context, sc EvalScenario) EvalResult {
	result := EvalResult{
		ScenarioID:   sc.ID,
		ScenarioName: sc.Name,
		Passed:       true,
	}

	n := sc.Expectation.RepeatCount
	if n < 2 {
		n = 3 // default
	}

	hashes := make([]string, 0, n)
	budget := sc.Expectation.BudgetLimit
	if budget == 0 {
		budget = 3000
	}

	for i := 0; i < n; i++ {
		var hash string
		switch sc.Feature {
		case "repo-overview":
			resp, err := r.inv.RepoOverview(ctx, cfeatures.RepoOverviewRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: r.repoPath,
					Budget:   budget,
				},
			})
			if err != nil {
				result.Passed = false
				result.Notes = append(result.Notes, fmt.Sprintf("run %d failed: %v", i+1, err))
				return result
			}
			hash = resp.Metrics.DeterministicHash
		default:
			result.Passed = false
			result.Notes = append(result.Notes, fmt.Sprintf("determinism check not implemented for feature %q", sc.Feature))
			return result
		}
		hashes = append(hashes, hash)
	}

	// Check all hashes are identical.
	allSame := true
	for _, h := range hashes[1:] {
		if h != hashes[0] {
			allSame = false
			break
		}
	}

	result.Passed = allSame
	result.Metrics = append(result.Metrics, EvalMetric{
		Name:   "deterministic_hash_stable",
		Value:  boolToFloat(allSame),
		Passed: allSame,
		Detail: fmt.Sprintf("hash across %d runs: %v", n, hashesEqual(hashes)),
	})

	if !allSame {
		result.Notes = append(result.Notes, fmt.Sprintf("hashes differed across %d runs — output is non-deterministic", n))
	}

	return result
}

func (r *Runner) checkBudgetCompliance(ctx context.Context, sc EvalScenario) EvalResult {
	result := EvalResult{
		ScenarioID:   sc.ID,
		ScenarioName: sc.Name,
		Passed:       true,
	}

	budget := sc.Expectation.BudgetLimit
	if budget == 0 {
		budget = 3000
	}

	var used, requested int

	switch sc.Feature {
	case "repo-overview":
		resp, err := r.inv.RepoOverview(ctx, cfeatures.RepoOverviewRequest{
			BaseFeatureRequest: cfeatures.BaseFeatureRequest{RepoPath: r.repoPath, Budget: budget},
		})
		if err != nil {
			result.Passed = false
			result.Notes = append(result.Notes, err.Error())
			return result
		}
		used = resp.Metrics.Budget.Used
		requested = resp.Metrics.Budget.Requested

	case "context":
		resp, err := r.inv.Context(ctx, cfeatures.ContextRequest{
			BaseFeatureRequest: cfeatures.BaseFeatureRequest{RepoPath: r.repoPath, Budget: budget},
			Files:              sc.Expectation.ExpectedFiles,
		})
		if err != nil {
			result.Passed = false
			result.Notes = append(result.Notes, err.Error())
			return result
		}
		used = resp.Metrics.Budget.Used
		requested = resp.Metrics.Budget.Requested

	default:
		result.Notes = append(result.Notes, fmt.Sprintf("budget compliance check not implemented for feature %q", sc.Feature))
		return result
	}

	passed := requested == 0 || used <= requested
	result.Passed = passed
	result.Metrics = append(result.Metrics, EvalMetric{
		Name:   "budget_compliance",
		Value:  float64(used) / float64(max(requested, 1)),
		Passed: passed,
		Detail: fmt.Sprintf("used %d / requested %d tokens", used, requested),
	})

	return result
}

func (r *Runner) checkContextCompression(ctx context.Context, sc EvalScenario) EvalResult {
	result := EvalResult{
		ScenarioID:   sc.ID,
		ScenarioName: sc.Name,
		Passed:       true,
	}

	budget := sc.Expectation.BudgetLimit
	if budget == 0 {
		budget = 8000
	}

	files := sc.Expectation.ExpectedFiles
	if len(files) == 0 {
		result.Notes = append(result.Notes, "no seed files specified; skipping compression check")
		return result
	}

	resp, err := r.inv.Context(ctx, cfeatures.ContextRequest{
		BaseFeatureRequest: cfeatures.BaseFeatureRequest{RepoPath: r.repoPath, Budget: budget},
		Files:              files,
	})
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, err.Error())
		return result
	}

	ratio := resp.CompressionRatio
	maxAllowed := sc.Expectation.MaxCompressionRatio
	if maxAllowed == 0 {
		maxAllowed = 1.0 // no constraint by default
	}

	passed := ratio <= maxAllowed
	result.Passed = passed
	result.Metrics = append(result.Metrics,
		EvalMetric{
			Name:   "compression_ratio",
			Value:  ratio,
			Passed: passed,
			Detail: fmt.Sprintf("capsule/evidence ratio = %.2f (max allowed: %.2f)", ratio, maxAllowed),
		},
		EvalMetric{
			Name:   "estimated_context_avoided_tokens",
			Value:  float64(resp.EstimatedContextAvoided.Tokens),
			Passed: resp.EstimatedContextAvoided.Tokens > 0,
			Detail: fmt.Sprintf("~%d tokens not loaded by caller", resp.EstimatedContextAvoided.Tokens),
		},
		EvalMetric{
			Name:   "files_included",
			Value:  float64(resp.FilesIncluded),
			Passed: true,
			Detail: fmt.Sprintf("%d / %d files included", resp.FilesIncluded, resp.FilesConsidered),
		},
	)

	return result
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func summarise(results []EvalResult) EvalSummary {
	s := EvalSummary{Total: len(results)}
	for _, r := range results {
		if r.Passed {
			s.Passed++
		} else {
			s.Failed++
		}
	}
	if s.Total > 0 {
		s.Score = float64(s.Passed) / float64(s.Total)
	}
	return s
}

func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}
	return 0.0
}

func hashesEqual(hashes []string) string {
	for _, h := range hashes[1:] {
		if h != hashes[0] {
			return "not equal"
		}
	}
	return "all equal"
}
