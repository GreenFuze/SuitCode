// Package jsprovider implements the JavaScript/TypeScript language provider
// for SuitCode.
//
// Phase 1 (static): walks the repository tree, parses ES6 import/export and
// CommonJS require statements with regexes, and builds an in-memory
// bidirectional import graph. No external tools are required.
//
// Phase 2 (LSP symbols): not yet implemented. GetSymbols returns a
// "lsp_not_available" Limitation until TypeScript Language Server integration
// is added.
package jsprovider

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

const providerID provider.ProviderID = "js-language"

// JSLanguageProvider implements provider.ImportGraphProvider for JavaScript and
// TypeScript repositories.
//
// Detection: the provider is considered applicable (Ready() == true) when at
// least one .js/.ts/.jsx/.tsx source file is found in the repo tree (excluding
// node_modules and other vendor directories). This makes it usable for both
// pure JS/TS repos and monorepos that contain a JS/TS frontend alongside Go or
// Python code.
//
// JSLanguageProvider is safe for concurrent use after construction.
type JSLanguageProvider struct {
	repoPath string

	mu          sync.RWMutex
	idx         *jsImportIndex // nil until load succeeds
	limitations []provider.Limitation
	ready       bool
	loadOnce    sync.Once
}

// NewJSLanguageProvider creates a JSLanguageProvider for the given repo root.
// Phase 1 (import graph) runs synchronously before returning.
// An error is returned only when repoPath is fundamentally invalid (not a directory).
// All other failures (no JS/TS files, parse errors) are captured as Limitations
// and do not prevent the provider from being returned.
func NewJSLanguageProvider(_ context.Context, repoPath string) (*JSLanguageProvider, error) {
	info, err := os.Stat(repoPath)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("js language provider: path is not a valid directory: %s", repoPath)
	}

	p := &JSLanguageProvider{repoPath: repoPath}
	p.ensureLoaded()
	return p, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.Provider implementation
// ──────────────────────────────────────────────────────────────────────────────

// Capabilities returns the static metadata for this provider.
func (p *JSLanguageProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		ID:          providerID,
		DisplayName: "JS/TS Language Provider (static import analysis)",
		Roles:       []provider.ProviderRole{provider.RoleLanguage},
		Languages:   []string{"JavaScript", "TypeScript"},
	}
}

// Ready reports whether the Phase 1 import graph was built with at least one
// JS/TS source file found. Returns false for repos with no JS/TS files.
func (p *JSLanguageProvider) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ready
}

// Close is a no-op for Phase 1 (no subprocesses or open handles).
// Safe to call multiple times.
func (p *JSLanguageProvider) Close() error {
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.LanguageProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// GetImports returns the absolute paths of repo-local files directly imported
// by the file at filePath. External packages (bare specifiers like "react") are
// excluded — only relative imports that resolve to a file in the repo are returned.
func (p *JSLanguageProvider) GetImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := copyLimitations(p.limitations)
	var files []string
	if ready {
		files = p.idx.fileImports[filePath]
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindSyntax,
			SourceTool:      "js-import-parser",
			Authority:       provider.AuthorityHeuristic,
			EvidenceSummary: fmt.Sprintf("relative imports resolved from %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// GetSymbols is not yet implemented for JS/TS (requires a TypeScript Language
// Server subprocess). Returns a "lsp_not_available" Limitation. Callers should
// treat the empty result gracefully.
func (p *JSLanguageProvider) GetSymbols(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	p.mu.RLock()
	lims := copyLimitations(p.limitations)
	p.mu.RUnlock()

	lims = append(lims, provider.Limitation{
		Kind:    "lsp_not_available",
		Message: "symbol extraction via TypeScript Language Server is not yet implemented",
		Scope:   filePath,
	})
	return &provider.ProviderResult[[]string]{Limitations: lims}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.ImportGraphProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// FileImports returns the absolute paths of repo-local files directly imported
// by the file at filePath. node_modules and external packages are excluded.
func (p *JSLanguageProvider) FileImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := copyLimitations(p.limitations)
	var files []string
	if ready {
		files = p.idx.fileImports[filePath]
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindSyntax,
			SourceTool:      "js-import-parser",
			Authority:       provider.AuthorityHeuristic,
			EvidenceSummary: fmt.Sprintf("files directly imported by %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// FileImporters returns the absolute paths of repo-local files that directly
// import the file at filePath.
func (p *JSLanguageProvider) FileImporters(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := copyLimitations(p.limitations)
	var files []string
	if ready {
		files = p.idx.fileImporters[filePath]
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindSyntax,
			SourceTool:      "js-import-parser",
			Authority:       provider.AuthorityHeuristic,
			EvidenceSummary: fmt.Sprintf("files that directly import %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// ensureLoaded builds the import graph exactly once via sync.Once.
func (p *JSLanguageProvider) ensureLoaded() {
	p.loadOnce.Do(func() {
		idx, lims, err := buildImportGraph(p.repoPath)

		p.mu.Lock()
		defer p.mu.Unlock()

		p.limitations = append(p.limitations, lims...)

		if err != nil {
			p.limitations = append(p.limitations, provider.Limitation{
				Kind:    "js_graph_load_failed",
				Message: fmt.Sprintf("import graph build failed: %v", err),
				Scope:   p.repoPath,
			})
			return
		}

		// Not ready when no JS/TS files were found — the provider has nothing to offer.
		if idx.sourceFileCount == 0 {
			return
		}

		p.idx = idx
		p.ready = true
	})
}

// copyLimitations returns a copy of accumulated limitations.
// Caller must hold p.mu.RLock before calling.
func copyLimitations(lims []provider.Limitation) []provider.Limitation {
	if len(lims) == 0 {
		return nil
	}
	out := make([]provider.Limitation, len(lims))
	copy(out, lims)
	return out
}

// notReadyResult returns a ProviderResult with a "provider_not_ready" Limitation
// plus any accumulated load-time limitations.
func notReadyResult[T any](accumulated []provider.Limitation) *provider.ProviderResult[T] {
	lims := make([]provider.Limitation, 0, 1+len(accumulated))
	lims = append(lims, provider.Limitation{
		Kind:    "provider_not_ready",
		Message: "JSLanguageProvider is not ready — no JS/TS files found or import graph failed to load",
	})
	lims = append(lims, accumulated...)
	return &provider.ProviderResult[T]{Limitations: lims}
}

// ──────────────────────────────────────────────────────────────────────────────
// Compile-time interface assertions
// ──────────────────────────────────────────────────────────────────────────────

var _ provider.LanguageProvider = (*JSLanguageProvider)(nil)
var _ provider.ImportGraphProvider = (*JSLanguageProvider)(nil)
