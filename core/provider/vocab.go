package provider

// Authority describes how confident we are in a piece of evidence.
type Authority string

const (
	// AuthorityVerified means the evidence comes directly from a manifest,
	// build file, or other machine-readable source of truth.
	AuthorityVerified Authority = "verified"

	// AuthorityDerived means the evidence was computed from verified data
	// (e.g. a dependency graph built from go.mod).
	AuthorityDerived Authority = "derived"

	// AuthorityHeuristic means the evidence was inferred from naming
	// conventions, directory structure, or similar patterns.
	AuthorityHeuristic Authority = "heuristic"

	// AuthorityAdvisory means the evidence is a suggestion the caller
	// should validate before acting on.
	AuthorityAdvisory Authority = "advisory"
)

// SourceKind identifies the tool or artefact that produced a piece of evidence.
type SourceKind string

const (
	SourceKindManifest    SourceKind = "manifest"
	SourceKindGit         SourceKind = "git"
	SourceKindFilesystem  SourceKind = "filesystem"
	SourceKindSyntax      SourceKind = "syntax"
	SourceKindLSP         SourceKind = "lsp"
	SourceKindHeuristic   SourceKind = "heuristic"
	SourceKindTestTool    SourceKind = "test_tool"
	SourceKindBuildTool   SourceKind = "build_tool"
)

// Provenance records the origin and trustworthiness of a piece of evidence.
// Every evidence item returned by a provider must carry provenance.
type Provenance struct {
	SourceKind SourceKind
	SourceTool string
	Authority  Authority
	// EvidenceSummary is a short human-readable description of what was observed.
	EvidenceSummary string
	// EvidencePaths lists the specific files or URIs that were consulted.
	EvidencePaths []string
}

// Limitation describes something the provider could not determine or a
// boundary beyond which its answer should not be trusted.
type Limitation struct {
	// Kind is a machine-readable category (e.g. "no_lsp", "large_file",
	// "no_build_manifest").
	Kind string
	// Message is a human-readable explanation.
	Message string
	// Scope describes which files or subsystems this limitation applies to.
	Scope string
}

// EvidenceItem is a generic labelled evidence value produced by a provider.
type EvidenceItem struct {
	Kind       string
	Value      string
	Provenance Provenance
}

// FileReference identifies a file within a repository, with language and role
// classification and a provenance chain.
type FileReference struct {
	// Path is the absolute filesystem path.
	Path string
	// RelPath is the path relative to the repository root, using forward slashes.
	RelPath string
	// Language is the detected programming or markup language (empty if unknown).
	Language string
	// Role classifies the file: "source", "test", "generated", "config",
	// "docs", "vendor", or "other".
	Role       string
	Provenance Provenance
}
