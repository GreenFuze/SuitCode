package features

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultTestsBudget = 4_000

// RunTests finds tests relevant to the given source file or change.
// langProv may be nil; when provided, test files that directly import the target
// source file are given the highest confidence score.
func RunTests(
	ctx context.Context,
	req cfeatures.TestsRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
	langProv provider.ImportGraphProvider,
) (*cfeatures.TestsResponse, error) {
	if req.FilePath == "" && req.DiffRef == "" {
		return nil, fmt.Errorf("tests: --path or --from is required")
	}

	budget := budgetOrDefault(req.Budget, defaultTestsBudget)
	runID := newRunID("tests")
	metrics, start := startMetrics(runID, "tests", req.RepoPath, budget)

	resp := &cfeatures.TestsResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
	}

	// Gather all test files in the repository.
	var allTestFiles []provider.FilesystemFile
	for _, f := range listing.Data.Files {
		if f.Role == "test" {
			allTestFiles = append(allTestFiles, f)
		}
	}

	resp.TestsConsidered = len(allTestFiles)
	resp.TargetPath = req.FilePath

	if req.FilePath == "" {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "diff_ref_not_implemented",
			Message: "diff-based test selection requires VCS provider; showing all tests",
			Scope:   "tests",
		})
		// Return all tests up to budget.
		resp.RelevantTests = allTestsUpToBudget(allTestFiles, listing, estimator, budget)
		resp.TestsSelected = len(resp.RelevantTests)
		resp.TestsExcluded = resp.TestsConsidered - resp.TestsSelected
		finishMetrics(&metrics, start, resp)
		resp.Metrics = metrics
		return resp, nil
	}

	// Find the target file.
	fsFile, err := findFile(listing, req.FilePath, req.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("tests: %w", err)
	}

	// ── Structural test discovery ─────────────────────────────────────────────
	//
	// Two structural signals, both backed by the language provider:
	//   FileTests     — spec-backed test files for the seed's compilation unit
	//                   (Go §10.3: *_test.go in the package directory).
	//   FileImporters — files that directly import the seed, filtered to those
	//                   tagged as test files in the filesystem listing.
	//
	// When no language provider is available we cannot determine structural test
	// relationships without heuristics, so we emit a limitation and return empty.

	if langProv == nil {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "no_lang_provider",
			Message: "no language provider available; structural test discovery requires an import graph",
			Scope:   req.FilePath,
		})
		resp.TestsSelected = 0
		resp.TestsExcluded = resp.TestsConsidered
		finishMetrics(&metrics, start, resp)
		resp.Metrics = metrics
		return resp, nil
	}

	type scored struct {
		file   provider.FilesystemFile
		score  float64
		reason string
	}

	seenTests := make(map[string]bool)
	var candidates []scored

	// Signal 1: FileTests — compilation-unit test files (highest fidelity).
	testRes, testErr := langProv.FileTests(ctx, fsFile.Path)
	if testErr != nil {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "file_tests_query_failed",
			Message: fmt.Sprintf("FileTests query failed for %s: %v", fsFile.RelPath, testErr),
			Scope:   fsFile.RelPath,
		})
	} else if testRes != nil {
		resp.Limitations = append(resp.Limitations, testRes.Limitations...)
		for _, absPath := range testRes.Data {
			if seenTests[absPath] {
				continue
			}
			// Resolve to filesystem file.
			var matched *provider.FilesystemFile
			for i := range allTestFiles {
				if allTestFiles[i].Path == absPath {
					matched = &allTestFiles[i]
					break
				}
			}
			if matched == nil {
				continue
			}
			seenTests[absPath] = true
			candidates = append(candidates, scored{
				file:   *matched,
				score:  0.95,
				reason: "test file for this file's compilation unit (language spec)",
			})
		}
	}

	// Signal 2: FileImporters filtered to test files — tests that directly
	// import the seed file. These are structurally guaranteed to exercise it.
	impRes, impErr := langProv.FileImporters(ctx, fsFile.Path)
	if impErr != nil {
		resp.Limitations = append(resp.Limitations, provider.Limitation{
			Kind:    "file_importers_query_failed",
			Message: fmt.Sprintf("FileImporters query failed for %s: %v", fsFile.RelPath, impErr),
			Scope:   fsFile.RelPath,
		})
	} else if impRes != nil {
		resp.Limitations = append(resp.Limitations, impRes.Limitations...)
		for _, absPath := range impRes.Data {
			if seenTests[absPath] {
				continue
			}
			// Resolve to filesystem file and check it is a test file.
			var matched *provider.FilesystemFile
			for i := range allTestFiles {
				if allTestFiles[i].Path == absPath {
					matched = &allTestFiles[i]
					break
				}
			}
			if matched == nil {
				continue // not a test file
			}
			seenTests[absPath] = true
			candidates = append(candidates, scored{
				file:   *matched,
				score:  0.97,
				reason: "test file directly imports this source file (import graph)",
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

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

		resp.RelevantTests = append(resp.RelevantTests, cfeatures.RelevantTest{
			Test: cfeatures.TestReference{
				Name:       filepath.Base(c.file.RelPath),
				FilePath:   c.file.Path,
				RelPath:    c.file.RelPath,
				RunCommand: buildTestCommand(c.file, listing),
				Framework:  detectFramework(listing),
				Provenance: prov,
			},
			Reason:     c.reason,
			Provenance: prov,
			Confidence: c.score,
		})
	}

	resp.TestsSelected = len(resp.RelevantTests)
	resp.TestsExcluded = resp.TestsConsidered - resp.TestsSelected

	allTokens := 0
	for _, tf := range allTestFiles {
		est, _ := estimator.EstimateFile(tf.Path)
		allTokens += est.Tokens
	}
	resp.EstimatedContextAvoided = provider.TokenEstimate{
		Tokens:     allTokens - tokenUsed,
		Method:     "heuristic_chars_div4",
		IsEstimate: true,
	}

	metrics.Budget.Used = tokenUsed
	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

func allTestsUpToBudget(
	testFiles []provider.FilesystemFile,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
	budget int,
) []cfeatures.RelevantTest {
	var out []cfeatures.RelevantTest
	tokenUsed := 0

	for _, tf := range testFiles {
		est, _ := estimator.EstimateFile(tf.Path)
		if budget > 0 && tokenUsed+est.Tokens > budget {
			break
		}
		tokenUsed += est.Tokens

		prov := fsProv("all test files in repository", tf.Path)
		out = append(out, cfeatures.RelevantTest{
			Test: cfeatures.TestReference{
				Name:       filepath.Base(tf.RelPath),
				FilePath:   tf.Path,
				RelPath:    tf.RelPath,
				RunCommand: buildTestCommand(tf, listing),
				Framework:  detectFramework(listing),
				Provenance: prov,
			},
			Reason:     "all test files (no target specified)",
			Provenance: prov,
			Confidence: 0.5,
		})
	}

	return out
}
