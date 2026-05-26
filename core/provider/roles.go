package provider

import "context"

// ──────────────────────────────────────────────────────────────────────────────
// Filesystem provider
// ──────────────────────────────────────────────────────────────────────────────

// FilesystemFile describes a single file discovered during a repository walk.
type FilesystemFile struct {
	Path     string // absolute path
	RelPath  string // relative to repo root, forward-slash separated
	Size     int64
	Language string // empty if not recognised
	// Role classifies the file: "source", "test", "generated", "config",
	// "docs", "vendor", or "other".
	Role  string
	IsDir bool
}

// FilesystemListing is the full result of a directory walk.
type FilesystemListing struct {
	Files []FilesystemFile
	// TotalFiles is the count of non-directory entries returned.
	TotalFiles int
	// TotalDirs is the count of directories visited (not skipped).
	TotalDirs int
	// Languages lists detected languages ordered by file count, descending.
	Languages []string
	// BuildSystems lists detected build system names (e.g. "Go Modules", "npm").
	BuildSystems []string
	// TestSystems lists detected test framework names (e.g. "Go test", "Jest").
	TestSystems []string
	// IgnoredPaths lists path patterns that were skipped during the walk.
	IgnoredPaths []string
}

// FilesystemProvider performs directory walks and file classification.
type FilesystemProvider interface {
	Provider
	ListFiles(ctx context.Context) (*ProviderResult[FilesystemListing], error)
}

// ──────────────────────────────────────────────────────────────────────────────
// VCS provider
// ──────────────────────────────────────────────────────────────────────────────

// VCSCommit represents a single commit in the version-control history.
type VCSCommit struct {
	Hash      string
	ShortHash string
	Author    string
	Date      string
	Message   string
}

// VCSStatus describes the current working-tree state.
type VCSStatus struct {
	Branch   string
	HeadHash string
	IsClean  bool
	Modified []string
	Untracked []string
}

// VCSDiff represents a diff between two refs or between a ref and HEAD.
type VCSDiff struct {
	FromRef      string
	ToRef        string
	ChangedFiles []string
	Additions    int
	Deletions    int
	// RawDiff is the raw unified-diff text (may be empty if not requested).
	RawDiff string
}

// VCSProvider queries a version-control system for change and history data.
type VCSProvider interface {
	Provider
	Status(ctx context.Context) (*ProviderResult[VCSStatus], error)
	Diff(ctx context.Context, fromRef, toRef string) (*ProviderResult[VCSDiff], error)
	ChangedFiles(ctx context.Context, fromRef string) (*ProviderResult[[]string], error)
	RecentCommits(ctx context.Context, limit int) (*ProviderResult[[]VCSCommit], error)
}

// ──────────────────────────────────────────────────────────────────────────────
// Future provider interfaces — defined here so the type system is complete;
// no implementations exist in v1.
// ──────────────────────────────────────────────────────────────────────────────

// LanguageProvider performs semantic analysis of source files (LSP-backed in
// future versions).
type LanguageProvider interface {
	Provider
	GetImports(ctx context.Context, filePath string) (*ProviderResult[[]string], error)
	GetSymbols(ctx context.Context, filePath string) (*ProviderResult[[]string], error)
}

// ImportGraphProvider extends LanguageProvider with file-level import graph
// queries. Implemented by GoLanguageProvider in Phase 1 (static go/packages)
// and enriched by gopls in Phase 2 (symbol-level navigation).
//
// All methods operate one hop only — direct imports, not transitive closure.
// Results contain only files that belong to the same module (no stdlib or
// external dependency files are included).
//
// Language providers that do not support a given concept (e.g. JS has no
// compilation-unit peer concept) return an empty result, never an error.
type ImportGraphProvider interface {
	LanguageProvider

	// FileImports returns the absolute paths of all non-test source files in
	// packages/modules directly imported by the package containing filePath.
	// Returns an empty result (not an error) when filePath is not indexed.
	FileImports(ctx context.Context, filePath string) (*ProviderResult[[]string], error)

	// FileImporters returns the absolute paths of all non-test source files in
	// packages/modules that directly import the package containing filePath.
	// Returns an empty result (not an error) when filePath is not indexed.
	FileImporters(ctx context.Context, filePath string) (*ProviderResult[[]string], error)

	// FilePeers returns the absolute paths of all other non-test source files
	// that belong to the same compilation unit as filePath — the same Go
	// package, the same C# project, etc. These are files that are compiled
	// together with filePath and share its namespace/package scope.
	// Languages without a compilation-unit concept (JS, Python) return empty.
	// Returns an empty result (not an error) when filePath is not indexed.
	FilePeers(ctx context.Context, filePath string) (*ProviderResult[[]string], error)

	// FileTests returns the absolute paths of test files that directly test
	// the compilation unit containing filePath. For Go this is the set of
	// *_test.go files in the same package (both in-package and external).
	// Languages without structural test-to-source mapping return empty.
	// Returns an empty result (not an error) when filePath is not indexed.
	FileTests(ctx context.Context, filePath string) (*ProviderResult[[]string], error)
}

// TestProvider discovers and maps test cases to source files.
type TestProvider interface {
	Provider
	DiscoverTests(ctx context.Context) (*ProviderResult[[]string], error)
	FindRelevantTests(ctx context.Context, targetPath string) (*ProviderResult[[]string], error)
}

// BuildProvider analyses build graphs and targets.
type BuildProvider interface {
	Provider
	DetectTargets(ctx context.Context) (*ProviderResult[[]string], error)
}
