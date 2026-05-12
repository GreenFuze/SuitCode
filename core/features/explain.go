package features

import "github.com/GreenFuze/SuitCode/core/provider"

// ExplainFileRequest parameters for the explain-file feature.
type ExplainFileRequest struct {
	BaseFeatureRequest
	// FilePath is the path to the file to explain, relative to the repo root
	// or absolute.
	FilePath string
}

// ExplainFileResponse is the structured result of an explain-file run.
type ExplainFileResponse struct {
	BaseFeatureResponse

	// FilePath is the resolved absolute path of the explained file.
	FilePath string
	// RelPath is the path relative to the repository root.
	RelPath string
	// Language is the detected language of the file.
	Language string
	// FileRole classifies the file: "source", "test", "generated", etc.
	FileRole string
	// Symbols lists important symbols found in the file (functions, types,
	// constants). Populated only when a language provider is attached.
	Symbols []SymbolInfo
	// Imports lists files or packages imported by this file.
	Imports []provider.FileReference
	// Dependents lists files that import this file (requires dependency index).
	Dependents []provider.FileReference
	// RelatedTests lists test files associated with this file.
	RelatedTests []TestReference
	// RelatedFiles lists other files with a meaningful relationship.
	RelatedFiles []provider.FileReference
	// RisksAndBoundaries lists anything the caller should be careful about
	// when modifying this file.
	RisksAndBoundaries []string

	// FileTokenEstimate is the estimated token cost of the file itself.
	FileTokenEstimate provider.TokenEstimate
}

// SymbolInfo describes a single symbol exported from a file.
type SymbolInfo struct {
	Name       string
	Kind       string // "func", "type", "var", "const", "interface", "struct"
	Signature  string // brief signature or type, if available
	Provenance provider.Provenance
}
