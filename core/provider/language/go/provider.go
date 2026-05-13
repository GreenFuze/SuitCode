package goprovider

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

const providerID provider.ProviderID = "go-language"

// GoLanguageProvider implements provider.ImportGraphProvider for Go repositories.
//
// Phase 1 (static): Loads the full package import graph once via
// golang.org/x/tools/go/packages. Provides FileImports, FileImporters, and
// GetImports with package-level granularity.
//
// Phase 2 (gopls): Starts a gopls subprocess asynchronously in Attach(). When
// ready, GetSymbols returns real symbol names via textDocument/documentSymbol.
// gopls readiness is independent of Phase 1 readiness — callers check
// GoplsReady() separately.
//
// Graceful degradation: if either phase fails, the provider records a
// Limitation and continues with whatever is available. Callers never receive
// hard errors from method calls — only Limitations in the result.
//
// GoLanguageProvider is safe for concurrent use after Attach returns.
type GoLanguageProvider struct {
	repoPath string

	// ── Phase 1: static package graph ─────────────────────────────────────────
	mu          sync.RWMutex
	idx         *packageIndex // nil until load succeeds
	limitations []provider.Limitation
	ready       bool
	loadOnce    sync.Once

	// ── Phase 2: gopls subprocess ─────────────────────────────────────────────
	// goplsBinary is the resolved path to the gopls executable.
	goplsBinary string
	// gopls is the high-level client; nil until ensureGoplsLoaded succeeds.
	gopls *goplsClient
	// goplsLimitations accumulates any limitation from gopls startup.
	goplsLimitations []provider.Limitation
	// goplsStartOnce ensures the gopls startup sequence runs exactly once.
	goplsStartOnce sync.Once
	// goplsReady is set to true after the LSP initialize handshake succeeds.
	goplsReady atomic.Bool
	// goplsClosed is set by Close() to prevent a post-close goroutine from
	// storing a freshly-initialized goplsClient.
	goplsClosed atomic.Bool
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
		DisplayName: "Go Language Provider (go/packages + gopls)",
		Roles:       []provider.ProviderRole{provider.RoleLanguage},
		Languages:   []string{"Go"},
	}
}

// Attach validates the repo path, eagerly loads the package graph (Phase 1),
// and starts the gopls subprocess asynchronously (Phase 2).
// Load or start failures are non-fatal — they record Limitations.
// Always returns nil.
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

	// Phase 1: load package graph synchronously (fast — in-process).
	p.ensureLoaded(ctx)

	// Phase 2: start gopls asynchronously. This can take a few seconds on
	// first run (binary download + LSP handshake). context.Background() is
	// used so the goroutine is not cancelled when Attach's ctx is done.
	go p.ensureGoplsLoaded(context.Background())

	return nil
}

// Ready reports whether the Phase 1 package graph has been loaded successfully.
// This is the readiness gate for FileImports, FileImporters, and GetImports.
// For gopls readiness, use GoplsReady().
func (p *GoLanguageProvider) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ready
}

// GoplsReady reports whether the Phase 2 gopls subprocess has been started and
// the LSP initialize handshake has completed successfully.
func (p *GoLanguageProvider) GoplsReady() bool {
	return p.goplsReady.Load()
}

// Close stops the gopls subprocess (if running) and releases resources.
// Safe to call multiple times.
func (p *GoLanguageProvider) Close() error {
	// Mark as closed first, so ensureGoplsLoaded will not store a new client
	// if it completes after Close() returns.
	p.goplsClosed.Store(true)

	p.mu.Lock()
	client := p.gopls
	p.gopls = nil
	p.mu.Unlock()

	if client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return client.Shutdown(ctx)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.LanguageProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// GetImports returns the import path strings of all packages directly imported
// by the package containing filePath.
func (p *GoLanguageProvider) GetImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := append([]provider.Limitation(nil), p.limitations...)
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

// GetSymbols returns the symbol names defined in the file at filePath.
//
// When gopls is ready: calls textDocument/documentSymbol and returns a flat
// list of names (hierarchical symbols are flattened to "Parent.Child").
// When gopls is not yet ready: returns a "gopls_not_ready" Limitation so
// callers can decide whether to wait or fall back.
func (p *GoLanguageProvider) GetSymbols(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	if !p.goplsReady.Load() {
		return &provider.ProviderResult[[]string]{
			Limitations: append(p.accumulatedGoplsLimitations(), provider.Limitation{
				Kind:    "gopls_not_ready",
				Message: "gopls is not yet ready (still starting or failed to start)",
				Scope:   filePath,
			}),
		}, nil
	}

	p.mu.RLock()
	client := p.gopls
	lims := p.accumulatedLimitations()
	p.mu.RUnlock()

	if client == nil {
		// goplsReady was true but client is nil — Close() was called concurrently.
		return &provider.ProviderResult[[]string]{Limitations: lims}, nil
	}

	symbols, err := client.DocumentSymbols(ctx, filePath)
	if err != nil {
		lims = append(lims, provider.Limitation{
			Kind:    "gopls_query_failed",
			Message: fmt.Sprintf("DocumentSymbols(%s): %v", filePath, err),
			Scope:   filePath,
		})
		return &provider.ProviderResult[[]string]{Limitations: lims}, nil
	}

	names := flattenSymbolNames(symbols)

	return &provider.ProviderResult[[]string]{
		Data: names,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindLSP,
			SourceTool:      "gopls",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("document symbols in %s via gopls", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.ImportGraphProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// FileImports returns the absolute paths of all non-test .go files in packages
// directly imported by the package containing filePath.
func (p *GoLanguageProvider) FileImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := append([]provider.Limitation(nil), p.limitations...)
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
func (p *GoLanguageProvider) FileImporters(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := append([]provider.Limitation(nil), p.limitations...)
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

// ensureGoplsLoaded starts gopls exactly once via sync.Once. Designed to be
// called as: go p.ensureGoplsLoaded(context.Background())
func (p *GoLanguageProvider) ensureGoplsLoaded(ctx context.Context) {
	p.goplsStartOnce.Do(func() {
		// Resolve the binary — auto-installs if not found.
		binary, lim := resolveBinary()
		if lim != nil {
			p.mu.Lock()
			p.goplsLimitations = append(p.goplsLimitations, *lim)
			p.mu.Unlock()
			return
		}

		client := newGoplsClient(binary, p.repoPath)

		initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		if err := client.Initialize(initCtx); err != nil {
			p.mu.Lock()
			p.goplsLimitations = append(p.goplsLimitations, provider.Limitation{
				Kind:    "gopls_init_failed",
				Message: fmt.Sprintf("gopls initialize failed: %v", err),
				Scope:   p.repoPath,
			})
			p.mu.Unlock()
			return
		}

		// Check if Close() was called while we were starting. If so, shut down
		// the freshly-initialized client rather than storing it.
		if p.goplsClosed.Load() {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel2()
			_ = client.Shutdown(ctx2)
			return
		}

		p.mu.Lock()
		p.goplsBinary = binary
		p.gopls = client
		p.mu.Unlock()

		p.goplsReady.Store(true)
	})
}

// accumulatedLimitations returns a copy of Phase 1 + Phase 2 limitations.
// Caller must hold p.mu.RLock.
func (p *GoLanguageProvider) accumulatedLimitations() []provider.Limitation {
	all := make([]provider.Limitation, 0, len(p.limitations)+len(p.goplsLimitations))
	all = append(all, p.limitations...)
	all = append(all, p.goplsLimitations...)
	return all
}

// accumulatedGoplsLimitations returns a copy of Phase 2 limitations without
// holding a lock (safe because goplsLimitations is only appended, never replaced).
func (p *GoLanguageProvider) accumulatedGoplsLimitations() []provider.Limitation {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]provider.Limitation(nil), p.goplsLimitations...)
}

// flattenSymbolNames recursively collects symbol names from a hierarchical
// document symbol tree. Top-level symbols produce "Name"; nested symbols
// (methods, fields) produce "Parent.Name".
func flattenSymbolNames(symbols []lspDocumentSymbol) []string {
	return flattenSymbolNamesWithPrefix(symbols, "")
}

func flattenSymbolNamesWithPrefix(symbols []lspDocumentSymbol, prefix string) []string {
	var names []string
	for _, sym := range symbols {
		name := sym.Name
		if prefix != "" {
			name = prefix + "." + sym.Name
		}
		names = append(names, name)
		if len(sym.Children) > 0 {
			names = append(names, flattenSymbolNamesWithPrefix(sym.Children, name)...)
		}
	}
	return names
}

// notReadyResult returns a ProviderResult with a "provider_not_ready" limitation
// and any accumulated load-time limitations.
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
