package features

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultContextBudget = 8_000

// Scoring constants for context candidate ranking. All scored files are
// included — scores only determine display order (highest-confidence first).
// No file is excluded solely because of its score.
const (
	scoreImportedBy = 0.90 // forward import: seed imports this package
	scoreImporterOf = 0.80 // reverse import: this package imports the seed
	scorePeer       = 0.75 // package peer: same compilation unit as seed
	scoreTest       = 0.70 // package test: test files for the seed's package
)

// RunContext is the ContextCompiler: it gathers candidates, scores and ranks
// them, selects within budget, and returns a bounded ContextCapsule.
// langProv may be nil — when provided it enriches scoring with import-graph
// signals; otherwise the function falls back to heuristic-only scoring.
func RunContext(
	ctx context.Context,
	req cfeatures.ContextRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
	langProv provider.ImportGraphProvider,
) (*cfeatures.ContextResponse, error) {
	if len(req.Files) == 0 && req.DiffRef == "" {
		return nil, fmt.Errorf("context: --files or --from is required")
	}

	budget := budgetOrDefault(req.Budget, defaultContextBudget)
	runID := newRunID("context")
	metrics, start := startMetrics(runID, "context", req.RepoPath, budget)

	resp := &cfeatures.ContextResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
	}

	// Resolve seed files.
	var seedRelPaths []string
	for _, f := range req.Files {
		fsFile, err := findFile(listing, f, req.RepoPath)
		if err != nil {
			resp.Limitations = append(resp.Limitations, provider.Limitation{
				Kind:    "seed_file_not_found",
				Message: fmt.Sprintf("seed file not found in index: %q", f),
				Scope:   f,
			})
			continue
		}
		seedRelPaths = append(seedRelPaths, fsFile.RelPath)
	}

	if len(seedRelPaths) == 0 {
		return nil, fmt.Errorf("context: none of the specified files were found in the repository index")
	}

	// ── Candidate scoring ──────────────────────────────────────────────────────
	//
	// Score every file in the repository as a potential capsule candidate.
	// Seeds score 1.0, same-directory files score 0.7, test files for seeds
	// score 0.8, similar-name files 0.4. Everything else is omitted.

	type candidate struct {
		file   provider.FilesystemFile
		score  float64
		reason string
		est    provider.TokenEstimate
	}

	seedSet := make(map[string]bool, len(seedRelPaths))
	for _, s := range seedRelPaths {
		seedSet[s] = true
	}

	// ── Import-graph enrichment ───────────────────────────────────────────────
	//
	// When a language provider is available, query all four structural
	// relationships for every seed:
	//   importedAbsPaths — files in packages the seed directly imports (0.90)
	//   importerAbsPaths — files in packages that directly import the seed (0.80)
	//   peerAbsPaths     — other files in the same compilation unit (0.75)
	//   testAbsPaths     — test files for the seed's package (0.70)
	//
	// All four sets are language-provider-backed — no naming heuristics.
	// Files not covered by any of these sets are not included in the capsule.

	importedAbsPaths := make(map[string]bool)
	importerAbsPaths := make(map[string]bool)
	peerAbsPaths := make(map[string]bool)
	testAbsPaths := make(map[string]bool)
	importEdgesScanned := 0
	lspEnhanced := false

	if langProv != nil {
		for _, seedRel := range seedRelPaths {
			seedAbs := filepath.Join(req.RepoPath, filepath.FromSlash(seedRel))

			// Forward: packages directly imported by the seed's package.
			if res, err := langProv.FileImports(ctx, seedAbs); err == nil {
				for _, p := range res.Data {
					importedAbsPaths[p] = true
					importEdgesScanned++
				}
				if len(res.Data) > 0 {
					lspEnhanced = true
				}
			}

			// Reverse: packages that directly import the seed's package.
			if res, err := langProv.FileImporters(ctx, seedAbs); err == nil {
				for _, p := range res.Data {
					importerAbsPaths[p] = true
					importEdgesScanned++
				}
				if len(res.Data) > 0 {
					lspEnhanced = true
				}
			}

			// Peers: other files in the same compilation unit (same Go package,
			// same C# project). Language-spec fact, not a directory heuristic.
			if res, err := langProv.FilePeers(ctx, seedAbs); err == nil {
				for _, p := range res.Data {
					peerAbsPaths[p] = true
					importEdgesScanned++
				}
				if len(res.Data) > 0 {
					lspEnhanced = true
				}
			}

			// Tests: test files for the seed's package. For Go this is the set
			// of *_test.go files in the package directory (Go spec §10.3).
			if res, err := langProv.FileTests(ctx, seedAbs); err == nil {
				for _, p := range res.Data {
					testAbsPaths[p] = true
					importEdgesScanned++
				}
				if len(res.Data) > 0 {
					lspEnhanced = true
				}
			}
		}
	}

	// ── Candidate selection ───────────────────────────────────────────────────
	//
	// A file is a candidate if and only if it falls into one of these categories:
	//   1.0  seed — explicitly requested
	//   0.90 imported-by — seed's package imports this file's package
	//   0.80 importer-of — this file's package imports the seed's package
	//   0.75 peer — same compilation unit as seed (same Go package / C# project)
	//   0.70 test — test file for the seed's package
	//
	// There are NO heuristics. Files not covered by any of the above are
	// not included. All included files are returned — no budget trimming.

	var candidates []candidate
	seenCandidates := make(map[string]bool)

	for _, f := range listing.Data.Files {
		if seenCandidates[f.RelPath] {
			continue
		}

		var score float64
		var reason string

		switch {
		case seedSet[f.RelPath]:
			score = 1.0
			reason = "seed file (explicitly requested)"

		case importedAbsPaths[f.Path]:
			score = scoreImportedBy
			reason = "file is in a package directly imported by a seed"

		case importerAbsPaths[f.Path]:
			score = scoreImporterOf
			reason = "file is in a package that directly imports a seed"

		case peerAbsPaths[f.Path]:
			score = scorePeer
			reason = "file is in the same compilation unit as a seed"

		case testAbsPaths[f.Path]:
			score = scoreTest
			reason = "test file for the seed's package"

		default:
			continue // not structurally related — excluded
		}

		est, _ := estimator.EstimateFile(f.Path)
		seenCandidates[f.RelPath] = true
		candidates = append(candidates, candidate{f, score, reason, est})
	}

	// Sort candidates by score descending, then by path for determinism.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].file.RelPath < candidates[j].file.RelPath
	})

	// ── Include all candidates ────────────────────────────────────────────────
	//
	// Every candidate has a structural justification from the language provider —
	// no heuristics remain. All candidates are included in the capsule.
	// The --budget value is used only for reporting (how much over/under budget
	// the result is) — nothing is dropped because of it.
	//
	// If the total exceeds the requested budget the caller receives a
	// "over_budget" limitation so it can decide whether to raise the budget,
	// narrow the seed set, or proceed with the full context.

	totalCandidateTokens := 0
	for _, c := range candidates {
		totalCandidateTokens += c.est.Tokens
	}

	tokenUsed := 0
	capsule := cfeatures.ContextCapsule{
		BudgetRequested: budget,
	}

	rank := 0
	for _, c := range candidates {
		// Read the file content for the capsule.
		content, err := os.ReadFile(c.file.Path)
		if err != nil {
			capsule.Rejections = append(capsule.Rejections, cfeatures.ContextRejection{
				Candidate: cfeatures.ContextCandidate{
					File:  fileToRef(c.file, fsProv(c.reason, c.file.Path)),
					Score: c.score,
				},
				Reason: fmt.Sprintf("could not read file: %v", err),
			})
			continue
		}

		rank++
		tokenUsed += c.est.Tokens
		prov := fsProv(c.reason, c.file.Path)

		capsule.Selections = append(capsule.Selections, cfeatures.ContextSelection{
			Candidate: cfeatures.ContextCandidate{
				File:          fileToRef(c.file, prov),
				Score:         c.score,
				ScoreReasons:  []string{c.reason},
				TokenEstimate: c.est,
			},
			Rank:   rank,
			Reason: c.reason,
		})

		capsule.Facts = append(capsule.Facts, cfeatures.ContextFact{
			Kind:          "file_content",
			Content:       string(content),
			Source:        fileToRef(c.file, prov),
			Provenance:    prov,
			TokenEstimate: c.est,
		})
	}

	// Report when the structurally-justified context exceeds the requested budget.
	// This is informational — nothing was dropped; the caller may choose to
	// narrow the seed set or raise the budget.
	if tokenUsed > budget {
		overage := tokenUsed - budget
		overagePct := int(float64(overage) / float64(budget) * 100)
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind: "over_budget",
			Message: fmt.Sprintf(
				"structurally-related context is %d%% over requested budget: %d tokens (%d requested); all %d related files included — narrow seed set or raise --budget to reduce",
				overagePct, tokenUsed, budget, rank,
			),
		})
	}

	capsule.BudgetUsed = tokenUsed
	capsule.TotalEstimate = provider.TokenEstimate{
		Tokens:     tokenUsed,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}

	if totalCandidateTokens > 0 {
		capsule.CompressionRatio = roundTo2(float64(tokenUsed) / float64(totalCandidateTokens))
	}

	resp.Capsule = capsule
	resp.FilesConsidered = len(candidates)
	resp.FilesIncluded = len(capsule.Selections)
	resp.FilesExcluded = len(capsule.Rejections)

	// Populate IncludedRelPaths for eval golden-files checks.
	for _, sel := range capsule.Selections {
		resp.IncludedRelPaths = append(resp.IncludedRelPaths, sel.Candidate.File.RelPath)
	}

	// Populate flat Files[] for agents — one entry per selected file, with
	// content included, ordered by rank. No Capsule.Facts traversal needed.
	for i, fact := range capsule.Facts {
		sel := capsule.Selections[i]
		resp.Files = append(resp.Files, cfeatures.ContextFileEntry{
			Path:          fact.Source.Path,
			RelPath:       fact.Source.RelPath,
			Language:      fact.Source.Language,
			Role:          fact.Source.Role,
			TokenEstimate: fact.TokenEstimate.Tokens,
			Rank:          sel.Rank,
			Score:         sel.Candidate.Score,
			Reason:        sel.Reason,
			Content:       fact.Content,
		})
	}

	resp.EvidenceScanned = provider.TokenEstimate{
		Tokens:     totalCandidateTokens,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}
	avoided := totalCandidateTokens - tokenUsed
	if avoided < 0 {
		avoided = 0
	}
	resp.EstimatedContextAvoided = provider.TokenEstimate{
		Tokens:     avoided,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}
	resp.CompressionRatio = capsule.CompressionRatio

	metrics.Budget.Used = tokenUsed
	computeContextReduction(&metrics, totalCandidateTokens, tokenUsed,
		resp.FilesConsidered, resp.FilesIncluded, resp.FilesExcluded)

	// Propagate import-graph signals into metrics.
	metrics.ContextReduction.LspEnhanced = lspEnhanced
	metrics.ContextReduction.ImportEdgesScanned = importEdgesScanned

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

func roundTo2(f float64) float64 {
	return float64(int(f*100)) / 100
}
