// Package main — the investigator binary's top-level package.
// LanguageDispatcher owns the four language providers and routes each query to
// exactly the one provider responsible for the file's extension. This replaces
// the old MultiLangProvider fan-out approach with precise per-extension dispatch.
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
	csprovider "github.com/GreenFuze/SuitCode/core/provider/language/csharp"
	goprovider "github.com/GreenFuze/SuitCode/core/provider/language/go"
	jsprovider "github.com/GreenFuze/SuitCode/core/provider/language/js"
	pyprovider "github.com/GreenFuze/SuitCode/core/provider/language/python"
)

const dispatcherProviderID provider.ProviderID = "language-dispatcher"

// jsExtensions is the set of extensions handled by the JS/TS provider.
var jsExtensions = map[string]bool{
	".js":  true,
	".ts":  true,
	".jsx": true,
	".tsx": true,
	".mjs": true,
	".cjs": true,
	".mts": true,
	".cts": true,
}

// LanguageDispatcher owns the four language providers (Go, JS/TS, Python, C#)
// and routes each query to exactly the one provider responsible for that file's
// extension. No fan-out occurs — each query touches at most one provider.
//
// LanguageDispatcher implements provider.ImportGraphProvider so that feature
// handler signatures are unchanged from the old MultiLangProvider approach.
//
// LanguageDispatcher is safe for concurrent use after construction.
type LanguageDispatcher struct {
	// Concrete typed fields — owned by this struct, created in the constructor.
	// Keeping them typed (not interface) gives callers access to provider-specific
	// methods (e.g. GoplsReady, WaitForGopls) without type assertions.
	goProvider *goprovider.GoLanguageProvider
	jsProvider *jsprovider.JSLanguageProvider
	pyProvider *pyprovider.PythonLanguageProvider
	csProvider *csprovider.CSHarpLanguageProvider
}

// NewLanguageDispatcher constructs a LanguageDispatcher for the given repository
// path. Each language provider is created in turn; providers that fail to
// initialise or report not-ready are closed immediately and discarded. At least
// one ready provider is not required here — the caller uses
// HasAnyLanguageProvider to decide whether to fail fast.
func NewLanguageDispatcher(ctx context.Context, repoPath string) *LanguageDispatcher {
	d := &LanguageDispatcher{}

	// ── Go provider (tool-backed: go/packages + gopls) ────────────────────────
	if goP, err := goprovider.NewGoLanguageProvider(ctx, repoPath); err == nil {
		if goP.Ready() {
			d.goProvider = goP
		} else {
			// Constructed but not ready (e.g. no Go module found). Release any
			// goroutines or file handles it may hold.
			_ = goP.Close()
		}
	}

	// ── JS/TS provider (static import graph + tsconfig alias resolution) ──────
	if jsP, err := jsprovider.NewJSLanguageProvider(ctx, repoPath); err == nil {
		if jsP.Ready() {
			d.jsProvider = jsP
		} else {
			_ = jsP.Close()
		}
	}

	// ── Python provider (static import graph) ─────────────────────────────────
	if pyP, err := pyprovider.NewPythonLanguageProvider(ctx, repoPath); err == nil {
		if pyP.Ready() {
			d.pyProvider = pyP
		} else {
			_ = pyP.Close()
		}
	}

	// ── C# provider (csproj ProjectReference graph + Avalonia pair detection) ─
	if csP, err := csprovider.NewCSHarpLanguageProvider(ctx, repoPath); err == nil {
		if csP.Ready() {
			d.csProvider = csP
		} else {
			_ = csP.Close()
		}
	}

	return d
}

// ──────────────────────────────────────────────────────────────────────────────
// Extension-based routing
// ──────────────────────────────────────────────────────────────────────────────

// providerFor returns the single provider responsible for filePath's extension,
// or nil if no provider handles that extension or the provider is not loaded.
// No fan-out — each file maps to at most one provider.
func (d *LanguageDispatcher) providerFor(filePath string) provider.ImportGraphProvider {
	ext := strings.ToLower(filepath.Ext(filePath))

	switch {
	case ext == ".go":
		if d.goProvider != nil {
			return d.goProvider
		}
	case jsExtensions[ext]:
		if d.jsProvider != nil {
			return d.jsProvider
		}
	case ext == ".py":
		if d.pyProvider != nil {
			return d.pyProvider
		}
	case ext == ".cs":
		if d.csProvider != nil {
			return d.csProvider
		}
	}

	return nil
}

// delegate calls fn on the provider responsible for filePath and returns the
// result. When no provider handles the extension, an empty result is returned
// rather than an error — the file type is simply unsupported.
func (d *LanguageDispatcher) delegate(
	filePath string,
	fn func(provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error),
) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p := d.providerFor(filePath)
	if p == nil {
		// Unknown extension — return an empty result, no limitations emitted.
		return &provider.ProviderResult[[]string]{
			Data:       []string{},
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	return fn(p)
}

// ──────────────────────────────────────────────────────────────────────────────
// Dispatcher-specific readiness queries
// ──────────────────────────────────────────────────────────────────────────────

// HasAnyLanguageProvider reports whether at least one language provider
// initialised successfully. The investigator uses this to decide whether to
// fail fast when no useful analysis is possible.
func (d *LanguageDispatcher) HasAnyLanguageProvider() bool {
	return len(d.activeProviders()) > 0
}

// GoReady reports whether the Go package graph has been loaded successfully.
func (d *LanguageDispatcher) GoReady() bool {
	return d.goProvider != nil && d.goProvider.Ready()
}

// GoplsReady reports whether the gopls LSP subprocess has completed its
// initialise handshake and is ready for symbol queries.
func (d *LanguageDispatcher) GoplsReady() bool {
	return d.goProvider != nil && d.goProvider.GoplsReady()
}

// WaitForGopls blocks until the Go provider's gopls subprocess has finished
// starting (success or failure) or until ctx is cancelled. Returns true if
// gopls is ready for symbol queries. Returns true immediately when no Go
// provider is present (gopls is not applicable to the repository).
func (d *LanguageDispatcher) WaitForGopls(ctx context.Context) bool {
	if d.goProvider == nil {
		return true
	}
	return d.goProvider.WaitForGopls(ctx)
}

// JSReady reports whether the JavaScript/TypeScript import graph is ready.
func (d *LanguageDispatcher) JSReady() bool {
	return d.jsProvider != nil && d.jsProvider.Ready()
}

// PyReady reports whether the Python import graph is ready.
func (d *LanguageDispatcher) PyReady() bool {
	return d.pyProvider != nil && d.pyProvider.Ready()
}

// CSReady reports whether the C# import graph (csproj ProjectReference graph) is ready.
func (d *LanguageDispatcher) CSReady() bool {
	return d.csProvider != nil && d.csProvider.Ready()
}

// GetCSPackageRefs returns the NuGet package references for the project
// containing filePath, via the C# provider. Returns nil when no C# provider
// is active or filePath does not belong to a tracked C# project.
func (d *LanguageDispatcher) GetCSPackageRefs(filePath string) []csprovider.PackageRef {
	if d.csProvider == nil {
		return nil
	}
	return d.csProvider.GetPackageRefs(filePath)
}

// GetDaemons returns DaemonInfo for all LSP subprocesses currently managed by
// active language providers. Providers that have no daemon (JS/TS static AST,
// Python static AST) are not represented. Nil providers are skipped.
func (d *LanguageDispatcher) GetDaemons() []provider.DaemonInfo {
	var daemons []provider.DaemonInfo

	// Go provider → gopls.
	if d.goProvider != nil {
		daemons = append(daemons, d.goProvider.DaemonInfo())
	}

	// C# provider → csharp-ls.
	if d.csProvider != nil {
		daemons = append(daemons, d.csProvider.DaemonInfo())
	}

	// JS/TS and Python providers currently use static AST (no daemon).

	return daemons
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.Provider implementation
// ──────────────────────────────────────────────────────────────────────────────

// Capabilities returns synthetic capabilities representing the union of all
// ready underlying providers' languages.
func (d *LanguageDispatcher) Capabilities() provider.ProviderCapabilities {
	langSeen := make(map[string]bool)
	var langs []string

	for _, p := range d.activeProviders() {
		for _, l := range p.Capabilities().Languages {
			if !langSeen[l] {
				langSeen[l] = true
				langs = append(langs, l)
			}
		}
	}
	sort.Strings(langs)

	return provider.ProviderCapabilities{
		ID:          dispatcherProviderID,
		DisplayName: "Language Dispatcher (extension-routed)",
		Roles:       []provider.ProviderRole{provider.RoleLanguage},
		Languages:   langs,
	}
}

// Ready returns true when at least one underlying provider is ready.
func (d *LanguageDispatcher) Ready() bool { return d.HasAnyLanguageProvider() }

// Close shuts down all underlying language providers. Safe to call multiple times.
func (d *LanguageDispatcher) Close() error {
	var errs []error

	if d.goProvider != nil {
		if err := d.goProvider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("go provider: %w", err))
		}
	}
	if d.jsProvider != nil {
		if err := d.jsProvider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("js provider: %w", err))
		}
	}
	if d.pyProvider != nil {
		if err := d.pyProvider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("python provider: %w", err))
		}
	}
	if d.csProvider != nil {
		if err := d.csProvider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("csharp provider: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("language dispatcher close: %v", errs)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.LanguageProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// GetImports returns the repo-local import path strings for the package
// containing filePath, routed to the single responsible provider.
func (d *LanguageDispatcher) GetImports(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return d.delegate(filePath, func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.GetImports(ctx, filePath)
	})
}

// GetSymbols returns the symbol names defined in filePath, routed to the
// single provider responsible for that file's extension.
func (d *LanguageDispatcher) GetSymbols(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return d.delegate(filePath, func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.GetSymbols(ctx, filePath)
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.ImportGraphProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// FileImports returns the repo-local files directly imported by filePath,
// routed to the single provider responsible for that file's extension.
func (d *LanguageDispatcher) FileImports(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return d.delegate(filePath, func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.FileImports(ctx, filePath)
	})
}

// FileImporters returns the repo-local files that directly import filePath,
// routed to the single provider responsible for that file's extension.
func (d *LanguageDispatcher) FileImporters(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return d.delegate(filePath, func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.FileImporters(ctx, filePath)
	})
}

// FilePeers returns the other source files in the same compilation unit as
// filePath (same Go package, same C# project), routed to the single provider
// responsible for that file's extension.
func (d *LanguageDispatcher) FilePeers(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return d.delegate(filePath, func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.FilePeers(ctx, filePath)
	})
}

// FileTests returns the test files for the compilation unit containing
// filePath, routed to the single provider responsible for that file's extension.
func (d *LanguageDispatcher) FileTests(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return d.delegate(filePath, func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.FileTests(ctx, filePath)
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// activeProviders returns a slice of all non-nil ready providers. Used only
// for Capabilities() and HasAnyLanguageProvider() — not for routing.
func (d *LanguageDispatcher) activeProviders() []provider.ImportGraphProvider {
	var active []provider.ImportGraphProvider

	if d.goProvider != nil {
		active = append(active, d.goProvider)
	}
	if d.jsProvider != nil {
		active = append(active, d.jsProvider)
	}
	if d.pyProvider != nil {
		active = append(active, d.pyProvider)
	}
	if d.csProvider != nil {
		active = append(active, d.csProvider)
	}

	return active
}

// ──────────────────────────────────────────────────────────────────────────────
// Compile-time interface assertion
// ──────────────────────────────────────────────────────────────────────────────

var _ provider.ImportGraphProvider = (*LanguageDispatcher)(nil)
