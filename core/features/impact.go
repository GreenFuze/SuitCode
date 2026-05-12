package features

import "github.com/GreenFuze/SuitCode/core/provider"

// ImpactedFile is a file that may be affected by the change under analysis.
type ImpactedFile struct {
	File       provider.FileReference
	Reason     string
	Provenance provider.Provenance
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
type ImpactResponse struct {
	BaseFeatureResponse

	// ChangedFiles is the resolved list of files that were changed.
	ChangedFiles []provider.FileReference
	// ImpactedFiles are files that import or otherwise depend on the changed files.
	ImpactedFiles []ImpactedFile
	// ImpactedTests are tests that exercise the changed or impacted files.
	ImpactedTests []RelevantTest
	// ImpactedTargets are build targets that include the impacted files.
	ImpactedTargets []BuildTargetReference
	// DependencyPaths records the import chains that link changed to impacted files.
	DependencyPaths []DependencyPath
	// RiskyBoundaries flags any high-risk interface boundaries in the blast radius.
	RiskyBoundaries []RiskyBoundary
	// GeneratedWarnings lists any generated files in the blast radius.
	GeneratedWarnings []string

	FilesConsidered int
	FilesIncluded   int
	FilesExcluded   int

	EstimatedContextAvoided provider.TokenEstimate
}
