package features

import "github.com/GreenFuze/SuitCode/core/provider"

// ImpactedFile is a file that may be affected by the change under analysis.
type ImpactedFile struct {
	File       provider.FileReference `json:"file"`
	Reason     string                 `json:"reason"`
	Provenance provider.Provenance    `json:"provenance"`
}

// ImpactRequest parameters for the impact feature.
type ImpactRequest struct {
	BaseFeatureRequest
	// FilePaths is an explicit list of changed files to analyse.
	FilePaths []string
	// GitRef, if set, derives the changed-file list from `git diff <GitRef>`.
	GitRef string
}

// ImpactResponse is the structured result of an impact run.
//
// JSON key reference (snake_case throughout):
//
//	changed_files    — the resolved list of changed input files
//	impacted_files   — files in the blast radius (import-graph OR proximity heuristic)
//	impacted_tests   — tests that cover the changed or impacted files
//	limitations      — degraded-quality notices (check kind:"no_import_graph"
//	                   to know whether impacted_files is graph-based or heuristic)
type ImpactResponse struct {
	BaseFeatureResponse

	// ChangedFiles is the resolved list of files that were changed.
	ChangedFiles []provider.FileReference `json:"changed_files"`

	// ImpactedFiles are files within the blast radius. When the
	// "no_import_graph" limitation is present these are proximity-based
	// (same-directory heuristic), NOT real import-graph dependents. Check
	// limitations[].kind == "no_import_graph" before treating this as a
	// definitive downstream list.
	ImpactedFiles []ImpactedFile `json:"impacted_files"`

	// ImpactedTests are tests that exercise the changed or impacted files.
	ImpactedTests []RelevantTest `json:"impacted_tests,omitempty"`

	// ImpactedTargets are build targets that include the impacted files.
	ImpactedTargets []BuildTargetReference `json:"impacted_targets,omitempty"`

	// DependencyPaths records the import chains that link changed to impacted files.
	DependencyPaths []DependencyPath `json:"dependency_paths,omitempty"`

	// RiskyBoundaries flags any high-risk interface boundaries in the blast radius.
	RiskyBoundaries []RiskyBoundary `json:"risky_boundaries,omitempty"`

	// GeneratedWarnings lists any generated files in the blast radius.
	GeneratedWarnings []string `json:"generated_warnings,omitempty"`

	FilesConsidered int `json:"files_considered"`
	FilesIncluded   int `json:"files_included"`
	FilesExcluded   int `json:"files_excluded"`

	EstimatedContextAvoided provider.TokenEstimate `json:"estimated_context_avoided"`
}
