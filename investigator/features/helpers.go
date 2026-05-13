// Package features contains the service implementations for all SuitCode
// investigator features. Each function takes pre-fetched provider results and
// returns a fully-populated, metrics-bearing response.
//
// No provider calls are made here — all I/O is done by the ProjectInvestigator
// before delegating. This keeps the feature logic pure and testable.
package features

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

// ──────────────────────────────────────────────────────────────────────────────
// Run-ID generation
// ──────────────────────────────────────────────────────────────────────────────

// newRunID generates a unique run identifier from the current time plus a
// short hash of the feature name. No external UUID library required.
func newRunID(feature string) cfeatures.RunID {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d-%s", time.Now().UnixNano(), feature)))
	return cfeatures.RunID(fmt.Sprintf("%s-%x", feature, h[:4]))
}

// ──────────────────────────────────────────────────────────────────────────────
// Metrics helpers
// ──────────────────────────────────────────────────────────────────────────────

// startMetrics initialises a FeatureMetrics for a new run.
func startMetrics(runID cfeatures.RunID, feature, repoPath string, budget int) (cfeatures.FeatureMetrics, time.Time) {
	now := time.Now()
	m := cfeatures.FeatureMetrics{
		RunID:    runID,
		Feature:  feature,
		RepoPath: repoPath,
		Timing: cfeatures.TimingMetrics{
			StartedAt:  now,
			ProviderMs: make(map[string]int64),
		},
		Budget: cfeatures.BudgetMetrics{
			Requested: budget,
		},
	}
	return m, now
}

// finishMetrics records the end time, duration, deterministic hash, and
// compliance ratio.
func finishMetrics(m *cfeatures.FeatureMetrics, start time.Time, response any) {
	m.Timing.FinishedAt = time.Now()
	m.Timing.DurationMs = time.Since(start).Milliseconds()

	if m.Budget.Requested > 0 && m.Budget.Used > 0 {
		m.Budget.Compliance = float64(m.Budget.Used) / float64(m.Budget.Requested)
	}

	// Compute a deterministic hash of the response's *content* fields only.
	// Volatile metadata (RunID, Timing, Metrics, Trace) is excluded so the
	// hash is stable across identical runs regardless of wall-clock time.
	if response != nil {
		m.DeterministicHash = contentHash(response)
	}
}

// contentHash returns the SHA-256 (hex) of the response after stripping the
// run-specific fields that change on every invocation: RunID, Metrics, Trace.
// This makes the hash a reliable signal for output determinism.
func contentHash(response any) string {
	data, err := json.Marshal(response)
	if err != nil {
		return ""
	}

	// Parse into a generic map so we can surgically remove volatile keys.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}

	// Remove all fields whose values change between identical runs.
	delete(raw, "RunID")   // time-based in BaseFeatureResponse
	delete(raw, "Metrics") // contains RunID + Timing
	delete(raw, "Trace")   // contains timestamps

	clean, err := json.Marshal(raw)
	if err != nil {
		return ""
	}

	h := sha256.Sum256(clean)
	return fmt.Sprintf("%x", h)
}

// computeContextReduction populates the ContextReductionMetrics fields.
func computeContextReduction(
	m *cfeatures.FeatureMetrics,
	evidenceScanned, capsuleTokens int,
	filesConsidered, filesIncluded, filesExcluded int,
) {
	m.Candidates = cfeatures.CandidateSelectionMetrics{
		Considered: filesConsidered,
		Included:   filesIncluded,
		Excluded:   filesExcluded,
	}

	avoided := evidenceScanned - capsuleTokens
	if avoided < 0 {
		avoided = 0
	}

	ratio := 0.0
	if evidenceScanned > 0 {
		ratio = math.Round(float64(capsuleTokens)/float64(evidenceScanned)*100) / 100
	}

	m.ContextReduction = cfeatures.ContextReductionMetrics{
		EvidenceScannedTokens:   evidenceScanned,
		CapsuleTokens:           capsuleTokens,
		EstimatedContextAvoided: avoided,
		CompressionRatio:        ratio,
		FilesConsidered:         filesConsidered,
		FilesIncluded:           filesIncluded,
		FilesExcluded:           filesExcluded,
	}
}

// budgetOrDefault returns the request budget, falling back to defaultBudget if
// the request has no budget set.
func budgetOrDefault(b, defaultBudget int) int {
	if b > 0 {
		return b
	}
	return defaultBudget
}

// ──────────────────────────────────────────────────────────────────────────────
// File-listing helpers
// ──────────────────────────────────────────────────────────────────────────────

// findFile locates a FileReference in the listing by absolute or relative path.
func findFile(listing *provider.ProviderResult[provider.FilesystemListing], targetPath, repoPath string) (*provider.FilesystemFile, error) {
	// Normalise the target path.
	abs := targetPath
	if !filepath.IsAbs(targetPath) {
		abs = filepath.Join(repoPath, targetPath)
	}
	rel, _ := filepath.Rel(repoPath, abs)
	rel = filepath.ToSlash(rel)

	for i, f := range listing.Data.Files {
		if f.Path == abs || f.RelPath == rel {
			return &listing.Data.Files[i], nil
		}
	}
	return nil, fmt.Errorf("file not found in repository index: %q", targetPath)
}

// fileToRef converts a FilesystemFile to a provider.FileReference.
func fileToRef(f provider.FilesystemFile, prov provider.Provenance) provider.FileReference {
	return provider.FileReference{
		Path:       f.Path,
		RelPath:    f.RelPath,
		Language:   f.Language,
		Role:       f.Role,
		Provenance: prov,
	}
}

// filesInSameDir returns all files in the same directory as relPath.
func filesInSameDir(listing *provider.ProviderResult[provider.FilesystemListing], relPath string) []provider.FilesystemFile {
	dir := filepath.ToSlash(filepath.Dir(relPath))
	if dir == "." {
		dir = ""
	}

	var out []provider.FilesystemFile
	for _, f := range listing.Data.Files {
		if f.RelPath == relPath {
			continue // exclude the file itself
		}
		fd := filepath.ToSlash(filepath.Dir(f.RelPath))
		if fd == "." {
			fd = ""
		}
		if fd == dir {
			out = append(out, f)
		}
	}
	return out
}

// testFilesForSource returns test files that correspond to the given source
// file using naming conventions.
func testFilesForSource(listing *provider.ProviderResult[provider.FilesystemListing], relPath string) []provider.FilesystemFile {
	base := filepath.Base(relPath)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	dir := filepath.ToSlash(filepath.Dir(relPath))

	// Patterns: stem_test.go, test_stem.go, stem.test.ts, stem.spec.ts, etc.
	testPatterns := []string{
		stem + "_test" + ext,                         // Go convention
		"test_" + stem + ext,                         // Python convention
		stem + ".test" + ext,                         // JS/TS convention
		stem + ".spec" + ext,                         // JS/TS convention
		stem + ".test" + strings.TrimSuffix(ext, filepath.Ext(ext)), // for .ts -> .test.ts
	}

	var out []provider.FilesystemFile
	for _, f := range listing.Data.Files {
		if f.Role != "test" {
			continue
		}
		fBase := filepath.Base(f.RelPath)
		fDir := filepath.ToSlash(filepath.Dir(f.RelPath))

		// Same directory or subdirectory.
		if !strings.HasPrefix(fDir, dir) && fDir != dir {
			continue
		}

		for _, pat := range testPatterns {
			if strings.EqualFold(fBase, pat) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// isTestFile reports whether a file path matches common test naming patterns.
func isTestFile(relPath string) bool {
	base := strings.ToLower(filepath.Base(relPath))
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasPrefix(base, "test_") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.")
}

// provenance builds a standard heuristic Provenance record.
func heuristicProv(tool, summary string, paths ...string) provider.Provenance {
	return provider.Provenance{
		SourceKind:      provider.SourceKindHeuristic,
		SourceTool:      tool,
		Authority:       provider.AuthorityHeuristic,
		EvidenceSummary: summary,
		EvidencePaths:   paths,
	}
}

// fsProv builds a filesystem-sourced Provenance record.
func fsProv(summary string, paths ...string) provider.Provenance {
	return provider.Provenance{
		SourceKind:      provider.SourceKindFilesystem,
		SourceTool:      "filepath.WalkDir",
		Authority:       provider.AuthorityVerified,
		EvidenceSummary: summary,
		EvidencePaths:   paths,
	}
}
