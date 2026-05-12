package features

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultImpactBudget = 6_000

// RunImpact analyses the blast radius of a set of changed files.
// vcsResult may be nil when no GitRef was provided.
func RunImpact(
	_ context.Context,
	req cfeatures.ImpactRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	vcsResult *provider.ProviderResult[provider.VCSDiff],
	estimator provider.TokenEstimator,
) (*cfeatures.ImpactResponse, error) {
	if len(req.FilePaths) == 0 && req.GitRef == "" {
		return nil, fmt.Errorf("impact: --files or --from is required")
	}

	budget := budgetOrDefault(req.Budget, defaultImpactBudget)
	runID := newRunID("impact")
	metrics, start := startMetrics(runID, "impact", req.RepoPath, budget)

	resp := &cfeatures.ImpactResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
	}

	// Resolve changed file list.
	changedRelPaths := req.FilePaths
	if vcsResult != nil && req.GitRef != "" {
		changedRelPaths = vcsResult.Data.ChangedFiles
	}

	if len(changedRelPaths) == 0 {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "no_changed_files",
			Message: "no changed files detected; verify the git ref or file list is correct",
			Scope:   "impact",
		})
		finishMetrics(&metrics, start, resp)
		resp.Metrics = metrics
		return resp, nil
	}

	// Resolve changed files against the index.
	for _, relPath := range changedRelPaths {
		fsFile, err := findFile(listing, relPath, req.RepoPath)
		if err != nil {
			// File not in index (e.g. deleted); add with limited info.
			resp.ChangedFiles = append(resp.ChangedFiles, provider.FileReference{
				RelPath: filepath.ToSlash(relPath),
				Provenance: heuristicProv("vcs_diff",
					fmt.Sprintf("changed file from diff: %s", relPath)),
			})
			continue
		}
		resp.ChangedFiles = append(resp.ChangedFiles, fileToRef(*fsFile,
			heuristicProv("vcs_diff", fmt.Sprintf("changed: %s", relPath), fsFile.Path)))
	}

	// Heuristic: files in the same directories as changed files are impacted.
	changedDirs := make(map[string]bool)
	for _, cf := range resp.ChangedFiles {
		dir := filepath.ToSlash(filepath.Dir(cf.RelPath))
		changedDirs[dir] = true
	}

	seen := make(map[string]bool)
	for _, cf := range resp.ChangedFiles {
		seen[cf.RelPath] = true
	}

	for _, f := range listing.Data.Files {
		if seen[f.RelPath] {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f.RelPath))
		if changedDirs[dir] {
			seen[f.RelPath] = true
			resp.ImpactedFiles = append(resp.ImpactedFiles, cfeatures.ImpactedFile{
				File: fileToRef(f, heuristicProv("same_directory",
					fmt.Sprintf("in same directory as changed file: %s", dir), f.Path)),
				Reason: fmt.Sprintf("same directory as changed file (%s)", dir),
			})
		}
	}

	// Find test files for changed files.
	for _, cf := range resp.ChangedFiles {
		testFiles := testFilesForSource(listing, cf.RelPath)
		for _, tf := range testFiles {
			prov := heuristicProv("naming_convention",
				fmt.Sprintf("test for changed file %s", cf.RelPath), tf.Path)
			resp.ImpactedTests = append(resp.ImpactedTests, cfeatures.RelevantTest{
				Test: cfeatures.TestReference{
					Name:       filepath.Base(tf.RelPath),
					FilePath:   tf.Path,
					RelPath:    tf.RelPath,
					RunCommand: buildTestCommand(tf, listing),
					Provenance: prov,
				},
				Reason:     fmt.Sprintf("directly tests changed file %s", cf.RelPath),
				Provenance: prov,
				Confidence: 0.90,
			})
		}
	}

	// Flag generated files in the blast radius.
	for _, f := range resp.ImpactedFiles {
		if f.File.Role == "generated" {
			resp.GeneratedWarnings = append(resp.GeneratedWarnings,
				fmt.Sprintf("`%s` appears to be generated — regenerate rather than edit manually", f.File.RelPath))
		}
	}

	// Heuristic: note that deeper impact analysis requires the language provider.
	resp.Limitations = append(resp.Limitations, provider.Limitation{
		Kind:    "no_import_graph",
		Message: "import-graph-based blast radius requires a language provider (not yet implemented); showing same-directory proximity only",
		Scope:   "impacted_files",
	})

	resp.FilesConsidered = listing.Data.TotalFiles
	resp.FilesIncluded = len(resp.ImpactedFiles) + len(resp.ChangedFiles)
	resp.FilesExcluded = resp.FilesConsidered - resp.FilesIncluded

	tokenUsed := estimator.Estimate(
		strings.Join(changedRelPaths, "\n"),
	).Tokens
	for _, f := range resp.ImpactedFiles {
		tokenUsed += estimator.Estimate(f.File.RelPath).Tokens
	}
	metrics.Budget.Used = tokenUsed

	allTokens := 0
	for _, f := range listing.Data.Files {
		allTokens += estimator.Estimate(f.RelPath).Tokens
	}
	computeContextReduction(&metrics, allTokens, tokenUsed,
		resp.FilesConsidered, resp.FilesIncluded, resp.FilesExcluded)

	resp.EstimatedContextAvoided = provider.TokenEstimate{
		Tokens:     allTokens - tokenUsed,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}
