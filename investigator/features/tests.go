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

const defaultTestsBudget = 4_000

// RunTests finds tests relevant to the given source file or change.
func RunTests(
	_ context.Context,
	req cfeatures.TestsRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
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

	// Score each test file by proximity to the source file.
	type scored struct {
		file  provider.FilesystemFile
		score float64
		reason string
	}

	var candidates []scored

	targetDir := filepath.ToSlash(filepath.Dir(fsFile.RelPath))
	targetStem := strings.ToLower(strings.TrimSuffix(filepath.Base(fsFile.RelPath),
		filepath.Ext(fsFile.RelPath)))

	for _, tf := range allTestFiles {
		testDir := filepath.ToSlash(filepath.Dir(tf.RelPath))
		testBase := strings.ToLower(filepath.Base(tf.RelPath))
		testStem := strings.TrimSuffix(testBase, filepath.Ext(testBase))
		testStem = strings.TrimSuffix(testStem, "_test")
		testStem = strings.TrimPrefix(testStem, "test_")

		var score float64
		var reason string

		// Direct name match (e.g. foo_test.go for foo.go).
		if testStem == targetStem {
			score = 0.95
			reason = "test file name directly matches source file"
		} else if testDir == targetDir {
			score = 0.70
			reason = "test file is in the same directory as source"
		} else if strings.HasPrefix(testDir, targetDir) {
			score = 0.50
			reason = "test file is in a subdirectory of the source's directory"
		} else if strings.Contains(testStem, targetStem) || strings.Contains(targetStem, testStem) {
			score = 0.35
			reason = fmt.Sprintf("name similarity between %q and %q", testStem, targetStem)
		}

		if score > 0 {
			candidates = append(candidates, scored{tf, score, reason})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	tokenUsed := 0
	for _, c := range candidates {
		est, _ := estimator.EstimateFile(c.file.Path)
		if budget > 0 && tokenUsed+est.Tokens > budget {
			break
		}
		tokenUsed += est.Tokens

		prov := heuristicProv("proximity_scoring",
			fmt.Sprintf("score %.2f for %s", c.score, c.file.RelPath), c.file.Path)

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
