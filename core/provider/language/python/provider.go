// Package pyprovider implements the Python language provider for SuitCode.
//
// Phase 1 (static): walks the repository tree, parses "import" and "from …
// import" statements, builds a dotted-module-to-file map, and resolves both
// absolute and relative imports to absolute file paths in the repo. No
// external tools are required.
//
// Phase 2 (LSP symbols): not yet implemented. GetSymbols returns a
// "lsp_not_available" Limitation until pylsp / Pyright integration is added.
package pyprovider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

const providerID provider.ProviderID = "python-language"

// PythonLanguageProvider implements provider.ImportGraphProvider for Python
// repositories.
//
// Detection: the provider is considered applicable (Ready() == true) when at
// least one .py source file is found in the repo tree (excluding virtual
// environments, __pycache__, etc.). This makes it usable for pure Python repos
// and for monorepos that contain a Python component alongside Go or JS/TS code.
//
// PythonLanguageProvider is safe for concurrent use after construction.
type PythonLanguageProvider struct {
	repoPath string

	mu          sync.RWMutex
	idx         *pyImportIndex // nil until load succeeds
	limitations []provider.Limitation
	ready       bool
	loadOnce    sync.Once
}

// NewPythonLanguageProvider creates a PythonLanguageProvider for the given repo root.
// Phase 1 (import graph) runs synchronously before returning.
// An error is returned only when repoPath is fundamentally invalid (not a directory).
// All other failures (no .py files, parse errors) are captured as Limitations
// and do not prevent the provider from being returned.
func NewPythonLanguageProvider(_ context.Context, repoPath string) (*PythonLanguageProvider, error) {
	info, err := os.Stat(repoPath)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("python language provider: path is not a valid directory: %s", repoPath)
	}

	p := &PythonLanguageProvider{repoPath: repoPath}
	p.ensureLoaded()
	return p, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.Provider implementation
// ──────────────────────────────────────────────────────────────────────────────

// Capabilities returns the static metadata for this provider.
func (p *PythonLanguageProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		ID:          providerID,
		DisplayName: "Python Language Provider (static import analysis)",
		Roles:       []provider.ProviderRole{provider.RoleLanguage},
		Languages:   []string{"Python"},
	}
}

// Ready reports whether the Phase 1 import graph was built with at least one
// .py source file found. Returns false for repos with no Python files.
func (p *PythonLanguageProvider) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ready
}

// Close is a no-op for Phase 1 (no subprocesses or open handles).
// Safe to call multiple times.
func (p *PythonLanguageProvider) Close() error {
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.LanguageProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// GetImports returns the absolute paths of repo-local files directly imported
// by the file at filePath. Stdlib and third-party packages are excluded — only
// imports that resolve to a .py file within the repo are returned.
func (p *PythonLanguageProvider) GetImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
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
			SourceTool:      "python-import-parser",
			Authority:       provider.AuthorityHeuristic,
			EvidenceSummary: fmt.Sprintf("repo-local imports resolved from %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// GetSymbols is not yet implemented for Python (requires an LSP like pylsp or
// Pyright). Returns a "lsp_not_available" Limitation only for .py files.
// Files of other languages are silently ignored so the multi-provider does not
// emit spurious limitations for unrelated file types.
func (p *PythonLanguageProvider) GetSymbols(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	// Only emit limitations for files this provider actually handles.
	if strings.ToLower(filepath.Ext(filePath)) != ".py" {
		return &provider.ProviderResult[[]string]{}, nil
	}

	p.mu.RLock()
	lims := copyLimitations(p.limitations)
	p.mu.RUnlock()

	lims = append(lims, provider.Limitation{
		Kind:    "lsp_not_available",
		Message: "symbol extraction via Python LSP (pylsp / Pyright) is not yet implemented",
		Scope:   filePath,
	})
	return &provider.ProviderResult[[]string]{Limitations: lims}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.ImportGraphProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// FileImports returns the absolute paths of repo-local .py files directly
// imported by the file at filePath. Stdlib and third-party packages are excluded.
func (p *PythonLanguageProvider) FileImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
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
			SourceTool:      "python-import-parser",
			Authority:       provider.AuthorityHeuristic,
			EvidenceSummary: fmt.Sprintf("files directly imported by %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// FileImporters returns the absolute paths of repo-local .py files that
// directly import the file at filePath.
func (p *PythonLanguageProvider) FileImporters(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
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
			SourceTool:      "python-import-parser",
			Authority:       provider.AuthorityHeuristic,
			EvidenceSummary: fmt.Sprintf("files that directly import %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// FilePeers returns an empty result for Python. Python packages (directories
// with __init__.py) do not have a compiler-enforced membership list — the
// relationship between files in a directory is not structural in the same way
// as Go packages or C# projects. The import graph captures actual dependencies.
func (p *PythonLanguageProvider) FilePeers(_ context.Context, _ string) (*provider.ProviderResult[[]string], error) {
	return &provider.ProviderResult[[]string]{Data: []string{}}, nil
}

// FileTests returns an empty result for Python. pytest test discovery is
// naming-convention-based (test_*.py / *_test.py), not structural. Returning
// empty avoids masquerading heuristic results as verified facts.
func (p *PythonLanguageProvider) FileTests(_ context.Context, _ string) (*provider.ProviderResult[[]string], error) {
	return &provider.ProviderResult[[]string]{Data: []string{}}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// ensureLoaded builds the import graph exactly once via sync.Once.
func (p *PythonLanguageProvider) ensureLoaded() {
	p.loadOnce.Do(func() {
		idx, lims, err := buildPythonImportGraph(p.repoPath)

		p.mu.Lock()
		defer p.mu.Unlock()

		p.limitations = append(p.limitations, lims...)

		if err != nil {
			p.limitations = append(p.limitations, provider.Limitation{
				Kind:    "python_graph_load_failed",
				Message: fmt.Sprintf("import graph build failed: %v", err),
				Scope:   p.repoPath,
			})
			return
		}

		// Not ready when no .py files were found — the provider has nothing to offer.
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
		Message: "PythonLanguageProvider is not ready — no .py files found or import graph failed to load",
	})
	lims = append(lims, accumulated...)
	return &provider.ProviderResult[T]{Limitations: lims}
}

// ──────────────────────────────────────────────────────────────────────────────
// Compile-time interface assertions
// ──────────────────────────────────────────────────────────────────────────────

var _ provider.LanguageProvider = (*PythonLanguageProvider)(nil)
var _ provider.ImportGraphProvider = (*PythonLanguageProvider)(nil)
