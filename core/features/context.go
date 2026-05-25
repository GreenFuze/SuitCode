package features

import "github.com/GreenFuze/SuitCode/core/provider"

// ContextRequest parameters for the context feature.
type ContextRequest struct {
	BaseFeatureRequest
	// Files is the set of seed files to build context around.
	Files []string
	// Symbols narrows context to specific symbols (future use).
	Symbols []string
	// DiffRef, if set, uses changed files as additional seeds.
	DiffRef string
}

// ContextFact is one piece of content included in the capsule.
type ContextFact struct {
	// Kind describes what this fact contains: "file_content", "file_summary",
	// "symbol_signature", etc.
	Kind string `json:"kind"`
	// Content is the actual text included in the capsule.
	Content string `json:"content"`
	// Source identifies the file this fact came from.
	Source        provider.FileReference `json:"source"`
	Provenance    provider.Provenance    `json:"provenance"`
	TokenEstimate provider.TokenEstimate `json:"token_estimate"`
}

// ContextCandidate is a file evaluated for inclusion in the capsule.
type ContextCandidate struct {
	File          provider.FileReference `json:"file"`
	// Score is a 0–1 relevance score; higher means more likely to be included.
	Score         float64                `json:"score"`
	ScoreReasons  []string               `json:"score_reasons,omitempty"`
	TokenEstimate provider.TokenEstimate `json:"token_estimate"`
}

// ContextSelection records a candidate that was included and why.
type ContextSelection struct {
	Candidate ContextCandidate `json:"candidate"`
	Rank      int              `json:"rank"`
	Reason    string           `json:"reason"`
}

// ContextRejection records a candidate that was excluded and why.
type ContextRejection struct {
	Candidate ContextCandidate `json:"candidate"`
	Reason    string           `json:"reason"`
}

// ContextCapsule is the bounded, evidence-backed context package returned by
// the context feature. This is the primary product artefact of SuitCode.
type ContextCapsule struct {
	Facts      []ContextFact      `json:"facts"`
	Selections []ContextSelection `json:"selections"`
	Rejections []ContextRejection `json:"rejections"`
	// TotalEstimate is the token estimate for all included facts combined.
	TotalEstimate   provider.TokenEstimate `json:"total_estimate"`
	BudgetRequested int                    `json:"budget_requested"`
	BudgetUsed      int                    `json:"budget_used"`
	// CompressionRatio = BudgetUsed / (token estimate of all candidates).
	CompressionRatio float64 `json:"compression_ratio"`
}

// ContextFileEntry is a flat, agent-friendly view of a single file selected into
// the context capsule. Agents should iterate Files rather than navigating the
// nested Capsule.Facts structure.
type ContextFileEntry struct {
	// Path is the absolute path on disk.
	Path string `json:"path"`
	// RelPath is the path relative to the repository root.
	RelPath string `json:"rel_path"`
	// Language is the detected programming language.
	Language string `json:"language,omitempty"`
	// Role is the file role: "source", "test", "generated", etc.
	Role string `json:"role,omitempty"`
	// TokenEstimate is the approximate token cost of the file content.
	TokenEstimate int `json:"token_estimate"`
	// Rank is 1-based position in the ranked selection (lower = more relevant).
	Rank int `json:"rank"`
	// Score is the 0–1 relevance score assigned to this file.
	Score float64 `json:"score"`
	// Reason explains why this file was included.
	Reason string `json:"reason"`
	// Content is the full text of the file as included in the capsule.
	Content string `json:"content"`
}

// ContextResponse is the structured result of a context run.
type ContextResponse struct {
	BaseFeatureResponse

	// Files is a flat, agent-friendly list of files selected into the capsule,
	// ordered by relevance rank. Each entry includes the file content. This is
	// the primary field agents should use — no nested traversal required.
	Files []ContextFileEntry `json:"files"`

	Capsule ContextCapsule `json:"capsule"`

	FilesConsidered int `json:"files_considered"`
	FilesIncluded   int `json:"files_included"`
	FilesExcluded   int `json:"files_excluded"`

	// EvidenceScanned is the token-equivalent of all candidates examined.
	EvidenceScanned provider.TokenEstimate `json:"evidence_scanned"`
	// EstimatedContextAvoided = EvidenceScanned - Capsule.TotalEstimate.
	EstimatedContextAvoided provider.TokenEstimate `json:"estimated_context_avoided"`
	// CompressionRatio mirrors Capsule.CompressionRatio for easy access.
	CompressionRatio float64 `json:"compression_ratio"`

	// IncludedRelPaths lists the repo-relative paths of all files selected
	// into the capsule, in rank order. Populated by RunContext; used by
	// KindGoldenFiles eval checks.
	IncludedRelPaths []string `json:"included_rel_paths,omitempty"`
}
