package features

import "github.com/GreenFuze/SuitCode/core/provider"

// RepoOverviewRequest parameters for the repo-overview feature.
type RepoOverviewRequest struct {
	BaseFeatureRequest
}

// RepoOverviewResponse is the structured result of a repo-overview run.
type RepoOverviewResponse struct {
	BaseFeatureResponse

	// Languages lists detected programming languages ordered by file count.
	Languages []string
	// TopLevelStructure lists notable top-level directories and files.
	TopLevelStructure []DirectoryEntry
	// ConfigFiles lists key configuration files found at the root.
	ConfigFiles []provider.FileReference
	// BuildSystems lists detected build system names.
	BuildSystems []string
	// TestSystems lists detected test framework names.
	TestSystems []string
	// NotableDirectories highlights directories worth knowing about.
	NotableDirectories []DirectoryEntry
	// GeneratedAreas lists paths that appear to contain generated files.
	GeneratedAreas []string
	// IgnoredAreas lists paths excluded by .gitignore or similar.
	IgnoredAreas []string
	// TotalFiles is the total count of non-directory files indexed.
	TotalFiles int
}
