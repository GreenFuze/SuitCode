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
// langProv may be nil; when provided, impacted files are discovered via the
// import graph (FileImporters) rather than directory proximity, and test files
// are discovered via FileTests rather than naming conventions.
func RunImpact(
	ctx context.Context,
	req cfeatures.ImpactRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	vcsResult *provider.ProviderResult[provider.VCSDiff],
	estimator provider.TokenEstimator,
	langProv provider.ImportGraphProvider,
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
	// When using --files (not --from), expand any directory entries so that
	// "suitcode . impact --files server" includes all files under server/.
	var changedRelPaths []string
	if vcsResult != nil && req.GitRef != "" {
		changedRelPaths = vcsResult.Data.ChangedFiles
	} else {
		seen := make(map[string]bool)
		for _, p := range req.FilePaths {
			expanded, err := findFilesOrDir(listing, p, req.RepoPath)
			if err != nil {
				// Treat unresolvable paths as-is (e.g. deleted files not in index).
				if !seen[p] {
					seen[p] = true
					changedRelPaths = append(changedRelPaths, p)
				}
				continue
			}
			for _, f := range expanded {
				if !seen[f.Path] {
					seen[f.Path] = true
					changedRelPaths = append(changedRelPaths, f.Path)
				}
			}
		}
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

	// Build a path→file index for O(1) lookups when resolving abs paths.
	listingByPath := make(map[string]provider.FilesystemFile, len(listing.Data.Files))
	for _, f := range listing.Data.Files {
		listingByPath[f.Path] = f
	}

	// Resolve changed files against the index.
	for _, relPath := range changedRelPaths {
		fsFile, err := findFile(listing, relPath, req.RepoPath)
		if err != nil {
			// File not in index (e.g. deleted); add with limited info.
			resp.ChangedFiles = append(resp.ChangedFiles, provider.FileReference{
				RelPath: filepath.ToSlash(relPath),
				Provenance: provider.Provenance{
					SourceKind:      provider.SourceKindGit,
					SourceTool:      "git",
					Authority:       provider.AuthorityVerified,
					EvidenceSummary: fmt.Sprintf("changed file from diff (not in repo index — may be deleted): %s", relPath),
				},
			})
			continue
		}
		resp.ChangedFiles = append(resp.ChangedFiles, fileToRef(*fsFile, provider.Provenance{
			SourceKind:      provider.SourceKindGit,
			SourceTool:      "git",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("changed: %s", relPath),
			EvidencePaths:   []string{fsFile.Path},
		}))
	}

	// ── Import-graph impact analysis ──────────────────────────────────────────
	//
	// For each changed file, query FileImporters to find all files that directly
	// depend on it — those are the truly impacted files. When no language provider
	// is available, we cannot determine impact without heuristics, so we emit a
	// limitation and leave ImpactedFiles empty.

	seen := make(map[string]bool)
	for _, cf := range resp.ChangedFiles {
		seen[cf.RelPath] = true
	}

	if langProv != nil {
		for _, cf := range resp.ChangedFiles {
			if cf.Path == "" {
				continue // deleted file — no abs path available
			}

			impRes, impErr := langProv.FileImporters(ctx, cf.Path)
			if impErr != nil || impRes == nil {
				continue
			}
			resp.Limitations = append(resp.Limitations, impRes.Limitations...)

			for _, absPath := range impRes.Data {
				f, ok := listingByPath[absPath]
				if !ok || seen[f.RelPath] {
					continue
				}
				seen[f.RelPath] = true
				resp.ImpactedFiles = append(resp.ImpactedFiles, cfeatures.ImpactedFile{
					File: fileToRef(f, provider.Provenance{
						SourceKind:      provider.SourceKindSyntax,
						SourceTool:      "language-provider",
						Authority:       provider.AuthorityVerified,
						EvidenceSummary: fmt.Sprintf("%s directly imports changed file %s", f.RelPath, cf.RelPath),
						EvidencePaths:   []string{absPath},
					}),
					Reason: fmt.Sprintf("directly imports changed file %s (import graph)", cf.RelPath),
				})
			}
		}
	} else {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "no_import_graph",
			Message: "import-graph-based blast radius requires a language provider; none available",
			Scope:   "impacted_files",
		})
	}

	// ── Structural test discovery ─────────────────────────────────────────────
	//
	// For each changed file, FileTests returns the spec-backed test files for
	// that file's compilation unit. FileImporters filtered to test files
	// additionally catches test files that directly import the changed file.

	if langProv != nil {
		seenTests := make(map[string]bool)
		for _, cf := range resp.ChangedFiles {
			if cf.Path == "" {
				continue // deleted file
			}

			// FileTests: spec-backed test files for the changed file's compilation unit.
			testRes, testErr := langProv.FileTests(ctx, cf.Path)
			if testErr == nil && testRes != nil {
				resp.Limitations = append(resp.Limitations, testRes.Limitations...)
				for _, absPath := range testRes.Data {
					if seenTests[absPath] {
						continue
					}
					tf, ok := listingByPath[absPath]
					if !ok {
						continue
					}
					seenTests[absPath] = true
					prov := provider.Provenance{
						SourceKind:      provider.SourceKindSyntax,
						SourceTool:      "language-provider",
						Authority:       provider.AuthorityVerified,
						EvidenceSummary: fmt.Sprintf("test file for compilation unit containing changed file %s", cf.RelPath),
						EvidencePaths:   []string{absPath},
					}
					resp.ImpactedTests = append(resp.ImpactedTests, cfeatures.RelevantTest{
						Test: cfeatures.TestReference{
							Name:       filepath.Base(tf.RelPath),
							FilePath:   tf.Path,
							RelPath:    tf.RelPath,
							RunCommand: buildTestCommand(tf, listing),
							Provenance: prov,
						},
						Reason:     fmt.Sprintf("test file for compilation unit containing changed file %s", cf.RelPath),
						Provenance: prov,
						Confidence: 0.95,
					})
				}
			}

			// FileImporters filtered to test files: tests that directly import the changed file.
			impRes, impErr := langProv.FileImporters(ctx, cf.Path)
			if impErr == nil && impRes != nil {
				for _, absPath := range impRes.Data {
					if seenTests[absPath] {
						continue
					}
					tf, ok := listingByPath[absPath]
					if !ok || tf.Role != "test" {
						continue
					}
					seenTests[absPath] = true
					prov := provider.Provenance{
						SourceKind:      provider.SourceKindSyntax,
						SourceTool:      "language-provider",
						Authority:       provider.AuthorityVerified,
						EvidenceSummary: fmt.Sprintf("test file directly imports changed file %s", cf.RelPath),
						EvidencePaths:   []string{absPath},
					}
					resp.ImpactedTests = append(resp.ImpactedTests, cfeatures.RelevantTest{
						Test: cfeatures.TestReference{
							Name:       filepath.Base(tf.RelPath),
							FilePath:   tf.Path,
							RelPath:    tf.RelPath,
							RunCommand: buildTestCommand(tf, listing),
							Provenance: prov,
						},
						Reason:     fmt.Sprintf("test file directly imports changed file %s (import graph)", cf.RelPath),
						Provenance: prov,
						Confidence: 0.97,
					})
				}
			}
		}
	}

	// Flag generated files in the blast radius.
	for _, f := range resp.ImpactedFiles {
		if f.File.Role == "generated" {
			resp.GeneratedWarnings = append(resp.GeneratedWarnings,
				fmt.Sprintf("`%s` appears to be generated — regenerate rather than edit manually", f.File.RelPath))
		}
	}

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
