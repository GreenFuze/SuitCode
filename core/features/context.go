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
	Kind    string
	// Content is the actual text included in the capsule.
	Content string
	// Source identifies the file this fact came from.
	Source     provider.FileReference
	Provenance provider.Provenance
	TokenEstimate provider.TokenEstimate
}

// ContextCandidate is a file evaluated for inclusion in the capsule.
type ContextCandidate struct {
	File          provider.FileReference
	// Score is a 0–1 relevance score; higher means more likely to be included.
	Score         float64
	ScoreReasons  []string
	TokenEstimate provider.TokenEstimate
}

// ContextSelection records a candidate that was included and why.
type ContextSelection struct {
	Candidate ContextCandidate
	Rank      int
	Reason    string
}

// ContextRejection records a candidate that was excluded and why.
type ContextRejection struct {
	Candidate ContextCandidate
	Reason    string
}

// ContextCapsule is the bounded, evidence-backed context package returned by
// the context feature. This is the primary product artefact of SuitCode.
type ContextCapsule struct {
	Facts       []ContextFact
	Selections  []ContextSelection
	Rejections  []ContextRejection
	// TotalEstimate is the token estimate for all included facts combined.
	TotalEstimate   provider.TokenEstimate
	BudgetRequested int
	BudgetUsed      int
	// CompressionRatio = BudgetUsed / (token estimate of all candidates).
	CompressionRatio float64
}

// ContextFileEntry is a flat, agent-friendly view of a single file selected into
// the context capsule. Agents should iterate Files rather than navigating the
// nested Capsule.Facts structure.
type ContextFileEntry struct {
	// Path is the absolute path on disk.
	Path string
	// RelPath is the path relative to the repository root.
	RelPath string
	// Language is the detected programming language.
	Language string
	// Role is the file role: "source", "test", "generated", etc.
	Role string
	// TokenEstimate is the approximate token cost of the file content.
	TokenEstimate int
	// Rank is 1-based position in the ranked selection (lower = more relevant).
	Rank int
	// Score is the 0–1 relevance score assigned to this file.
	Score float64
	// Reason explains why this file was included.
	Reason string
	// Content is the full text of the file as included in the capsule.
	Content string
}

// ContextResponse is the structured result of a context run.
type ContextResponse struct {
	BaseFeatureResponse

	// Files is a flat, agent-friendly list of files selected into the capsule,
	// ordered by relevance rank. Each entry includes the file content. This is
	// the primary field agents should use — no nested traversal required.
	Files []ContextFileEntry

	Capsule ContextCapsule

	FilesConsidered int
	FilesIncluded   int
	FilesExcluded   int

	// EvidenceScanned is the token-equivalent of all candidates examined.
	EvidenceScanned provider.TokenEstimate
	// EstimatedContextAvoided = EvidenceScanned - Capsule.TotalEstimate.
	EstimatedContextAvoided provider.TokenEstimate
	// CompressionRatio mirrors Capsule.CompressionRatio for easy access.
	CompressionRatio float64

	// IncludedRelPaths lists the repo-relative paths of all files selected
	// into the capsule, in rank order. Populated by RunContext; used by
	// KindGoldenFiles eval checks.
	IncludedRelPaths []string
}
