package features

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultRelatedBudget = 4_000

// RunRelated finds files related to the target using import graph signals and
// filesystem heuristics. langProv may be nil; when provided it enriches scoring
// with direct-import and direct-importer edges (highest accuracy signals).
func RunRelated(
	ctx context.Context,
	req cfeatures.RelatedRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
	langProv provider.ImportGraphProvider,
) (*cfeatures.RelatedResponse, error) {
	if req.FilePath == "" {
		return nil, fmt.Errorf("related: --path is required")
	}

	budget := budgetOrDefault(req.Budget, defaultRelatedBudget)
	runID := newRunID("related")
	metrics, start := startMetrics(runID, "related", req.RepoPath, budget)

	fsFile, err := findFile(listing, req.FilePath, req.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("related: %w", err)
	}

	resp := &cfeatures.RelatedResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
		TargetPath:          fsFile.RelPath,
		FilesConsidered:     listing.Data.TotalFiles,
	}

	// Collect candidate related files with scores.
	type candidate struct {
		file     provider.FilesystemFile
		relation cfeatures.RelationKind
		reason   string
		score    float64
	}

	// ── Import-graph enrichment ───────────────────────────────────────────────
	//
	// When a language provider is available, pre-compute the set of files that
	// are directly imported by this file (forward edges) and files that directly
	// import this file (reverse edges). These are the highest-confidence signals
	// and are scored above all filesystem heuristics.

	importedAbsPaths := make(map[string]bool)
	importerAbsPaths := make(map[string]bool)

	if langProv != nil {
		seedAbs := fsFile.Path

		if res, err := langProv.FileImports(ctx, seedAbs); err == nil {
			for _, p := range res.Data {
				importedAbsPaths[p] = true
			}
		}
		if res, err := langProv.FileImporters(ctx, seedAbs); err == nil {
			for _, p := range res.Data {
				importerAbsPaths[p] = true
			}
		}
	}

	seen := make(map[string]bool)
	var candidates []candidate

	// 0. Import-graph edges — highest confidence signals.
	for _, f := range listing.Data.Files {
		if f.RelPath == fsFile.RelPath {
			continue
		}

		if importedAbsPaths[f.Path] {
			if !seen[f.RelPath] {
				seen[f.RelPath] = true
				candidates = append(candidates, candidate{
					f,
					cfeatures.RelationImports,
					"directly imported by this file (import graph)",
					0.92,
				})
			}
		} else if importerAbsPaths[f.Path] {
			if !seen[f.RelPath] {
				seen[f.RelPath] = true
				candidates = append(candidates, candidate{
					f,
					cfeatures.RelationImportedBy,
					"directly imports this file (import graph)",
					0.88,
				})
			}
		}
	}

	// 1. Test files for this source (or source files for a test).
	testFiles := testFilesForSource(listing, fsFile.RelPath)
	for _, tf := range testFiles {
		if seen[tf.RelPath] {
			continue
		}
		seen[tf.RelPath] = true
		rel := cfeatures.RelationTestedBy
		reason := "matches by naming convention (test file for this source)"
		if isTestFile(fsFile.RelPath) {
			rel = cfeatures.RelationTest
			reason = "this is the test file for the source"
		}
		candidates = append(candidates, candidate{tf, rel, reason, 0.95})
	}

	// 2. Files in the same directory (same package/module).
	sameDir := filesInSameDir(listing, fsFile.RelPath)
	for _, f := range sameDir {
		if seen[f.RelPath] {
			continue
		}
		seen[f.RelPath] = true
		score := 0.75
		if f.Role == "test" {
			score = 0.80
		}
		candidates = append(candidates, candidate{f, cfeatures.RelationSamePackage,
			"same directory/package", score})
	}

	// 3. Files with similar names in the repository (heuristic).
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(fsFile.RelPath),
		filepath.Ext(fsFile.RelPath)))
	for _, f := range listing.Data.Files {
		if seen[f.RelPath] || f.RelPath == fsFile.RelPath {
			continue
		}
		fStem := strings.ToLower(strings.TrimSuffix(filepath.Base(f.RelPath),
			filepath.Ext(f.RelPath)))
		if fStem == stem && f.Language == fsFile.Language {
			seen[f.RelPath] = true
			candidates = append(candidates, candidate{f, cfeatures.RelationSimilarName,
				fmt.Sprintf("same stem %q in different directory", stem), 0.40})
		}
	}

	// Sort by score descending.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	// Apply budget: include candidates that fit within the token budget.
	// We use continue (not break) so that a single large file cannot exclude
	// all subsequent smaller, high-value candidates.
	tokenUsed := 0
	budgetReached := false
	for _, c := range candidates {
		est, _ := estimator.EstimateFile(c.file.Path)
		if budget > 0 && tokenUsed+est.Tokens > budget {
			budgetReached = true
			continue // skip this candidate but keep checking smaller ones
		}
		tokenUsed += est.Tokens

		prov := heuristicProv("filesystem_heuristic",
			fmt.Sprintf("related to %s via %s", fsFile.RelPath, c.relation), c.file.Path)

		resp.RelatedFiles = append(resp.RelatedFiles, cfeatures.RelatedFile{
			File:       fileToRef(c.file, prov),
			Relation:   c.relation,
			Reason:     c.reason,
			Provenance: prov,
			Confidence: c.score,
		})
	}

	if budgetReached {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "budget_reached",
			Message: fmt.Sprintf("some candidates excluded — budget %d tokens used; large files skipped", tokenUsed),
			Scope:   "related_files",
		})
	}

	resp.FilesIncluded = len(resp.RelatedFiles)
	resp.FilesExcluded = len(candidates) - resp.FilesIncluded

	// Estimate context avoided.
	totalCandidateTokens := 0
	for _, c := range candidates {
		est, _ := estimator.EstimateFile(c.file.Path)
		totalCandidateTokens += est.Tokens
	}
	allFileTokens := 0
	for _, f := range listing.Data.Files {
		allFileTokens += estimator.Estimate(f.RelPath).Tokens
	}
	resp.EstimatedContextAvoided = provider.TokenEstimate{
		Tokens:     allFileTokens - tokenUsed,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}

	metrics.Budget.Used = tokenUsed
	computeContextReduction(&metrics, allFileTokens, tokenUsed,
		listing.Data.TotalFiles, resp.FilesIncluded, resp.FilesExcluded)

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}
