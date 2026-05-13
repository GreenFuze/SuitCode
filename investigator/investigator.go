// Package main — the investigator binary's top-level package.
// ProjectInvestigator is the central object that owns all provider instances
// and dispatches every feature request.
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

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
	ReadinessUnknown  ReadinessLevel = 0
	ReadinessLevel1   ReadinessLevel = 1 // repo identity, git state, language detection
	ReadinessLevel2   ReadinessLevel = 2 // full file index, ignore rules
	ReadinessLevel3   ReadinessLevel = 3 // symbol/import graph, test mapping (requires language providers)
	ReadinessLevel4   ReadinessLevel = 4 // expensive on-demand computations
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
	RepoPath      string
	Readiness     ReadinessLevel
	ReadinessDesc string
	Providers     []ProviderStatus
	LastWarmedAt  *time.Time
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
	mu          sync.RWMutex
	fileListing *provider.ProviderResult[provider.FilesystemListing]
	vcsStatus   *provider.ProviderResult[provider.VCSStatus]
	readiness   ReadinessLevel
	lastWarmed  *time.Time
	warmDuration time.Duration

	// Artifact store for persisting run metrics and eval results.
	store *artifacts.Store
}

// NewProjectInvestigator creates and attaches an investigator for the given
// repository path. Attach does NOT warm the investigator — call Warm() for that.
func NewProjectInvestigator(ctx context.Context, repoPath string) (*ProjectInvestigator, error) {
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("investigator: resolving repo path: %w", err)
	}

	cfg := config.LoadProject(absPath)

	fsP := filesystem.New()
	if err := fsP.Attach(ctx, absPath); err != nil {
		return nil, fmt.Errorf("investigator: attaching filesystem provider: %w", err)
	}

	vcsP := vcs.New()
	vcsErr := vcsP.Attach(ctx, absPath)
	// VCS provider failure is not fatal — we can operate without git.
	if vcsErr != nil {
		vcsP = nil
	}

	store, err := artifacts.Open(absPath)
	if err != nil {
		// Artifact store failure is not fatal in v1 — log and continue.
		store = nil
	}

	// Language provider: try to load the Go import graph. Non-fatal on failure —
	// the investigator falls back to heuristic-only scoring.
	langP := goprovider.New()
	if attachErr := langP.Attach(ctx, absPath); attachErr != nil || !langP.Ready() {
		langP = nil
	}

	inv := &ProjectInvestigator{
		repoPath:     absPath,
		cfg:          cfg,
		estimator:    provider.NewHeuristicEstimator(),
		fsProvider:   fsP,
		vcsProvider:  vcsP,
		langProvider: langP,
		store:        store,
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
	return invfeatures.RunRepoOverview(ctx, req, listing, inv.estimator)
}

func (inv *ProjectInvestigator) ExplainFile(ctx context.Context, req cfeatures.ExplainFileRequest) (*cfeatures.ExplainFileResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("explain-file: %w", err)
	}
	return invfeatures.RunExplainFile(ctx, req, listing, inv.estimator)
}

func (inv *ProjectInvestigator) Related(ctx context.Context, req cfeatures.RelatedRequest) (*cfeatures.RelatedResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("related: %w", err)
	}
	return invfeatures.RunRelated(ctx, req, listing, inv.estimator)
}

func (inv *ProjectInvestigator) Tests(ctx context.Context, req cfeatures.TestsRequest) (*cfeatures.TestsResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("tests: %w", err)
	}
	return invfeatures.RunTests(ctx, req, listing, inv.estimator)
}

func (inv *ProjectInvestigator) Impact(ctx context.Context, req cfeatures.ImpactRequest) (*cfeatures.ImpactResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("impact: %w", err)
	}

	var vcsResult *provider.ProviderResult[provider.VCSDiff]
	if inv.vcsProvider != nil && req.GitRef != "" {
		vcsResult, err = inv.vcsProvider.Diff(ctx, req.GitRef, "")
		if err != nil {
			return nil, fmt.Errorf("impact: getting diff: %w", err)
		}
	}

	return invfeatures.RunImpact(ctx, req, listing, vcsResult, inv.estimator)
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

	return invfeatures.RunContext(ctx, req, listing, inv.estimator, langProv)
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

	return invfeatures.RunFailureContext(ctx, req, listing, inv.estimator, langProv)
}

func (inv *ProjectInvestigator) VerifyPlan(ctx context.Context, req cfeatures.VerifyPlanRequest) (*cfeatures.VerifyPlanResponse, error) {
	listing, err := inv.ensureFileListing(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify-plan: %w", err)
	}

	var vcsResult *provider.ProviderResult[provider.VCSDiff]
	if inv.vcsProvider != nil && req.GitRef != "" {
		vcsResult, err = inv.vcsProvider.Diff(ctx, req.GitRef, "")
		if err != nil {
			return nil, fmt.Errorf("verify-plan: getting diff: %w", err)
		}
	}

	return invfeatures.RunVerifyPlan(ctx, req, listing, vcsResult, inv.estimator)
}

// GetFileSymbols returns the symbol names defined in the file at absPath.
// Returns an empty slice (not an error) when gopls is not yet ready.
// GoplsReady reports whether the gopls subprocess has been started and is ready
// to answer symbol queries. Returns false when no language provider is attached.
func (inv *ProjectInvestigator) GoplsReady() bool {
	if inv.langProvider == nil {
		return false
	}
	return inv.langProvider.GoplsReady()
}

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

	// Need to (re)load.
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

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}
