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

// RunContext is the ContextCompiler: it gathers candidates, scores and ranks
// them, selects within budget, and returns a bounded ContextCapsule.
func RunContext(
	_ context.Context,
	req cfeatures.ContextRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
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

			// Test files for seeds.
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

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

func roundTo2(f float64) float64 {
	return float64(int(f*100)) / 100
}
