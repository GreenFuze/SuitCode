package features

import "github.com/GreenFuze/SuitCode/core/provider"

// RelationKind describes how two files are related.
type RelationKind string

const (
	RelationSamePackage  RelationKind = "same_package"
	RelationSameModule   RelationKind = "same_module"
	RelationImports      RelationKind = "imports"
	RelationImportedBy   RelationKind = "imported_by"
	RelationTest         RelationKind = "test"         // this file tests the target
	RelationTestedBy     RelationKind = "tested_by"    // the target tests this file
	RelationSimilarName  RelationKind = "similar_name" // heuristic naming proximity
	RelationHeuristic    RelationKind = "heuristic"
)

// RelatedFile is a file with an annotated relationship to the query target.
type RelatedFile struct {
	File       provider.FileReference `json:"file"`
	Relation   RelationKind           `json:"relation"`
	Reason     string                 `json:"reason,omitempty"`
	Provenance provider.Provenance    `json:"provenance"`
	// Confidence is a 0–1 score. Higher means more certain.
	Confidence float64 `json:"confidence"`
}

// RelatedRequest parameters for the related feature.
type RelatedRequest struct {
	BaseFeatureRequest
	// FilePath is the file whose related files we want.
	FilePath string
}

// RelatedResponse is the structured result of a related run.
type RelatedResponse struct {
	BaseFeatureResponse

	TargetPath   string        `json:"target_path"`
	RelatedFiles []RelatedFile `json:"related_files,omitempty"`

	FilesConsidered int `json:"files_considered"`
	FilesIncluded   int `json:"files_included"`
	FilesExcluded   int `json:"files_excluded"`

	// EstimatedContextAvoided is how many tokens a caller would NOT need to
	// load, given that this response tells them which files matter.
	EstimatedContextAvoided provider.TokenEstimate `json:"estimated_context_avoided"`
}
