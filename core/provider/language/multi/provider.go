// Package multiprovider implements a composite ImportGraphProvider that merges
// results from all underlying language providers. This allows polyglot
// repositories to benefit from every applicable provider simultaneously rather
// than silently falling back to the first one that is ready.
//
// Query results are the deduplicated, sorted union across all underlying
// providers. Provenance and Limitations from every provider are preserved so
// callers can reason about which provider contributed each result.
package multiprovider

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

const providerID provider.ProviderID = "multi-language"

// MultiLangProvider aggregates multiple ImportGraphProviders and merges their
// results. It is the single interface that the investigator's feature handlers
// interact with — they do not need to know which underlying language provider
// produced any given result.
//
// MultiLangProvider is safe for concurrent use; it holds no mutable state of
// its own and delegates all queries to the underlying providers.
type MultiLangProvider struct {
	providers []provider.ImportGraphProvider
}

// NewMultiLangProvider creates a MultiLangProvider wrapping the given providers.
// Nil entries — including typed-nil pointers wrapped in an interface — are
// silently discarded. Providers should already be in a Ready state when passed
// here; readiness is not re-checked per call.
func NewMultiLangProvider(providers ...provider.ImportGraphProvider) *MultiLangProvider {
	var valid []provider.ImportGraphProvider
	for _, p := range providers {
		if !isNilProvider(p) {
			valid = append(valid, p)
		}
	}
	return &MultiLangProvider{providers: valid}
}

// isNilProvider reports whether p is nil or is an interface holding a nil
// pointer. Go's interface nil check (p == nil) only catches the case where
// both the type and value parts of the interface are nil. When a typed nil
// (e.g. (*JSLanguageProvider)(nil)) is assigned to an interface variable, the
// interface itself is non-nil but the underlying pointer is nil — calling any
// method on it will panic. Reflection handles both cases correctly.
func isNilProvider(p provider.ImportGraphProvider) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Ptr, reflect.Chan, reflect.Func, reflect.Map, reflect.Slice, reflect.Interface:
		return v.IsNil()
	}
	return false
}

// Len returns the number of underlying providers.
func (m *MultiLangProvider) Len() int { return len(m.providers) }

// ──────────────────────────────────────────────────────────────────────────────
// provider.Provider implementation
// ──────────────────────────────────────────────────────────────────────────────

// Capabilities returns synthetic capabilities representing the union of all
// underlying providers' languages.
func (m *MultiLangProvider) Capabilities() provider.ProviderCapabilities {
	langSeen := make(map[string]bool)
	var langs []string

	for _, p := range m.providers {
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

// Ready returns true when at least one underlying provider is present.
// Callers pass only ready providers to New, so presence implies readiness.
func (m *MultiLangProvider) Ready() bool { return len(m.providers) > 0 }

// Close is a no-op. Individual providers are closed by their owners
// (ProjectInvestigator.Close) and must not be closed here.
func (m *MultiLangProvider) Close() error { return nil }

// ──────────────────────────────────────────────────────────────────────────────
// provider.LanguageProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// GetImports returns the deduplicated union of repo-local files imported by
// filePath, as reported by all underlying providers.
func (m *MultiLangProvider) GetImports(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return m.mergeStringSlice(func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.GetImports(ctx, filePath)
	})
}

// GetSymbols returns the deduplicated union of symbol names defined in filePath,
// as reported by all underlying providers.
func (m *MultiLangProvider) GetSymbols(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return m.mergeStringSlice(func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.GetSymbols(ctx, filePath)
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.ImportGraphProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// FileImports returns the deduplicated union of repo-local files directly
// imported by filePath, as reported by all underlying providers.
// In a polyglot repository each language provider only knows about its own
// file type, so the union naturally partitions by language.
func (m *MultiLangProvider) FileImports(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return m.mergeStringSlice(func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.FileImports(ctx, filePath)
	})
}

// FileImporters returns the deduplicated union of repo-local files that
// directly import filePath, as reported by all underlying providers.
func (m *MultiLangProvider) FileImporters(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	return m.mergeStringSlice(func(p provider.ImportGraphProvider) (*provider.ProviderResult[[]string], error) {
		return p.FileImporters(ctx, filePath)
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal merge helper
// ──────────────────────────────────────────────────────────────────────────────

// mergeStringSlice calls fn on every underlying provider and returns the
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

	for _, p := range m.providers {
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
