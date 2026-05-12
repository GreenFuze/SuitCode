package goprovider

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

const providerID provider.ProviderID = "go-language"

// GoLanguageProvider implements provider.ImportGraphProvider for Go
// repositories. It loads the full package import graph once via
// golang.org/x/tools/go/packages (Phase 1) and will add gopls symbol
// navigation in Phase 2.
//
// If the go binary is unavailable or the module fails to load, the provider
// marks itself not-ready and all queries return a Limitation instead of an
// error. This ensures graceful degradation — callers fall back to heuristic
// scoring rather than failing hard.
//
// GoLanguageProvider is safe for concurrent use after Attach returns.
type GoLanguageProvider struct {
	repoPath string

	mu          sync.RWMutex
	idx         *packageIndex      // nil until load succeeds
	limitations []provider.Limitation
	ready       bool

	// loadOnce ensures the package graph is loaded exactly once regardless of
	// how many callers reach ensureLoaded concurrently.
	loadOnce sync.Once
}

// New returns an unattached GoLanguageProvider. Call Attach before use.
func New() *GoLanguageProvider {
	return &GoLanguageProvider{}
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.Provider implementation
// ──────────────────────────────────────────────────────────────────────────────

// Capabilities returns the static metadata for this provider.
func (p *GoLanguageProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		ID:          providerID,
		DisplayName: "Go Language Provider (go/packages)",
		Roles:       []provider.ProviderRole{provider.RoleLanguage},
		Languages:   []string{"Go"},
	}
}

// Attach validates the repo path and eagerly loads the package graph.
// Load failure is non-fatal: the provider stays not-ready and records
// a Limitation. Always returns nil.
func (p *GoLanguageProvider) Attach(ctx context.Context, repoPath string) error {
	info, err := os.Stat(repoPath)
	if err != nil || !info.IsDir() {
		p.mu.Lock()
		p.limitations = append(p.limitations, provider.Limitation{
			Kind:    "invalid_repo_path",
			Message: fmt.Sprintf("path is not a valid directory: %s", repoPath),
			Scope:   repoPath,
		})
		p.mu.Unlock()
		return nil
	}

	p.repoPath = repoPath
	p.ensureLoaded(ctx)
	return nil
}

// Ready reports whether the package graph has been successfully loaded.
func (p *GoLanguageProvider) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ready
}

// Close is a no-op for Phase 1 (no subprocess to stop).
func (p *GoLanguageProvider) Close() error { return nil }

// ──────────────────────────────────────────────────────────────────────────────
// provider.LanguageProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// GetImports returns the import path strings of all packages directly imported
// by the package containing filePath.
func (p *GoLanguageProvider) GetImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := p.limitations
	var node *packageNode
	if ready {
		node = p.idx.fileToNode(filePath)
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	var importIDs []string
	if node != nil {
		importIDs = node.ImportIDs
	}

	return &provider.ProviderResult[[]string]{
		Data: importIDs,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindManifest,
			SourceTool:      "golang.org/x/tools/go/packages",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("direct import paths of package containing %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// GetSymbols is not implemented in Phase 1 — gopls is required.
func (p *GoLanguageProvider) GetSymbols(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return &provider.ProviderResult[[]string]{
		Limitations: []provider.Limitation{{
			Kind:    "not_implemented",
			Message: "GetSymbols requires gopls (Phase 2 — not yet implemented)",
			Scope:   filePath,
		}},
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.ImportGraphProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// FileImports returns the absolute paths of all non-test .go files in packages
// directly imported by the package containing filePath.
func (p *GoLanguageProvider) FileImports(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := p.limitations
	var files []string
	if ready {
		files = p.idx.importedFiles(filePath)
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindManifest,
			SourceTool:      "golang.org/x/tools/go/packages",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("files in packages directly imported by package containing %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// FileImporters returns the absolute paths of all non-test .go files in
// packages that directly import the package containing filePath.
func (p *GoLanguageProvider) FileImporters(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := p.limitations
	var files []string
	if ready {
		files = p.idx.importerFiles(filePath)
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindManifest,
			SourceTool:      "golang.org/x/tools/go/packages",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("files in packages that directly import the package containing %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// ensureLoaded calls loadPackageGraph exactly once (via sync.Once).
// Must be called after p.repoPath is set.
func (p *GoLanguageProvider) ensureLoaded(ctx context.Context) {
	p.loadOnce.Do(func() {
		idx, lims, err := loadPackageGraph(ctx, p.repoPath)

		p.mu.Lock()
		defer p.mu.Unlock()

		p.limitations = append(p.limitations, lims...)

		if err != nil {
			p.limitations = append(p.limitations, provider.Limitation{
				Kind:    "go_packages_load_failed",
				Message: fmt.Sprintf("package graph load failed: %v", err),
				Scope:   p.repoPath,
			})
			return
		}

		p.idx = idx
		p.ready = true
	})
}

// notReadyResult returns a ProviderResult with a "provider_not_ready" limitation
// and any accumulated load-time limitations. Used as a safe fallback when the
// provider hasn't been successfully attached.
func notReadyResult[T any](accumulated []provider.Limitation) *provider.ProviderResult[T] {
	lims := make([]provider.Limitation, 0, 1+len(accumulated))
	lims = append(lims, provider.Limitation{
		Kind:    "provider_not_ready",
		Message: "GoLanguageProvider is not ready — package graph was not loaded successfully",
	})
	lims = append(lims, accumulated...)
	return &provider.ProviderResult[T]{Limitations: lims}
}

// ──────────────────────────────────────────────────────────────────────────────
// Compile-time interface assertions
// ──────────────────────────────────────────────────────────────────────────────

var _ provider.LanguageProvider    = (*GoLanguageProvider)(nil)
var _ provider.ImportGraphProvider = (*GoLanguageProvider)(nil)
