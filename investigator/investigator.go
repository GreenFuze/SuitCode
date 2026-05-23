// Package main — the investigator binary's top-level package.
// ProjectInvestigator is the central object that owns all provider instances
// and dispatches every feature request.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GreenFuze/SuitCode/calllog"
	"github.com/GreenFuze/SuitCode/core/config"
	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
	"github.com/GreenFuze/SuitCode/core/provider/filesystem"
	goprovider "github.com/GreenFuze/SuitCode/core/provider/language/go"
	"github.com/GreenFuze/SuitCode/core/provider/vcs"
	"github.com/GreenFuze/SuitCode/investigator/artifacts"
	invfeatures "github.com/GreenFuze/SuitCode/investigator/features"
)

// ReadinessLevel describes how much of the repository has been indexed.
type ReadinessLevel int

const (
	ReadinessUnknown ReadinessLevel = 0
	ReadinessLevel1  ReadinessLevel = 1 // repo identity, git state, language detection
	ReadinessLevel2  ReadinessLevel = 2 // full file index, ignore rules
	ReadinessLevel3  ReadinessLevel = 3 // symbol/import graph, test mapping (requires language providers)
	ReadinessLevel4  ReadinessLevel = 4 // expensive on-demand computations
)

// String returns a human-readable readiness description.
func (r ReadinessLevel) String() string {
	switch r {
	case ReadinessLevel1:
		return "level 1 — repo identity and language detection ready"
	case ReadinessLevel2:
		return "level 2 — full file index ready"
	case ReadinessLevel3:
		return "level 3 — symbol graph ready (requires language providers)"
	case ReadinessLevel4:
		return "level 4 — all computations available"
	default:
		return "not ready"
	}
}

// ProviderStatus summarises one provider's current state for the status command.
type ProviderStatus struct {
	ProviderID  provider.ProviderID
	DisplayName string
	Ready       bool
	Summary     string
}

// InvestigatorStatus is returned by Status() for the status command.
type InvestigatorStatus struct {
	RepoPath       string
	Readiness      ReadinessLevel
	ReadinessDesc  string
	Providers      []ProviderStatus
	LastWarmedAt   *time.Time
	WarmDurationMs int64
}

// ProjectInvestigator owns all provider instances for one repository and
// dispatches every feature request. It is the primary value object of the
// investigator binary.
type ProjectInvestigator struct {
	repoPath  string
	cfg       config.ProjectConfig
	estimator *provider.HeuristicEstimator

	// Providers
	fsProvider   *filesystem.Provider
	vcsProvider  *vcs.Provider
	langProvider *goprovider.GoLanguageProvider // nil if load failed or not a Go module

	// Cached file listing (populated during Warm).
	mu           sync.RWMutex
	fileListing  *provider.ProviderResult[provider.FilesystemListing]
	vcsStatus    *provider.ProviderResult[provider.VCSStatus]
	readiness    ReadinessLevel
	lastWarmed   *time.Time
	warmDuration time.Duration

	// Artifact store for persisting run metrics and eval results.
	store *artifacts.Store
	// callLogger appends per-call metrics to .suitcode/calls.jsonl.
	// Nil when the log cannot be opened (non-fatal).
	callLogger *calllog.Logger
}

// NewProjectInvestigator creates and attaches an investigator for the given
// repository path. Attach does NOT warm the investigator — call Warm() for that.
func NewProjectInvestigator(ctx context.Context, repoPath string) (*ProjectInvestigator, error) {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("investigator: resolving repo path: %w", err)
	}

	cfg := config.LoadProject(absPath)

	fsP, err := filesystem.NewFilesystemProvider(ctx, absPath)
	if err != nil {
		return nil, fmt.Errorf("investigator: filesystem provider: %w", err)
	}

	// VCS provider failure is not fatal — we can operate without git.
	vcsP, _ := vcs.NewVCSProvider(ctx, absPath)

	store, err := artifacts.Open(absPath)
	if err != nil {
		// Artifact store failure is not fatal in v1 — log and continue.
		store = nil
	}

	// Language provider: try to load the Go import graph. Non-fatal on failure —
	// the investigator falls back to heuristic-only scoring.
	langP, langErr := goprovider.NewGoLanguageProvider(ctx, absPath)
	if langErr != nil || !langP.Ready() {
		// If construction succeeded but the provider is not ready, release it
		// before discarding — it may own goroutines or file handles.
		if langErr == nil {
			_ = langP.Close()
		}
		langP = nil
	}

	// Require at least one meaningful provider beyond the filesystem layer.
	// A VCS-less, language-less directory offers too little value to justify
	// running a daemon. Fail fast so the coordinator surfaces a clear error
	// rather than serving empty responses forever.
	if vcsP == nil && langP == nil {
		_ = fsP.Close()
		return nil, fmt.Errorf(
			"investigator: %q has no supported providers — "+
				"not a git repository and no recognized language project "+
				"(currently supported languages: Go)",
			absPath,
		)
	}

	// Call logger: non-fatal.
	clog, _ := calllog.New(absPath)

	inv := &ProjectInvestigator{
		repoPath:     absPath,
		cfg:          cfg,
		estimator:    provider.NewHeuristicEstimator(),
		fsProvider:   fsP,
		vcsProvider:  vcsP,
		langProvider: langP,
		store:        store,
		callLogger:   clog,
	}

	return inv, nil
}

// Close releases all provider resources held by the investigator.
func (inv *ProjectInvestigator) Close() error {
	var errs []error

	if inv.fsProvider != nil {
		if err := inv.fsProvider.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if inv.vcsProvider != nil {
		if err := inv.vcsProvider.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if inv.langProvider != nil {
		if err := inv.langProvider.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if inv.store != nil {
		if err := inv.store.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("investigator close errors: %v", errs)
	}
	return nil
}

// Status returns the current readiness state and provider summary.
func (inv *ProjectInvestigator) Status() InvestigatorStatus {
	inv.mu.RLock()
	defer inv.mu.RUnlock()

	status := InvestigatorStatus{
		RepoPath:      inv.repoPath,
		Readiness:     inv.readiness,
		ReadinessDesc: inv.readiness.String(),
		LastWarmedAt:  inv.lastWarmed,
	}
	if inv.warmDuration > 0 {
		status.WarmDurationMs = inv.warmDuration.Milliseconds()
	}

	// Filesystem provider
	fsStatus := ProviderStatus{
		ProviderID:  inv.fsProvider.Capabilities().ID,
		DisplayName: inv.fsProvider.Capabilities().DisplayName,
		Ready:       inv.fsProvider.Ready(),
	}
	if inv.fileListing != nil {
		fsStatus.Summary = fmt.Sprintf("%d files indexed", inv.fileListing.Data.TotalFiles)
	}
	status.Providers = append(status.Providers, fsStatus)

	// VCS provider
	if inv.vcsProvider != nil {
		vcsStatus := ProviderStatus{
			ProviderID:  inv.vcsProvider.Capabilities().ID,
			DisplayName: inv.vcsProvider.Capabilities().DisplayName,
			Ready:       inv.vcsProvider.Ready(),
		}
		if inv.vcsStatus != nil {
			vcsStatus.Summary = fmt.Sprintf("branch=%s HEAD=%s clean=%v",
				inv.vcsStatus.Data.Branch,
				shortHash(inv.vcsStatus.Data.HeadHash),
				inv.vcsStatus.Data.IsClean,
			)
		}
		status.Providers = append(status.Providers, vcsStatus)
	} else {
		status.Providers = append(status.Providers, ProviderStatus{
			ProviderID:  "vcs",
			DisplayName: "VCS Provider (git)",
			Ready:       false,
			Summary:     "not attached (no git repository detected)",
		})
	}

	// Language (Go import graph + gopls) provider.
	if inv.langProvider != nil && inv.langProvider.Ready() {
		caps := inv.langProvider.Capabilities()
		var summary string
		if inv.langProvider.GoplsReady() {
			summary = "package graph + gopls ready"
		} else {
			summary = "package graph loaded (gopls starting or unavailable)"
		}
		status.Providers = append(status.Providers, ProviderStatus{
			ProviderID:  caps.ID,
			DisplayName: caps.DisplayName,
			Ready:       true,
			Summary:     summary,
		})
	} else {
		status.Providers = append(status.Providers, ProviderStatus{
			ProviderID:  "go-language",
			DisplayName: "Go Language Provider (go/packages + gopls)",
			Ready:       false,
			Summary:     "not ready (go/packages load failed or not a Go module)",
		})
	}

	// Future providers — shown as not attached.
	for _, name := range []string{"test", "build"} {
		status.Providers = append(status.Providers, ProviderStatus{
			ProviderID:  provider.ProviderID(name),
			DisplayName: fmt.Sprintf("%s provider", name),
			Ready:       false,
			Summary:     "not implemented in v1",
		})
	}

	return status
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature dispatch — each method delegates to the appropriate feature service.
// ──────────────────────────────────────────────────────────────────────────────

func (inv *ProjectInvestigator) RepoOverview(ctx context.Context, req cfeatures.RepoOverviewRequest) (*cfeatures.RepoOverviewResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("repo-overview: %w", err)
	}
	resp, err := invfeatures.RunRepoOverview(ctx, req, listing, inv.estimator)
	if err != nil {
		return nil, err
	}
	inv.appendCall(calllog.Record{
		Feature:         "repo-overview",
		BudgetRequested: req.Budget,
		BudgetUsed:      resp.Metrics.Budget.Used,
		LatencyMs:       resp.Metrics.Timing.DurationMs,
	})
	return resp, nil
}

func (inv *ProjectInvestigator) ExplainFile(ctx context.Context, req cfeatures.ExplainFileRequest) (*cfeatures.ExplainFileResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("explain-file: %w", err)
	}
	resp, err := invfeatures.RunExplainFile(ctx, req, listing, inv.estimator)
	if err != nil {
		return nil, err
	}
	inv.appendCall(calllog.Record{
		Feature:         "explain-file",
		SeedFiles:       relPaths(inv.repoPath, []string{req.FilePath}),
		BudgetRequested: req.Budget,
		BudgetUsed:      resp.Metrics.Budget.Used,
		LatencyMs:       resp.Metrics.Timing.DurationMs,
	})
	return resp, nil
}

func (inv *ProjectInvestigator) Related(ctx context.Context, req cfeatures.RelatedRequest) (*cfeatures.RelatedResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("related: %w", err)
	}
	resp, err := invfeatures.RunRelated(ctx, req, listing, inv.estimator)
	if err != nil {
		return nil, err
	}
	inv.appendCall(calllog.Record{
		Feature:         "related",
		SeedFiles:       relPaths(inv.repoPath, []string{req.FilePath}),
		BudgetRequested: req.Budget,
		BudgetUsed:      resp.Metrics.Budget.Used,
		LatencyMs:       resp.Metrics.Timing.DurationMs,
	})
	return resp, nil
}

func (inv *ProjectInvestigator) Tests(ctx context.Context, req cfeatures.TestsRequest) (*cfeatures.TestsResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("tests: %w", err)
	}
	resp, err := invfeatures.RunTests(ctx, req, listing, inv.estimator)
	if err != nil {
		return nil, err
	}
	inv.appendCall(calllog.Record{
		Feature:         "tests",
		SeedFiles:       relPaths(inv.repoPath, []string{req.FilePath}),
		BudgetRequested: req.Budget,
		BudgetUsed:      resp.Metrics.Budget.Used,
		LatencyMs:       resp.Metrics.Timing.DurationMs,
	})
	return resp, nil
}

func (inv *ProjectInvestigator) Impact(ctx context.Context, req cfeatures.ImpactRequest) (*cfeatures.ImpactResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("impact: %w", err)
	}

	var vcsResult *provider.ProviderResult[provider.VCSDiff]
	var vcsErr error
	if inv.vcsProvider != nil && req.GitRef != "" {
		vcsResult, vcsErr = inv.vcsProvider.Diff(ctx, req.GitRef, "")
		if vcsErr != nil {
			return nil, fmt.Errorf("impact: getting diff: %w", vcsErr)
		}
	}

	resp, err := invfeatures.RunImpact(ctx, req, listing, vcsResult, inv.estimator)
	if err != nil {
		return nil, err
	}
	inv.appendCall(calllog.Record{
		Feature:         "impact",
		SeedFiles:       relPaths(inv.repoPath, req.FilePaths),
		BudgetRequested: req.Budget,
		BudgetUsed:      resp.Metrics.Budget.Used,
		LatencyMs:       resp.Metrics.Timing.DurationMs,
	})
	return resp, nil
}

func (inv *ProjectInvestigator) Context(ctx context.Context, req cfeatures.ContextRequest) (*cfeatures.ContextResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("context: %w", err)
	}

	// Nil-safe interface assignment: a *goprovider.GoLanguageProvider nil pointer
	// must NOT be passed as a non-nil interface or method calls will panic.
	var langProv provider.ImportGraphProvider
	if inv.langProvider != nil {
		langProv = inv.langProvider
	}

	resp, err := invfeatures.RunContext(ctx, req, listing, inv.estimator, langProv)
	if err != nil {
		return nil, err
	}
	inv.appendCall(calllog.Record{
		Feature:            "context",
		SeedFiles:          relPaths(inv.repoPath, req.Files),
		FilesReturned:      resp.IncludedRelPaths,
		CandidatesTotal:    resp.FilesConsidered,
		FilesIncluded:      resp.FilesIncluded,
		CompressionRatio:   resp.CompressionRatio,
		BudgetRequested:    req.Budget,
		BudgetUsed:         resp.Metrics.Budget.Used,
		LatencyMs:          resp.Metrics.Timing.DurationMs,
		ImportEdgesScanned: resp.Metrics.ContextReduction.ImportEdgesScanned,
		LspEnhanced:        resp.Metrics.ContextReduction.LspEnhanced,
	})
	return resp, nil
}

func (inv *ProjectInvestigator) FailureContext(ctx context.Context, req cfeatures.FailureContextRequest) (*cfeatures.FailureContextResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("failure-context: %w", err)
	}

	// Same nil-safe interface pattern as Context().
	var langProv provider.ImportGraphProvider
	if inv.langProvider != nil {
		langProv = inv.langProvider
	}

	resp, err := invfeatures.RunFailureContext(ctx, req, listing, inv.estimator, langProv)
	if err != nil {
		return nil, err
	}
	inv.appendCall(calllog.Record{
		Feature:         "failure-context",
		BudgetRequested: req.Budget,
		BudgetUsed:      resp.Metrics.Budget.Used,
		LatencyMs:       resp.Metrics.Timing.DurationMs,
	})
	return resp, nil
}

func (inv *ProjectInvestigator) VerifyPlan(ctx context.Context, req cfeatures.VerifyPlanRequest) (*cfeatures.VerifyPlanResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify-plan: %w", err)
	}

	var vcsResult *provider.ProviderResult[provider.VCSDiff]
	var vcsErr error
	if inv.vcsProvider != nil && req.GitRef != "" {
		vcsResult, vcsErr = inv.vcsProvider.Diff(ctx, req.GitRef, "")
		if vcsErr != nil {
			return nil, fmt.Errorf("verify-plan: getting diff: %w", vcsErr)
		}
	}

	resp, err := invfeatures.RunVerifyPlan(ctx, req, listing, vcsResult, inv.estimator)
	if err != nil {
		return nil, err
	}
	inv.appendCall(calllog.Record{
		Feature:         "verify-plan",
		SeedFiles:       relPaths(inv.repoPath, req.FilePaths),
		BudgetRequested: req.Budget,
		BudgetUsed:      resp.Metrics.Budget.Used,
		LatencyMs:       resp.Metrics.Timing.DurationMs,
	})
	return resp, nil
}

// GoplsReady reports whether the gopls subprocess has been started and is ready
// to answer symbol queries. Returns false when no language provider is attached.
func (inv *ProjectInvestigator) GoplsReady() bool {
	if inv.langProvider == nil {
		return false
	}
	return inv.langProvider.GoplsReady()
}

// GetFileSymbols returns the symbol names defined in the file at absPath.
// Returns an empty slice (not an error) when gopls is not yet ready.
func (inv *ProjectInvestigator) GetFileSymbols(ctx context.Context, absPath string) ([]string, error) {
	if inv.langProvider == nil {
		return nil, nil
	}
	result, err := inv.langProvider.GetSymbols(ctx, absPath)
	if err != nil {
		return nil, fmt.Errorf("get-file-symbols: %w", err)
	}
	if result == nil {
		return nil, nil
	}
	return result.Data, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// ensureFileListing returns the cached file listing, refreshing it if not yet
// populated. Thread-safe.
func (inv *ProjectInvestigator) ensureFileListing(ctx context.Context) (*provider.ProviderResult[provider.FilesystemListing], error) {
	inv.mu.RLock()
	if inv.fileListing != nil {
		listing := inv.fileListing
		inv.mu.RUnlock()
		return listing, nil
	}
	inv.mu.RUnlock()

	inv.mu.Lock()
	defer inv.mu.Unlock()

	// Double-check after acquiring write lock.
	if inv.fileListing != nil {
		return inv.fileListing, nil
	}

	listing, err := inv.fsProvider.ListFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing files: %w", err)
	}

	inv.fileListing = listing
	return listing, nil
}

// appendCall appends a calllog record non-fatally. Failures are logged to
// stderr with the [sc investigator] prefix so the operator can diagnose issues,
// but they never block or error the feature call itself.
func (inv *ProjectInvestigator) appendCall(r calllog.Record) {
	if inv.callLogger == nil {
		return
	}
	if err := inv.callLogger.Append(r); err != nil {
		logf("warn: calllog: %v", err)
	}
}

// relPaths converts a slice of potentially-absolute paths to paths relative to
// repoPath. Already-relative paths are returned unchanged.
func relPaths(repoPath string, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		rel, err := filepath.Rel(repoPath, p)
		if err != nil || len(rel) > len(p) {
			// Not under repoPath or error — use as-is.
			out = append(out, filepath.ToSlash(p))
		} else {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out
}

// logf writes a timestamped message to stderr with the [sc investigator] prefix.
func logf(format string, args ...any) {
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[sc investigator] %s %s\n", ts, msg)
}

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}
