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
	FilePath string `json:"file_path"`
	// RelPath is the path relative to the repository root.
	RelPath string `json:"rel_path"`
	// Language is the detected language of the file.
	Language string `json:"language,omitempty"`
	// FileRole classifies the file: "source", "test", "generated", etc.
	FileRole string `json:"file_role,omitempty"`
	// Symbols lists important symbols found in the file (functions, types,
	// constants). Populated only when a language provider is attached.
	Symbols []SymbolInfo `json:"symbols,omitempty"`
	// Imports lists files or packages imported by this file.
	Imports []provider.FileReference `json:"imports,omitempty"`
	// Dependents lists files that import this file (requires dependency index).
	Dependents []provider.FileReference `json:"dependents,omitempty"`
	// RelatedTests lists test files associated with this file.
	RelatedTests []TestReference `json:"related_tests,omitempty"`
	// RelatedFiles lists other files with a meaningful relationship.
	RelatedFiles []provider.FileReference `json:"related_files,omitempty"`
	// RisksAndBoundaries lists anything the caller should be careful about
	// when modifying this file.
	RisksAndBoundaries []string `json:"risks_and_boundaries,omitempty"`

	// FileTokenEstimate is the estimated token cost of the file itself.
	FileTokenEstimate provider.TokenEstimate `json:"file_token_estimate"`

	// ExternalDependencies lists external package dependencies for the project
	// containing this file (e.g. NuGet packages from the .csproj). Populated only
	// when a package-aware language provider is ready.
	ExternalDependencies []ExternalDependency `json:"external_dependencies,omitempty"`
}

// SymbolInfo describes a single symbol exported from a file.
type SymbolInfo struct {
	Name       string             `json:"name"`
	Kind       string             `json:"kind"` // "func", "type", "var", "const", "interface", "struct"
	Signature  string             `json:"signature,omitempty"` // brief signature or type, if available
	Provenance provider.Provenance `json:"provenance"`
}

// ExternalDependency is a declared external package dependency associated with
// the file being explained (e.g. a NuGet package from the containing .csproj).
type ExternalDependency struct {
	// Manager is the package manager: "NuGet", "npm", "pip", etc.
	Manager string `json:"manager"`
	// Name is the package name as declared in the manifest.
	Name string `json:"name"`
	// Version is the declared version constraint (empty when not specified).
	Version string `json:"version,omitempty"`
}
