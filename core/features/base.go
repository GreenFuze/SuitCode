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
	RunID RunID `json:"run_id"`
	// IsPartial is true when the response is incomplete due to a limitation
	// (e.g. a provider is not yet implemented). Never silently pretend a
	// partial result is complete.
	IsPartial bool `json:"is_partial,omitempty"`
	// Limitations lists anything the investigator could not determine.
	Limitations []provider.Limitation `json:"limitations,omitempty"`
	Metrics     FeatureMetrics        `json:"metrics"`
	Trace       FeatureTrace          `json:"trace"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Shared evidence types used across multiple features
// ──────────────────────────────────────────────────────────────────────────────

// TestReference identifies a single test case and how to run it.
type TestReference struct {
	Name       string             `json:"name"`
	FilePath   string             `json:"file_path"`
	RelPath    string             `json:"rel_path"`
	RunCommand string             `json:"run_command"`
	Framework  string             `json:"framework,omitempty"`
	Provenance provider.Provenance `json:"provenance"`
}

// BuildTargetReference identifies a build target.
type BuildTargetReference struct {
	Name       string             `json:"name"`
	Kind       string             `json:"kind"`
	FilePath   string             `json:"file_path"`
	Command    string             `json:"command"`
	Provenance provider.Provenance `json:"provenance"`
}

// DirectoryEntry describes one entry in a directory listing used by the
// repo-overview response.
type DirectoryEntry struct {
	RelPath  string `json:"rel_path"`
	IsDir    bool   `json:"is_dir"`
	Notes    string `json:"notes,omitempty"`
	Language string `json:"language,omitempty"`
}

// DependencyPath records one path through the import/dependency graph.
type DependencyPath struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Chain []string `json:"chain"`
}

// RiskyBoundary flags an interface boundary that change impact analysis
// should highlight.
type RiskyBoundary struct {
	Description string             `json:"description"`
	FilePath    string             `json:"file_path"`
	Reason      string             `json:"reason"`
	Provenance  provider.Provenance `json:"provenance"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Metrics types
// ──────────────────────────────────────────────────────────────────────────────

// TimingMetrics records when a feature run started and finished.
type TimingMetrics struct {
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt time.Time        `json:"finished_at"`
	DurationMs int64            `json:"duration_ms"`
	// ProviderMs maps ProviderID to its wall-clock contribution.
	ProviderMs map[string]int64 `json:"provider_ms,omitempty"`
}

// CacheMetrics describes cache behaviour for this run.
type CacheMetrics struct {
	Hit      bool       `json:"hit"`
	Key      string     `json:"key,omitempty"`
	StoredAt *time.Time `json:"stored_at,omitempty"`
	// WarmCold is "warm" if a cached index was used, "cold" otherwise.
	WarmCold string `json:"warm_cold,omitempty"`
}

// BudgetMetrics tracks requested vs used token budget.
type BudgetMetrics struct {
	Requested int `json:"requested"`
	Used      int `json:"used"`
	// Compliance is Used/Requested (0–1). A value > 1 means the budget was
	// exceeded (this should not happen; the context compiler enforces the limit).
	Compliance float64 `json:"compliance,omitempty"`
}

// CandidateSelectionMetrics records how many candidates were considered,
// included, and excluded during a selection step.
type CandidateSelectionMetrics struct {
	Considered int `json:"considered"`
	Included   int `json:"included"`
	Excluded   int `json:"excluded"`
}

// ContextReductionMetrics is the primary evaluation signal. Every feature
// that produces or filters evidence should populate this.
type ContextReductionMetrics struct {
	// EvidenceScannedTokens is the token-equivalent of everything examined.
	EvidenceScannedTokens int `json:"evidence_scanned_tokens"`
	// CapsuleTokens is the estimated tokens in the final output.
	CapsuleTokens int `json:"capsule_tokens"`
	// EstimatedContextAvoided = EvidenceScannedTokens - CapsuleTokens.
	// Labelled "estimated" because both figures are approximations.
	EstimatedContextAvoided int `json:"estimated_context_avoided"`
	// CompressionRatio = CapsuleTokens / EvidenceScannedTokens.
	// Closer to 0 means more was filtered out.
	CompressionRatio float64 `json:"compression_ratio"`
	// File-level selection counts.
	FilesConsidered int `json:"files_considered"`
	FilesIncluded   int `json:"files_included"`
	FilesExcluded   int `json:"files_excluded"`

	// LspEnhanced is true when at least one import-graph signal (from a
	// LanguageProvider) contributed to candidate scoring in this run.
	LspEnhanced bool `json:"lsp_enhanced,omitempty"`
	// ImportEdgesScanned is the total count of import edges (forward +
	// reverse) examined across all seed files.
	ImportEdgesScanned int `json:"import_edges_scanned,omitempty"`
}

// ProviderMetrics records per-provider contribution to a feature run.
type ProviderMetrics struct {
	ProviderID  string                `json:"provider_id"`
	DisplayName string                `json:"display_name"`
	DurationMs  int64                 `json:"duration_ms"`
	CacheHit    bool                  `json:"cache_hit"`
	Error       string                `json:"error,omitempty"`
	Limitations []provider.Limitation `json:"limitations,omitempty"`
}

// FeatureMetrics aggregates all measurement data for one feature invocation.
type FeatureMetrics struct {
	RunID    RunID  `json:"run_id"`
	Feature  string `json:"feature"`
	RepoPath string `json:"repo_path"`

	Timing           TimingMetrics             `json:"timing"`
	Cache            CacheMetrics              `json:"cache"`
	Budget           BudgetMetrics             `json:"budget"`
	Candidates       CandidateSelectionMetrics `json:"candidates"`
	ContextReduction ContextReductionMetrics   `json:"context_reduction"`
	Providers        []ProviderMetrics         `json:"providers,omitempty"`

	// DeterministicHash is the SHA-256 of the canonical JSON response body.
	// Identical values across runs confirm deterministic output.
	DeterministicHash string `json:"deterministic_hash,omitempty"`

	// ArtifactPath is where the full response JSON was persisted (if any).
	ArtifactPath string `json:"artifact_path,omitempty"`
	// TraceArtifactPath is where the trace JSON was persisted (if any).
	TraceArtifactPath string `json:"trace_artifact_path,omitempty"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature trace
// ──────────────────────────────────────────────────────────────────────────────

// TraceEvent records one step in the feature execution.
type TraceEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Step      string    `json:"step"`
	Detail    string    `json:"detail,omitempty"`
}

// TraceDecision records an include/exclude/defer decision about a candidate.
type TraceDecision struct {
	Item   string  `json:"item"`
	Action string  `json:"action"`
	Reason string  `json:"reason"`
	Score  float64 `json:"score,omitempty"`
}

// FeatureTrace is the full execution log for one feature run. It is written
// to an artifact file alongside the response.
type FeatureTrace struct {
	RunID     RunID           `json:"run_id"`
	Feature   string          `json:"feature"`
	Events    []TraceEvent    `json:"events,omitempty"`
	Decisions []TraceDecision `json:"decisions,omitempty"`
}
