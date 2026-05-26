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
	Languages []string `json:"languages,omitempty"`
	// TopLevelStructure lists notable top-level directories and files.
	TopLevelStructure []DirectoryEntry `json:"top_level_structure,omitempty"`
	// ConfigFiles lists key configuration files found at the root.
	ConfigFiles []provider.FileReference `json:"config_files,omitempty"`
	// BuildSystems lists detected build system names.
	BuildSystems []string `json:"build_systems,omitempty"`
	// TestSystems lists detected test framework names.
	TestSystems []string `json:"test_systems,omitempty"`
	// NotableDirectories highlights directories worth knowing about.
	NotableDirectories []DirectoryEntry `json:"notable_directories,omitempty"`
	// GeneratedAreas lists paths that appear to contain generated files.
	GeneratedAreas []string `json:"generated_areas,omitempty"`
	// IgnoredAreas lists paths excluded by .gitignore or similar.
	IgnoredAreas []string `json:"ignored_areas,omitempty"`
	// TotalFiles is the total count of non-directory files indexed.
	TotalFiles int `json:"total_files"`
}
