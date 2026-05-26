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
	"os"
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

	// Give a clear, actionable error when the caller passed a directory.
	if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
		var suggestions []string
		for _, f := range listing.Data.Files {
			if filepath.Dir(f.Path) == abs && len(suggestions) < 5 {
				suggestions = append(suggestions, f.RelPath)
			}
		}
		msg := fmt.Sprintf("%q is a directory, not a file — specify a file path", targetPath)
		if len(suggestions) > 0 {
			msg += fmt.Sprintf("; files in this directory: %s", strings.Join(suggestions, ", "))
		}
		return nil, fmt.Errorf("%s", msg)
	}

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
