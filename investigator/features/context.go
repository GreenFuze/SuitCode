package features

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultContextBudget = 8_000

// Scoring constants for candidate selection. Later constants in the list are
// only reached if no earlier rule matched (goto scored enforces exclusivity).
const (
	scoreImportedBy = 0.90 // file is in a package directly imported by a seed's package
	scoreImporterOf = 0.80 // file is in a package that directly imports a seed's package
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

	seedDirs := make(map[string]bool)
	for _, s := range seedRelPaths {
		seedDirs[filepath.ToSlash(filepath.Dir(s))] = true
	}

	// ── Import-graph enrichment (optional) ────────────────────────────────────
	//
	// When a language provider is available, pre-compute the set of absolute
	// file paths that are in packages directly imported by any seed (forward)
	// and in packages that directly import any seed (reverse). These sets feed
	// the 0.90/0.80 scoring rules below.

	importedAbsPaths := make(map[string]bool)
	importerAbsPaths := make(map[string]bool)
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
		}
	}

	var candidates []candidate
	seenCandidates := make(map[string]bool)

	for _, f := range listing.Data.Files {
		if seenCandidates[f.RelPath] {
			continue
		}

		var score float64
		var reason string

		if seedSet[f.RelPath] {
			score = 1.0
			reason = "seed file (explicitly requested)"
		} else {
			dir := filepath.ToSlash(filepath.Dir(f.RelPath))

			// File is in a package directly imported by a seed's package (0.90).
			// Checked before test-file heuristic since 0.90 > 0.85.
			if importedAbsPaths[f.Path] {
				score = scoreImportedBy
				reason = "file is in a package directly imported by a seed"
				goto scored
			}

			// Test files for seeds (0.85).
			for _, s := range seedRelPaths {
				tfs := testFilesForSource(listing, s)
				for _, tf := range tfs {
					if tf.RelPath == f.RelPath {
						score = 0.85
						reason = fmt.Sprintf("test file for seed %s", s)
						goto scored
					}
				}
			}

			// File's package directly imports a seed's package (0.80).
			// Checked AFTER test-file rule (0.85 > 0.80) so a test file that
			// also imports the seed still gets the higher test-file score.
			if importerAbsPaths[f.Path] {
				score = scoreImporterOf
				reason = "file is in a package that directly imports a seed's package"
				goto scored
			}

			// Same directory as a seed.
			if seedDirs[dir] {
				score = 0.70
				reason = fmt.Sprintf("same directory as seed (%s)", dir)
				goto scored
			}

			// Similar stem to a seed file.
			fStem := strings.ToLower(strings.TrimSuffix(filepath.Base(f.RelPath),
				filepath.Ext(f.RelPath)))
			for _, s := range seedRelPaths {
				sStem := strings.ToLower(strings.TrimSuffix(filepath.Base(s),
					filepath.Ext(s)))
				if fStem == sStem && f.Language != "" {
					score = 0.40
					reason = fmt.Sprintf("similar name to seed %s", s)
					goto scored
				}
			}
		}

	scored:
		if score == 0 {
			continue // not a candidate
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

	// ── Budget selection ───────────────────────────────────────────────────────

	totalCandidateTokens := 0
	for _, c := range candidates {
		totalCandidateTokens += c.est.Tokens
	}

	tokenUsed := 0
	capsule := cfeatures.ContextCapsule{
		BudgetRequested: budget,
	}

	for rank, c := range candidates {
		if tokenUsed+c.est.Tokens > budget {
			// Record as rejection.
			capsule.Rejections = append(capsule.Rejections, cfeatures.ContextRejection{
				Candidate: cfeatures.ContextCandidate{
					File:          fileToRef(c.file, fsProv(c.reason, c.file.Path)),
					Score:         c.score,
					ScoreReasons:  []string{c.reason},
					TokenEstimate: c.est,
				},
				Reason: fmt.Sprintf("budget exhausted (%d/%d tokens used)", tokenUsed, budget),
			})
			continue
		}

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

		tokenUsed += c.est.Tokens

		prov := fsProv(c.reason, c.file.Path)

		capsule.Selections = append(capsule.Selections, cfeatures.ContextSelection{
			Candidate: cfeatures.ContextCandidate{
				File:          fileToRef(c.file, prov),
				Score:         c.score,
				ScoreReasons:  []string{c.reason},
				TokenEstimate: c.est,
			},
			Rank:   rank + 1,
			Reason: c.reason,
		})

		capsule.Facts = append(capsule.Facts, cfeatures.ContextFact{
			Kind:    "file_content",
			Content: string(content),
			Source:  fileToRef(c.file, prov),
			Provenance: prov,
			TokenEstimate: c.est,
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
