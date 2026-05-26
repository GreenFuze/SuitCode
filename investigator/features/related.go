package features

import (
	"context"
	"fmt"
	"sort"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultRelatedBudget = 4_000

// RunRelated finds files related to the target using structural signals from the
// language provider. langProv may be nil; when provided, import graph edges
// (FileImports/FileImporters), compilation-unit peers (FilePeers), and
// spec-backed test files (FileTests) are all used. No filesystem heuristics.
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

	// ── Structural relationship discovery ────────────────────────────────────
	//
	// Four structural signals, all backed by the language provider. No directory-
	// proximity or naming heuristics are used.
	//
	//   FileImports   (0.92) — files in packages directly imported by this file
	//   FileImporters (0.88) — files in packages that directly import this file
	//   FileTests     (0.90) — test files for this file's compilation unit
	//   FilePeers     (0.75) — other files in the same compilation unit

	seen := make(map[string]bool)
	seen[fsFile.RelPath] = true // exclude the seed itself

	var candidates []candidate

	// Build a path→file index for O(1) lookups.
	byPath := make(map[string]provider.FilesystemFile, len(listing.Data.Files))
	for _, f := range listing.Data.Files {
		byPath[f.Path] = f
	}

	if langProv != nil {
		seedAbs := fsFile.Path

		// Forward: packages directly imported by this file.
		if res, err := langProv.FileImports(ctx, seedAbs); err == nil && res != nil {
			resp.Limitations = append(resp.Limitations, res.Limitations...)
			for _, absPath := range res.Data {
				f, ok := byPath[absPath]
				if !ok || seen[f.RelPath] {
					continue
				}
				seen[f.RelPath] = true
				candidates = append(candidates, candidate{
					f,
					cfeatures.RelationImports,
					"directly imported by this file (import graph)",
					0.92,
				})
			}
		}

		// Reverse: packages that directly import this file.
		if res, err := langProv.FileImporters(ctx, seedAbs); err == nil && res != nil {
			resp.Limitations = append(resp.Limitations, res.Limitations...)
			for _, absPath := range res.Data {
				f, ok := byPath[absPath]
				if !ok || seen[f.RelPath] {
					continue
				}
				seen[f.RelPath] = true
				candidates = append(candidates, candidate{
					f,
					cfeatures.RelationImportedBy,
					"directly imports this file (import graph)",
					0.88,
				})
			}
		}

		// Test files: spec-backed test files for this file's compilation unit.
		if res, err := langProv.FileTests(ctx, seedAbs); err == nil && res != nil {
			resp.Limitations = append(resp.Limitations, res.Limitations...)
			for _, absPath := range res.Data {
				f, ok := byPath[absPath]
				if !ok || seen[f.RelPath] {
					continue
				}
				seen[f.RelPath] = true
				rel := cfeatures.RelationTestedBy
				reason := "test file for this file's compilation unit (language spec)"
				if isTestFile(fsFile.RelPath) {
					rel = cfeatures.RelationTest
					reason = "this is a test file for the compilation unit (language spec)"
				}
				candidates = append(candidates, candidate{f, rel, reason, 0.90})
			}
		}

		// Peers: other files in the same compilation unit (same Go package,
		// same C# project). Manifest or language-spec fact — not directory proximity.
		if res, err := langProv.FilePeers(ctx, seedAbs); err == nil && res != nil {
			resp.Limitations = append(resp.Limitations, res.Limitations...)
			for _, absPath := range res.Data {
				f, ok := byPath[absPath]
				if !ok || seen[f.RelPath] {
					continue
				}
				seen[f.RelPath] = true
				candidates = append(candidates, candidate{
					f,
					cfeatures.RelationSamePackage,
					"same compilation unit (language provider)",
					0.75,
				})
			}
		}
	} else {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "no_lang_provider",
			Message: "no language provider available; structural relationship discovery requires an import graph",
			Scope:   req.FilePath,
		})
	}

	// Sort by score descending, path for determinism.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].file.RelPath < candidates[j].file.RelPath
	})

	// Include all structurally justified candidates — no budget trimming.
	// Budget is advisory: emit over_budget limitation if exceeded.
	tokenUsed := 0
	for _, c := range candidates {
		est, _ := estimator.EstimateFile(c.file.Path)
		tokenUsed += est.Tokens

		prov := provider.Provenance{
			SourceKind:      provider.SourceKindSyntax,
			SourceTool:      "language-provider",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: c.reason,
			EvidencePaths:   []string{c.file.Path},
		}

		resp.RelatedFiles = append(resp.RelatedFiles, cfeatures.RelatedFile{
			File:       fileToRef(c.file, prov),
			Relation:   c.relation,
			Reason:     c.reason,
			Provenance: prov,
			Confidence: c.score,
		})
	}

	if tokenUsed > budget {
		overage := tokenUsed - budget
		overagePct := int(float64(overage) / float64(budget) * 100)
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind: "over_budget",
			Message: fmt.Sprintf(
				"structurally-related files are %d%% over requested budget: %d tokens (%d requested); all %d related files included",
				overagePct, tokenUsed, budget, len(candidates),
			),
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
