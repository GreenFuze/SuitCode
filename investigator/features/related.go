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

// RunRelated finds files related to the target using filesystem heuristics.
func RunRelated(
	_ context.Context,
	req cfeatures.RelatedRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
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

	seen := make(map[string]bool)
	var candidates []candidate

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

	// Apply budget: include files until token budget is used.
	tokenUsed := 0
	for _, c := range candidates {
		est, _ := estimator.EstimateFile(c.file.Path)
		if budget > 0 && tokenUsed+est.Tokens > budget {
			resp.Limitations = append(resp.Limitations, provider.Limitation{
				Kind:    "budget_reached",
				Message: fmt.Sprintf("stopped after %d tokens; remaining candidates excluded", tokenUsed),
				Scope:   "related_files",
			})
			break
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
