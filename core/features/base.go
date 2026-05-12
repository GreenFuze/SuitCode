// Package features defines the typed request/response contracts for every
// SuitCode feature, plus the shared metrics and trace types that appear in
// every response. It is a shared vocabulary layer; no business logic lives
// here.
package features

import (
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

// OutputFormat controls how a feature response is rendered by the CLI or
// server.
type OutputFormat string

const (
	FormatMarkdown OutputFormat = "markdown"
	FormatJSON     OutputFormat = "json"
)

// RunID is a unique identifier for a single feature invocation.
type RunID string

// BaseFeatureRequest carries fields common to every feature request.
type BaseFeatureRequest struct {
	// RepoPath is the absolute path to the repository root.
	RepoPath string
	// Budget is the maximum estimated token count the response may use.
	// Zero means no budget limit.
	Budget int
	// Format selects the rendering format for the CLI output.
	Format OutputFormat
}

// BaseFeatureResponse carries fields present in every feature response.
// Feature-specific responses embed this struct.
type BaseFeatureResponse struct {
	RunID RunID
	// IsPartial is true when the response is incomplete due to a limitation
	// (e.g. a provider is not yet implemented). Never silently pretend a
	// partial result is complete.
	IsPartial bool
	// Limitations lists anything the investigator could not determine.
	Limitations []provider.Limitation
	Metrics     FeatureMetrics
	Trace       FeatureTrace
}

// ──────────────────────────────────────────────────────────────────────────────
// Shared evidence types used across multiple features
// ──────────────────────────────────────────────────────────────────────────────

// TestReference identifies a single test case and how to run it.
type TestReference struct {
	Name        string
	FilePath    string
	RelPath     string
	RunCommand  string // e.g. "go test ./internal/foo/ -run TestBar"
	Framework   string // e.g. "Go test", "pytest"
	Provenance  provider.Provenance
}

// BuildTargetReference identifies a build target.
type BuildTargetReference struct {
	Name       string
	Kind       string // e.g. "binary", "library", "container"
	FilePath   string
	Command    string
	Provenance provider.Provenance
}

// DirectoryEntry describes one entry in a directory listing used by the
// repo-overview response.
type DirectoryEntry struct {
	RelPath  string
	IsDir    bool
	Notes    string // e.g. "main package", "generated"
	Language string
}

// DependencyPath records one path through the import/dependency graph.
type DependencyPath struct {
	From  string
	To    string
	Chain []string
}

// RiskyBoundary flags an interface boundary that change impact analysis
// should highlight.
type RiskyBoundary struct {
	Description string
	FilePath    string
	Reason      string
	Provenance  provider.Provenance
}

// ──────────────────────────────────────────────────────────────────────────────
// Metrics types
// ──────────────────────────────────────────────────────────────────────────────

// TimingMetrics records when a feature run started and finished.
type TimingMetrics struct {
	StartedAt   time.Time
	FinishedAt  time.Time
	DurationMs  int64
	// ProviderMs maps ProviderID to its wall-clock contribution.
	ProviderMs map[string]int64
}

// CacheMetrics describes cache behaviour for this run.
type CacheMetrics struct {
	Hit      bool
	Key      string
	StoredAt *time.Time
	// WarmCold is "warm" if a cached index was used, "cold" otherwise.
	WarmCold string
}

// BudgetMetrics tracks requested vs used token budget.
type BudgetMetrics struct {
	Requested  int
	Used       int
	// Compliance is Used/Requested (0–1). A value > 1 means the budget was
	// exceeded (this should not happen; the context compiler enforces the limit).
	Compliance float64
}

// CandidateSelectionMetrics records how many candidates were considered,
// included, and excluded during a selection step.
type CandidateSelectionMetrics struct {
	Considered int
	Included   int
	Excluded   int
}

// ContextReductionMetrics is the primary evaluation signal. Every feature
// that produces or filters evidence should populate this.
type ContextReductionMetrics struct {
	// EvidenceScannedTokens is the token-equivalent of everything examined.
	EvidenceScannedTokens int
	// CapsuleTokens is the estimated tokens in the final output.
	CapsuleTokens int
	// EstimatedContextAvoided = EvidenceScannedTokens - CapsuleTokens.
	// Labelled "estimated" because both figures are approximations.
	EstimatedContextAvoided int
	// CompressionRatio = CapsuleTokens / EvidenceScannedTokens.
	// Closer to 0 means more was filtered out.
	CompressionRatio float64
	// File-level selection counts.
	FilesConsidered int
	FilesIncluded   int
	FilesExcluded   int

	// LspEnhanced is true when at least one import-graph signal (from a
	// LanguageProvider) contributed to candidate scoring in this run.
	LspEnhanced bool
	// ImportEdgesScanned is the total count of import edges (forward +
	// reverse) examined across all seed files.
	ImportEdgesScanned int
}

// ProviderMetrics records per-provider contribution to a feature run.
type ProviderMetrics struct {
	ProviderID  string
	DisplayName string
	DurationMs  int64
	CacheHit    bool
	Error       string
	Limitations []provider.Limitation
}

// FeatureMetrics aggregates all measurement data for one feature invocation.
type FeatureMetrics struct {
	RunID    RunID
	Feature  string
	RepoPath string

	Timing           TimingMetrics
	Cache            CacheMetrics
	Budget           BudgetMetrics
	Candidates       CandidateSelectionMetrics
	ContextReduction ContextReductionMetrics
	Providers        []ProviderMetrics

	// DeterministicHash is the SHA-256 of the canonical JSON response body.
	// Identical values across runs confirm deterministic output.
	DeterministicHash string

	// ArtifactPath is where the full response JSON was persisted (if any).
	ArtifactPath string
	// TraceArtifactPath is where the trace JSON was persisted (if any).
	TraceArtifactPath string
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature trace
// ──────────────────────────────────────────────────────────────────────────────

// TraceEvent records one step in the feature execution.
type TraceEvent struct {
	Timestamp time.Time
	Step      string
	Detail    string
}

// TraceDecision records an include/exclude/defer decision about a candidate.
type TraceDecision struct {
	Item   string  // file path, test name, etc.
	Action string  // "include", "exclude", "defer"
	Reason string
	Score  float64
}

// FeatureTrace is the full execution log for one feature run. It is written
// to an artifact file alongside the response.
type FeatureTrace struct {
	RunID     RunID
	Feature   string
	Events    []TraceEvent
	Decisions []TraceDecision
}
