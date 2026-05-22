package eval

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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
	// GetFileSymbols returns the symbol names defined in the file at absPath.
	// Returns nil (no error) when the language provider is unavailable or not ready.
	GetFileSymbols(ctx context.Context, absPath string) ([]string, error)
	// GoplsReady reports whether the gopls subprocess has been started and is
	// ready to answer symbol queries.
	GoplsReady() bool
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
	case SuiteGoProvider:
		scenarios = GoProviderScenarios(r.repoPath)
	case SuiteGoProviderSymbols:
		scenarios = GoProviderSymbolScenarios(r.repoPath)
	default:
		return nil, fmt.Errorf("eval runner: unknown suite %q; available: smoke, context-reduction, go-provider, go-provider-symbols", suite)
	}

	run := newEvalRun(suite, r.repoPath)

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
	result := newEvalResult(sc)

	switch sc.Kind {
	case KindDeterminism:
		result = r.checkDeterminism(ctx, sc)
	case KindBudgetCompliance:
		result = r.checkBudgetCompliance(ctx, sc)
	case KindContextCompression:
		result = r.checkContextCompression(ctx, sc)
	case KindGoldenFiles:
		result = r.checkGoldenFiles(ctx, sc)
	case KindGoldenSymbols:
		result = r.checkGoldenSymbols(ctx, sc)
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
	result := newEvalResult(sc)

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

		case "context":
			seeds := sc.Expectation.SeedFiles
			if len(seeds) == 0 {
				result.Notes = append(result.Notes, "no SeedFiles for context determinism; skipping")
				return result
			}
			resp, err := r.inv.Context(ctx, cfeatures.ContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{RepoPath: r.repoPath, Budget: budget},
				Files:              seeds,
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
	result := newEvalResult(sc)

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
	result := newEvalResult(sc)

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

// checkGoldenFiles runs Context() with SeedFiles and verifies that every
// ExpectedFile appears in the capsule (and every ForbiddenFile does not).
// It also records the import_edges_scanned metric for observability.
func (r *Runner) checkGoldenFiles(ctx context.Context, sc EvalScenario) EvalResult {
	result := newEvalResult(sc)

	budget := sc.Expectation.BudgetLimit
	if budget == 0 {
		budget = 8000
	}

	seeds := sc.Expectation.SeedFiles
	if len(seeds) == 0 {
		result.Notes = append(result.Notes, "no SeedFiles specified; skipping golden-files check")
		return result
	}

	// Call Context() with the specified seed files.
	resp, err := r.inv.Context(ctx, cfeatures.ContextRequest{
		BaseFeatureRequest: cfeatures.BaseFeatureRequest{RepoPath: r.repoPath, Budget: budget},
		Files:              seeds,
	})
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, fmt.Sprintf("Context() failed: %v", err))
		return result
	}

	// Build lookup sets for the included files — by full rel-path and by basename
	// for flexible matching.
	includedByRel := make(map[string]bool, len(resp.IncludedRelPaths))
	includedByBase := make(map[string]bool, len(resp.IncludedRelPaths))
	for _, rel := range resp.IncludedRelPaths {
		includedByRel[filepath.ToSlash(rel)] = true
		includedByBase[strings.ToLower(filepath.Base(rel))] = true
	}

	// Check that every expected file is present.
	for _, want := range sc.Expectation.ExpectedFiles {
		wantSlash := filepath.ToSlash(want)
		present := includedByRel[wantSlash] || includedByBase[strings.ToLower(filepath.Base(want))]
		passed := present
		if !passed {
			result.Passed = false
		}
		result.Metrics = append(result.Metrics, EvalMetric{
			Name:   fmt.Sprintf("golden_file_present:%s", wantSlash),
			Value:  boolToFloat(present),
			Passed: passed,
			Detail: fmt.Sprintf("expected %q in capsule: %v", want, present),
		})
	}

	// Check that every forbidden file is absent.
	for _, forbidden := range sc.Expectation.ForbiddenFiles {
		forbiddenSlash := filepath.ToSlash(forbidden)
		absent := !includedByRel[forbiddenSlash] && !includedByBase[strings.ToLower(filepath.Base(forbidden))]
		passed := absent
		if !passed {
			result.Passed = false
		}
		result.Metrics = append(result.Metrics, EvalMetric{
			Name:   fmt.Sprintf("golden_file_absent:%s", forbiddenSlash),
			Value:  boolToFloat(absent),
			Passed: passed,
			Detail: fmt.Sprintf("forbidden %q absent from capsule: %v", forbidden, absent),
		})
	}

	// Record import-graph observability metrics.
	edges := resp.Metrics.ContextReduction.ImportEdgesScanned
	result.Metrics = append(result.Metrics, EvalMetric{
		Name:   "import_edges_scanned",
		Value:  float64(edges),
		Passed: true,
		Detail: fmt.Sprintf("%d import edges examined during scoring", edges),
	})

	lspEnhanced := resp.Metrics.ContextReduction.LspEnhanced
	result.Metrics = append(result.Metrics, EvalMetric{
		Name:   "lsp_enhanced",
		Value:  boolToFloat(lspEnhanced),
		Passed: true,
		Detail: fmt.Sprintf("import-graph signals contributed to scoring: %v", lspEnhanced),
	})

	// On failure, dump the included paths for debugging.
	if !result.Passed {
		result.Notes = append(result.Notes,
			fmt.Sprintf("capsule contained %d files: %v", len(resp.IncludedRelPaths), resp.IncludedRelPaths))
	}

	return result
}

// checkGoldenSymbols calls GetFileSymbols for SeedFiles[0] and verifies that
// every ExpectedSymbol appears in the result. Waits up to 30 s for gopls to
// become ready; fails the scenario if gopls never starts or returns no symbols.
func (r *Runner) checkGoldenSymbols(ctx context.Context, sc EvalScenario) EvalResult {
	result := newEvalResult(sc)

	seeds := sc.Expectation.SeedFiles
	if len(seeds) == 0 {
		result.Passed = false
		result.Notes = append(result.Notes, "no SeedFiles specified for golden-symbols check")
		return result
	}

	// Wait up to 30 s for gopls to be ready — it starts asynchronously.
	const goplsTimeout = 30 * time.Second
	deadline := time.Now().Add(goplsTimeout)
	for !r.inv.GoplsReady() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}

	if !r.inv.GoplsReady() {
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("gopls not ready after %s — cannot evaluate symbol expectations", goplsTimeout))
		result.Metrics = append(result.Metrics, EvalMetric{
			Name:   "gopls_available",
			Value:  0,
			Passed: false,
			Detail: fmt.Sprintf("gopls did not become ready within %s", goplsTimeout),
		})
		return result
	}

	// Resolve the seed file to an absolute path.
	absPath := filepath.Join(r.repoPath, seeds[0])

	names, err := r.inv.GetFileSymbols(ctx, absPath)
	if err != nil {
		result.Passed = false
		result.Notes = append(result.Notes, fmt.Sprintf("GetFileSymbols(%s) failed: %v", seeds[0], err))
		return result
	}

	if len(names) == 0 {
		// gopls reported ready but returned no symbols — treat as a failure.
		result.Passed = false
		result.Notes = append(result.Notes,
			fmt.Sprintf("GetFileSymbols(%s) returned no symbols despite gopls being ready", seeds[0]))
		result.Metrics = append(result.Metrics, EvalMetric{
			Name:   "gopls_available",
			Value:  0,
			Passed: false,
			Detail: "gopls ready but returned empty symbol list — unexpected",
		})
		return result
	}

	// Record availability metric.
	result.Metrics = append(result.Metrics, EvalMetric{
		Name:   "gopls_available",
		Value:  1,
		Passed: true,
		Detail: fmt.Sprintf("gopls returned %d symbols from %s", len(names), seeds[0]),
	})

	// Check each expected symbol.
	// gopls returns Go methods as "(*Receiver).Method" so we match by
	// exact name or ".Name" suffix.
	for _, want := range sc.Expectation.ExpectedSymbols {
		present := symbolNamePresent(names, want)
		if !present {
			result.Passed = false
		}
		result.Metrics = append(result.Metrics, EvalMetric{
			Name:   fmt.Sprintf("golden_symbol_present:%s", want),
			Value:  boolToFloat(present),
			Passed: present,
			Detail: fmt.Sprintf("symbol %q present in %s: %v", want, seeds[0], present),
		})
	}

	if !result.Passed {
		result.Notes = append(result.Notes,
			fmt.Sprintf("returned symbols: %v", names))
	}

	return result
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// symbolNamePresent reports whether any symbol in names matches want.
// Accepts an exact match or a ".want" suffix to handle gopls's
// "(*Receiver).Method" method naming convention.
func symbolNamePresent(names []string, want string) bool {
	suffix := "." + want
	for _, n := range names {
		if n == want {
			return true
		}
		if len(n) > len(suffix) && n[len(n)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

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
