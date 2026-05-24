// Package multiprovider implements a composite language provider that owns all
// three per-language providers (Go, JS/TS, Python) and merges their results.
// This allows polyglot repositories to benefit from every applicable provider
// simultaneously rather than silently falling back to the first one that is ready.
//
// Query results are the deduplicated, sorted union across all underlying
// providers. Provenance and Limitations from every provider are preserved so
// callers can reason about which provider contributed each result.
package multiprovider

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
	csprovider "github.com/GreenFuze/SuitCode/core/provider/language/csharp"
	goprovider "github.com/GreenFuze/SuitCode/core/provider/language/go"
	jsprovider "github.com/GreenFuze/SuitCode/core/provider/language/js"
	pyprovider "github.com/GreenFuze/SuitCode/core/provider/language/python"
)

const providerID provider.ProviderID = "multi-language"

// MultiLangProvider creates and owns the Go, JS/TS, and Python language
// providers for one repository. It is the single language-provider interface
// that the investigator's feature handlers interact with — they do not need to
// know which underlying provider produced any given result.
//
// MultiLangProvider is safe for concurrent use after construction.
type MultiLangProvider struct {
	// Concrete typed fields — owned by this struct, created in the constructor.
	// Keeping them typed (not interface) lets callers access provider-specific
	// behaviour (e.g. GoplsReady) without additional type assertions.
	goProvider *goprovider.GoLanguageProvider
	jsProvider *jsprovider.JSLanguageProvider
	pyProvider *pyprovider.PythonLanguageProvider
	csProvider *csprovider.CSHarpLanguageProvider

	// active is the internal dispatch slice built from the non-nil providers
	// above. It is an implementation detail — not an externally supplied list.
	active []provider.ImportGraphProvider
}

// NewMultiLangProvider constructs a MultiLangProvider for the given repository
// path. It creates each language provider in turn; providers that fail to
// initialise or report not-ready are closed immediately and skipped. At least
// one ready provider is not required here — the caller uses
// HasAnyLanguageProvider to decide whether to fail fast.
func NewMultiLangProvider(ctx context.Context, repoPath string) *MultiLangProvider {
	m := &MultiLangProvider{}

	// ── Go provider (tool-backed: go/packages + gopls) ────────────────────────
	if goP, err := goprovider.NewGoLanguageProvider(ctx, repoPath); err == nil {
		if goP.Ready() {
			m.goProvider = goP
			m.active = append(m.active, goP)
		} else {
			// Constructed but not ready (e.g. no Go module found). Release any
			// goroutines or file handles it may hold.
			_ = goP.Close()
		}
	}

	// ── JS/TS provider (static import graph + tsconfig alias resolution) ──────
	if jsP, err := jsprovider.NewJSLanguageProvider(ctx, repoPath); err == nil {
		if jsP.Ready() {
			m.jsProvider = jsP
			m.active = append(m.active, jsP)
		} else {
			_ = jsP.Close()
		}
	}

	// ── Python provider (static import graph) ─────────────────────────────────
	if pyP, err := pyprovider.NewPythonLanguageProvider(ctx, repoPath); err == nil {
		if pyP.Ready() {
			m.pyProvider = pyP
			m.active = append(m.active, pyP)
		} else {
			_ = pyP.Close()
		}
	}

	// ── C# provider (csproj ProjectReference graph + Avalonia pair detection) ─
	if csP, err := csprovider.NewCSHarpLanguageProvider(ctx, repoPath); err == nil {
		if csP.Ready() {
			m.csProvider = csP
			m.active = append(m.active, csP)
		} else {
			_ = csP.Close()
		}
	}

	return m
}

// HasAnyLanguageProvider reports whether at least one language provider
// initialised successfully. The investigator uses this to decide whether to
// fail fast when no useful analysis is possible.
func (m *MultiLangProvider) HasAnyLanguageProvider() bool {
	return len(m.active) > 0
}

// ──────────────────────────────────────────────────────────────────────────────
// Per-provider readiness queries
// ──────────────────────────────────────────────────────────────────────────────

// GoReady reports whether the Go package graph has been loaded successfully.
func (m *MultiLangProvider) GoReady() bool {
	return m.goProvider != nil && m.goProvider.Ready()
}

// GoplsReady reports whether the gopls LSP subprocess has completed its
// initialise handshake and is ready for symbol queries.
func (m *MultiLangProvider) GoplsReady() bool {
	return m.goProvider != nil && m.goProvider.GoplsReady()
}

// JSReady reports whether the JavaScript/TypeScript import graph is ready.
func (m *MultiLangProvider) JSReady() bool {
	return m.jsProvider != nil && m.jsProvider.Ready()
}

// PyReady reports whether the Python import graph is ready.
func (m *MultiLangProvider) PyReady() bool {
	return m.pyProvider != nil && m.pyProvider.Ready()
}

// CSReady reports whether the C# import graph (csproj ProjectReference graph) is ready.
func (m *MultiLangProvider) CSReady() bool {
	return m.csProvider != nil && m.csProvider.Ready()
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.Provider implementation
// ──────────────────────────────────────────────────────────────────────────────

// Capabilities returns synthetic capabilities representing the union of all
// ready underlying providers' languages.
func (m *MultiLangProvider) Capabilities() provider.ProviderCapabilities {
	langSeen := make(map[string]bool)
	var langs []string

	for _, p := range m.active {
		for _, l := range p.Capabilities().Languages {
			if !langSeen[l] {
				langSeen[l] = true
				langs = append(langs, l)
			}
		}
	}
	sort.Strings(langs)

	return provider.ProviderCapabilities{
		ID:          providerID,
		DisplayName: "Multi-Language Provider (composite)",
		Roles:       []provider.ProviderRole{provider.RoleLanguage},
		Languages:   langs,
	}
}

// Ready returns true when at least one underlying provider is ready.
func (m *MultiLangProvider) Ready() bool { return len(m.active) > 0 }

// Close shuts down all underlying language providers. Safe to call multiple times.
func (m *MultiLangProvider) Close() error {
	var errs []error

	if m.goProvider != nil {
		if err := m.goProvider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("go provider: %w", err))
		}
	}
	if m.jsProvider != nil {
		if err := m.jsProvider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("js provider: %w", err))
		}
	}
	if m.pyProvider != nil {
		if err := m.pyProvider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("python provider: %w", err))
		}
	}
	if m.csProvider != nil {
		if err := m.csProvider.Close(); err != nil {
			errs = append(errs, fmt.Errorf("csharp provider: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("multi-language provider close: %v", errs)
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.LanguageProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// GetImports returns the deduplicated union of repo-local import path strings
// for the package containing filePath, across all ready providers.
func (m *MultiLangProvider) GetImports(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return m.mergeStringSlice(func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.GetImports(ctx, filePath)
	})
}

// GetSymbols returns the deduplicated union of symbol names defined in filePath,
// as reported by all ready underlying providers.
func (m *MultiLangProvider) GetSymbols(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return m.mergeStringSlice(func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.GetSymbols(ctx, filePath)
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.ImportGraphProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// FileImports returns the deduplicated union of repo-local files directly
// imported by filePath, as reported by all ready underlying providers.
// In a polyglot repository each language provider only knows about its own
// file type, so the union naturally partitions by language.
func (m *MultiLangProvider) FileImports(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return m.mergeStringSlice(func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.FileImports(ctx, filePath)
	})
}

// FileImporters returns the deduplicated union of repo-local files that
// directly import filePath, as reported by all ready underlying providers.
func (m *MultiLangProvider) FileImporters(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return m.mergeStringSlice(func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.FileImporters(ctx, filePath)
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal merge helper
// ──────────────────────────────────────────────────────────────────────────────

// mergeStringSlice calls fn on every active underlying provider and returns the
// deduplicated, sorted union of all non-empty results. Provenance and
// Limitations from all providers are preserved in the merged result.
func (m *MultiLangProvider) mergeStringSlice(
	fn func(provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error),
) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	var allData []string
	var allProvenance []provider.Provenance
	var allLimitations []provider.Limitation
	seen := make(map[string]bool)

	for _, p := range m.active {
		res, err := fn(p)
		if err != nil {
			// Record as a non-fatal limitation rather than aborting the merge.
			allLimitations = append(allLimitations, provider.Limitation{
				Kind:    "provider_error",
				Message: err.Error(),
			})
			continue
		}
		if res == nil {
			continue
		}

		// Deduplicate across providers.
		for _, d := range res.Data {
			if !seen[d] {
				seen[d] = true
				allData = append(allData, d)
			}
		}

		allProvenance = append(allProvenance, res.Provenance...)
		allLimitations = append(allLimitations, res.Limitations...)
	}

	sort.Strings(allData)

	return &provider.ProviderResult[[]string]{
		Data:        allData,
		Provenance:  allProvenance,
		Limitations: allLimitations,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Compile-time interface assertion
// ──────────────────────────────────────────────────────────────────────────────

var _ provider.ImportGraphProvider = (*MultiLangProvider)(nil)
