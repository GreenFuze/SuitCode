package features

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

// absPathsToImportRefs converts a slice of absolute file paths (as returned by
// an ImportGraphProvider) into FileReferences by looking each up in the listing.
// Paths not found in the index are silently skipped.
func absPathsToImportRefs(absPaths []string, listing *provider.ProviderResult[provider.FilesystemListing], provSource string) []provider.FileReference {
	if len(absPaths) == 0 {
		return nil
	}

	// Build a lookup map for O(1) access.
	byPath := make(map[string]provider.FilesystemFile, len(listing.Data.Files))
	for _, f := range listing.Data.Files {
		byPath[f.Path] = f
	}

	var refs []provider.FileReference
	for _, p := range absPaths {
		f, ok := byPath[p]
		if !ok {
			continue
		}
		refs = append(refs, provider.FileReference{
			Path:     f.Path,
			RelPath:  f.RelPath,
			Language: f.Language,
			Role:     f.Role,
			Provenance: provider.Provenance{
				SourceKind:      provider.SourceKindSyntax,
				SourceTool:      provSource,
				Authority:       provider.AuthorityVerified,
				EvidenceSummary: fmt.Sprintf("import resolved by language provider from %s", f.RelPath),
				EvidencePaths:   []string{p},
			},
		})
	}
	return refs
}

const defaultExplainBudget = 6_000

// RunExplainFile produces an ExplainFileResponse for the given request.
// langProv may be nil; when provided it is used to resolve imports for
// languages the built-in heuristic scanner does not cover (JS/TS, Python).
func RunExplainFile(
	ctx context.Context,
	req cfeatures.ExplainFileRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
	langProv provider.ImportGraphProvider,
) (*cfeatures.ExplainFileResponse, error) {
	if req.FilePath == "" {
		return nil, fmt.Errorf("explain-file: --path is required")
	}

	budget := budgetOrDefault(req.Budget, defaultExplainBudget)
	runID := newRunID("explain-file")
	metrics, start := startMetrics(runID, "explain-file", req.RepoPath, budget)

	// Locate the file in the index.
	// When the file is not found we return a partial response with a
	// "file_not_found" limitation rather than a hard error so that the CLI
	// can exit 0 (succeeded with limitations) instead of exit 1 (hard failure).
	// A hard error here produces HTTP 500, which the client interprets as a
	// fatal failure even though the investigator itself is healthy.
	fsFile, err := findFile(listing, req.FilePath, req.RepoPath)
	if err != nil {
		finishMetrics(&metrics, start, nil)
		return &cfeatures.ExplainFileResponse{
			BaseFeatureResponse: cfeatures.BaseFeatureResponse{
				RunID: runID,
				Limitations: []provider.Limitation{{
					Kind:    "file_not_found",
					Message: fmt.Sprintf("file not found in repository index: %q — verify the path is relative to the project root %q", req.FilePath, req.RepoPath),
					Scope:   req.FilePath,
				}},
				Metrics: metrics,
			},
			FilePath: req.FilePath,
		}, nil
	}

	resp := &cfeatures.ExplainFileResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
		FilePath:            fsFile.Path,
		RelPath:             fsFile.RelPath,
		Language:            fsFile.Language,
		FileRole:            fsFile.Role,
	}

	// Build a path→file index for O(1) lookups when resolving abs paths returned
	// by the language provider.
	listingByPath := make(map[string]provider.FilesystemFile, len(listing.Data.Files))
	for _, f := range listing.Data.Files {
		listingByPath[f.Path] = f
	}

	// Estimate file size.
	fileEst, _ := estimator.EstimateFile(fsFile.Path)
	resp.FileTokenEstimate = fileEst

	// Populate symbols from the language provider when available.
	if langProv != nil && langProv.Ready() {
		symResult, symErr := langProv.GetSymbols(ctx, fsFile.Path)
		if symErr == nil && symResult != nil {
			prov := provider.Provenance{
				SourceKind:      provider.SourceKindSyntax,
				SourceTool:      "language-provider",
				Authority:       provider.AuthorityVerified,
				EvidenceSummary: fmt.Sprintf("symbols extracted from %s by language provider", fsFile.RelPath),
				EvidencePaths:   []string{fsFile.Path},
			}
			for _, name := range symResult.Data {
				resp.Symbols = append(resp.Symbols, cfeatures.SymbolInfo{
					Name:       name,
					Provenance: prov,
				})
			}
			resp.Limitations = append(resp.Limitations, symResult.Limitations...)
		} else if symErr != nil {
			resp.Limitations = append(resp.Limitations, provider.Limitation{
				Kind:    "symbols_query_failed",
				Message: fmt.Sprintf("symbol query failed for %s: %v", fsFile.RelPath, symErr),
				Scope:   fsFile.RelPath,
			})
		}
	}

	// Parse imports — use the language provider when available, else fall back
	// to the built-in heuristic scanners.
	imports, importLims := parseImports(ctx, fsFile, req.RepoPath, listing, langProv)
	resp.Imports = imports
	resp.Limitations = append(resp.Limitations, importLims...)

	// Populate Dependents (files that import this file) from the import graph.
	// This does not require Roslyn — it is a pure graph lookup.
	if langProv != nil && langProv.Ready() {
		importersResult, impErr := langProv.FileImporters(ctx, fsFile.Path)
		if impErr == nil && importersResult != nil {
			resp.Dependents = absPathsToImportRefs(importersResult.Data, listing, "import-graph-provider")
			resp.Limitations = append(resp.Limitations, importersResult.Limitations...)
		}
	}

	// ── Structural test discovery ─────────────────────────────────────────────
	//
	// FileTests returns spec-backed test files for the seed's compilation unit.
	// For Go this is the set of *_test.go files in the package directory
	// (Go spec §10.3). For other languages it returns empty. No naming heuristics.
	if langProv != nil {
		testRes, testErr := langProv.FileTests(ctx, fsFile.Path)
		if testErr == nil && testRes != nil {
			resp.Limitations = append(resp.Limitations, testRes.Limitations...)
			for _, absPath := range testRes.Data {
				tf, ok := listingByPath[absPath]
				if !ok {
					continue
				}
				prov := provider.Provenance{
					SourceKind:      provider.SourceKindSyntax,
					SourceTool:      "language-provider",
					Authority:       provider.AuthorityVerified,
					EvidenceSummary: fmt.Sprintf("test file for compilation unit containing %s (language spec)", fsFile.RelPath),
					EvidencePaths:   []string{absPath},
				}
				resp.RelatedTests = append(resp.RelatedTests, cfeatures.TestReference{
					Name:       filepath.Base(tf.RelPath),
					FilePath:   tf.Path,
					RelPath:    tf.RelPath,
					RunCommand: buildTestCommand(tf, listing),
					Framework:  detectFramework(listing),
					Provenance: prov,
				})
			}
		} else if testErr != nil {
			resp.Limitations = append(resp.Limitations, provider.Limitation{
				Kind:    "file_tests_query_failed",
				Message: fmt.Sprintf("test file query failed for %s: %v", fsFile.RelPath, testErr),
				Scope:   fsFile.RelPath,
			})
		}
	}

	// ── Structural peer discovery ─────────────────────────────────────────────
	//
	// FilePeers returns other source files in the same compilation unit — the
	// same Go package (go/packages) or the same C# project (.csproj manifest).
	// No directory-proximity heuristics.
	if langProv != nil {
		peersRes, peersErr := langProv.FilePeers(ctx, fsFile.Path)
		if peersErr == nil && peersRes != nil {
			resp.Limitations = append(resp.Limitations, peersRes.Limitations...)
			for _, absPath := range peersRes.Data {
				f, ok := listingByPath[absPath]
				if !ok {
					continue
				}
				if f.Role == "test" {
					continue // already covered by RelatedTests
				}
				resp.RelatedFiles = append(resp.RelatedFiles, provider.FileReference{
					Path:    f.Path,
					RelPath: f.RelPath,
					Language: f.Language,
					Role:    f.Role,
					Provenance: provider.Provenance{
						SourceKind:      provider.SourceKindSyntax,
						SourceTool:      "language-provider",
						Authority:       provider.AuthorityVerified,
						EvidenceSummary: fmt.Sprintf("%s is in the same compilation unit as %s", f.RelPath, fsFile.RelPath),
						EvidencePaths:   []string{absPath},
					},
				})
			}
		} else if peersErr != nil {
			resp.Limitations = append(resp.Limitations, provider.Limitation{
				Kind:    "file_peers_query_failed",
				Message: fmt.Sprintf("peer file query failed for %s: %v", fsFile.RelPath, peersErr),
				Scope:   fsFile.RelPath,
			})
		}
	}

	// Add risk notes.
	resp.RisksAndBoundaries = buildRisks(fsFile, listing)

	// Metrics.
	scanned := fileEst.Tokens
	for _, f := range resp.RelatedFiles {
		est, _ := estimator.EstimateFile(f.Path)
		scanned += est.Tokens
	}

	outputTokens := estimator.Estimate(
		resp.RelPath + resp.Language + resp.FileRole +
			fmt.Sprintf("%d imports %d related %d tests",
				len(resp.Imports), len(resp.RelatedFiles), len(resp.RelatedTests)),
	).Tokens

	metrics.Budget.Used = outputTokens
	computeContextReduction(&metrics, scanned, outputTokens,
		len(listing.Data.Files), 1+len(resp.RelatedFiles), listing.Data.TotalFiles-1-len(resp.RelatedFiles))

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// parseImports extracts import paths from a source file using the language
// provider. An empty result from the provider is authoritative — we never fall
// back to a weaker signal when a stronger one is available or has failed.
//
// If the provider is nil or not yet ready (pre-warmup), an empty result is
// returned with a "no_import_provider" limitation. Callers should run
// 'suitcode warmup' to ensure the import graph is initialised before calling
// explain-file. No heuristic fallbacks exist — a silent regex scan would
// produce incomplete results without clearly signalling the problem.
func parseImports(
	ctx context.Context,
	f *provider.FilesystemFile,
	_ string, // repoPath — unused; kept for signature stability
	listing *provider.ProviderResult[provider.FilesystemListing],
	langProv provider.ImportGraphProvider,
) ([]provider.FileReference, []provider.Limitation) {
	// Provider is available and ready — use it exclusively.
	if langProv != nil && langProv.Ready() {
		res, err := langProv.FileImports(ctx, f.Path)
		if err != nil {
			return nil, []provider.Limitation{{
				Kind:    "import_graph_query_failed",
				Message: fmt.Sprintf("import graph query failed for %s: %v", f.RelPath, err),
				Scope:   f.RelPath,
			}}
		}
		if res == nil {
			return nil, []provider.Limitation{{
				Kind:    "import_graph_no_result",
				Message: fmt.Sprintf("import graph provider returned nil result for %s", f.RelPath),
				Scope:   f.RelPath,
			}}
		}
		refs := absPathsToImportRefs(res.Data, listing, "import-graph-provider")
		return refs, res.Limitations
	}

	// Provider unavailable or not ready. Return empty imports with a clear
	// limitation rather than a heuristic scan that may produce incorrect results.
	// Possible causes: provider failed to load (installation error) or warmup
	// has not completed yet.
	return nil, []provider.Limitation{{
		Kind: "no_import_provider",
		Message: fmt.Sprintf(
			"no language provider ready for %s — run 'suitcode . warmup' to initialise the import graph",
			f.RelPath,
		),
		Scope: f.RelPath,
	}}
}

func buildTestCommand(f provider.FilesystemFile, listing *provider.ProviderResult[provider.FilesystemListing]) string {
	if f.Language == "Go" || strings.HasSuffix(f.RelPath, "_test.go") {
		dir := filepath.ToSlash(filepath.Dir(f.RelPath))
		if dir == "." {
			return "go test ."
		}
		return fmt.Sprintf("go test ./%s/...", dir)
	}
	for _, ts := range listing.Data.TestSystems {
		switch ts {
		case "pytest":
			return fmt.Sprintf("pytest %s", f.RelPath)
		case "Jest", "Vitest":
			return fmt.Sprintf("npx %s %s", strings.ToLower(ts), f.RelPath)
		}
	}
	return ""
}

func detectFramework(listing *provider.ProviderResult[provider.FilesystemListing]) string {
	if len(listing.Data.TestSystems) > 0 {
		return listing.Data.TestSystems[0]
	}
	return ""
}

func buildRisks(f *provider.FilesystemFile, _ *provider.ProviderResult[provider.FilesystemListing]) []string {
	var risks []string

	if f.Role == "generated" {
		risks = append(risks, "⚠ This file appears to be generated — edit the source template, not this file directly.")
	}

	// Check if this is an interface definition file.
	if f.Language == "Go" && (strings.Contains(f.RelPath, "interface") ||
		strings.Contains(strings.ToLower(filepath.Base(f.RelPath)), "iface")) {
		risks = append(risks, "This file may define interface boundaries. Changes here may break all implementors.")
	}

	// Check for API/handler files.
	base := strings.ToLower(filepath.Base(f.RelPath))
	if strings.Contains(base, "handler") || strings.Contains(base, "router") ||
		strings.Contains(base, "controller") || strings.Contains(base, "api") {
		risks = append(risks, "API surface file — changes may affect external callers or OpenAPI contracts.")
	}

	return risks
}
