package features

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultExplainBudget = 6_000

// RunExplainFile produces an ExplainFileResponse for the given request.
func RunExplainFile(
	_ context.Context,
	req cfeatures.ExplainFileRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
) (*cfeatures.ExplainFileResponse, error) {
	if req.FilePath == "" {
		return nil, fmt.Errorf("explain-file: --path is required")
	}

	budget := budgetOrDefault(req.Budget, defaultExplainBudget)
	runID := newRunID("explain-file")
	metrics, start := startMetrics(runID, "explain-file", req.RepoPath, budget)

	// Locate the file in the index.
	fsFile, err := findFile(listing, req.FilePath, req.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("explain-file: %w", err)
	}

	resp := &cfeatures.ExplainFileResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
		FilePath:            fsFile.Path,
		RelPath:             fsFile.RelPath,
		Language:            fsFile.Language,
		FileRole:            fsFile.Role,
	}

	// Estimate file size.
	fileEst, _ := estimator.EstimateFile(fsFile.Path)
	resp.FileTokenEstimate = fileEst

	// Parse imports (language-aware, heuristic in v1).
	imports, limitations := parseImports(fsFile, req.RepoPath)
	resp.Imports = imports
	resp.Limitations = limitations

	// Find test files for this source file.
	testFiles := testFilesForSource(listing, fsFile.RelPath)
	for _, tf := range testFiles {
		prov := heuristicProv("naming_convention",
			fmt.Sprintf("test file matches %s by naming convention", fsFile.RelPath), tf.Path)
		resp.RelatedTests = append(resp.RelatedTests, cfeatures.TestReference{
			Name:       filepath.Base(tf.RelPath),
			FilePath:   tf.Path,
			RelPath:    tf.RelPath,
			RunCommand: buildTestCommand(tf, listing),
			Framework:  detectFramework(listing),
			Provenance: prov,
		})
	}

	// Find related files in same directory.
	sameDir := filesInSameDir(listing, fsFile.RelPath)
	for _, f := range sameDir {
		if f.Role == "test" {
			continue // already covered by RelatedTests
		}
		ref := fileToRef(f, heuristicProv("same_directory",
			fmt.Sprintf("%s is in the same directory as %s", f.RelPath, fsFile.RelPath), f.Path))
		resp.RelatedFiles = append(resp.RelatedFiles, ref)
	}

	// Add risk notes.
	resp.RisksAndBoundaries = buildRisks(fsFile, listing)

	// Metrics.
	scanned := fileEst.Tokens
	for _, f := range sameDir {
		est, _ := estimator.EstimateFile(f.Path)
		scanned += est.Tokens
	}

	outputTokens := estimator.Estimate(
		resp.RelPath + resp.Language + resp.FileRole +
			fmt.Sprintf("%d imports %d related %d tests",
				len(resp.Imports), len(resp.RelatedFiles), len(resp.RelatedTests)),
	).Tokens

	metrics.Budget.Used = outputTokens
	computeContextReduction(&metrics, scanned, outputTokens,
		len(listing.Data.Files), 1+len(sameDir), listing.Data.TotalFiles-1-len(sameDir))

	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// parseImports extracts import paths from a source file using simple text
// scanning. Returns FileReferences for files that exist in the repository.
func parseImports(f *provider.FilesystemFile, repoPath string) ([]provider.FileReference, []provider.Limitation) {
	var refs []provider.FileReference
	var limitations []provider.Limitation

	switch f.Language {
	case "Go":
		refs = parseGoImports(f, repoPath)
	case "Python":
		refs = parsePythonImports(f, repoPath)
	default:
		// For languages without a parser in v1, return an advisory limitation.
		limitations = append(limitations, provider.Limitation{
			Kind:    "no_import_parser",
			Message: fmt.Sprintf("import parsing not implemented for %s in v1; showing file relationships only", f.Language),
			Scope:   f.RelPath,
		})
	}

	return refs, limitations
}

// parseGoImports scans a Go file for import blocks and resolves local package
// paths to FileReferences.
func parseGoImports(f *provider.FilesystemFile, repoPath string) []provider.FileReference {
	file, err := os.Open(f.Path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var refs []provider.FileReference
	inBlock := false
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "import (") {
			inBlock = true
			continue
		}
		if inBlock && line == ")" {
			inBlock = false
			continue
		}

		var importPath string
		if inBlock {
			// Strip alias if present, then extract the quoted path.
			importPath = extractQuoted(line)
		} else if strings.HasPrefix(line, "import ") {
			importPath = extractQuoted(strings.TrimPrefix(line, "import "))
		}

		if importPath == "" {
			continue
		}

		// Only record local imports (those starting with the module path or
		// a relative path). Standard library and external imports are noted
		// as provenance only.
		if isLocalGoImport(importPath, repoPath) {
			refs = append(refs, provider.FileReference{
				RelPath:  importPath,
				Language: "Go",
				Role:     "source",
				Provenance: provider.Provenance{
					SourceKind:      provider.SourceKindSyntax,
					SourceTool:      "go_import_scanner",
					Authority:       provider.AuthorityDerived,
					EvidenceSummary: fmt.Sprintf("import %q in %s", importPath, f.RelPath),
					EvidencePaths:   []string{f.Path},
				},
			})
		}
	}

	return refs
}

// parsePythonImports performs a basic scan of Python import statements.
func parsePythonImports(f *provider.FilesystemFile, _ string) []provider.FileReference {
	file, err := os.Open(f.Path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var refs []provider.FileReference
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "from .") || strings.HasPrefix(line, "import .") {
			// Relative import — this is local.
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				refs = append(refs, provider.FileReference{
					RelPath: parts[1],
					Language: "Python",
					Role:    "source",
					Provenance: provider.Provenance{
						SourceKind:      provider.SourceKindSyntax,
						SourceTool:      "python_import_scanner",
						Authority:       provider.AuthorityHeuristic,
						EvidenceSummary: fmt.Sprintf("relative import %q in %s", parts[1], f.RelPath),
						EvidencePaths:   []string{f.Path},
					},
				})
			}
		}
	}

	return refs
}

func extractQuoted(s string) string {
	s = strings.TrimSpace(s)
	// Handle: "pkg", `pkg`, _ "pkg", name "pkg"
	start := strings.IndexAny(s, `"` + "`")
	if start < 0 {
		return ""
	}
	q := s[start]
	end := strings.IndexByte(s[start+1:], q)
	if end < 0 {
		return ""
	}
	return s[start+1 : start+1+end]
}

func isLocalGoImport(importPath, repoPath string) bool {
	// Heuristic: if the path contains a dot (domain.com/...), treat it as
	// potentially local only if it starts with the module path.
	// For v1 we just flag paths with fewer than 2 slashes as likely local.
	return !strings.Contains(importPath, ".") ||
		strings.HasPrefix(importPath, filepath.Base(repoPath))
}

func buildTestCommand(f provider.FilesystemFile, listing *provider.ProviderResult[provider.FilesystemListing]) string {
	if f.Language == "Go" || strings.HasSuffix(f.RelPath, "_test.go") {
		dir := filepath.ToSlash(filepath.Dir(f.RelPath))
		if dir == "." {
			return "go test ."
		}
		return fmt.Sprintf("go test ./%s/...", dir)
	}
	for _, ts := range listing.Data.TestSystems {
		switch ts {
		case "pytest":
			return fmt.Sprintf("pytest %s", f.RelPath)
		case "Jest", "Vitest":
			return fmt.Sprintf("npx %s %s", strings.ToLower(ts), f.RelPath)
		}
	}
	return ""
}

func detectFramework(listing *provider.ProviderResult[provider.FilesystemListing]) string {
	if len(listing.Data.TestSystems) > 0 {
		return listing.Data.TestSystems[0]
	}
	return ""
}

func buildRisks(f *provider.FilesystemFile, listing *provider.ProviderResult[provider.FilesystemListing]) []string {
	var risks []string

	if f.Role == "generated" {
		risks = append(risks, "⚠ This file appears to be generated — edit the source template, not this file directly.")
	}

	// Check if this is an interface definition file.
	if f.Language == "Go" && (strings.Contains(f.RelPath, "interface") ||
		strings.Contains(strings.ToLower(filepath.Base(f.RelPath)), "iface")) {
		risks = append(risks, "This file may define interface boundaries. Changes here may break all implementors.")
	}

	// Check for API/handler files.
	base := strings.ToLower(filepath.Base(f.RelPath))
	if strings.Contains(base, "handler") || strings.Contains(base, "router") ||
		strings.Contains(base, "controller") || strings.Contains(base, "api") {
		risks = append(risks, "API surface file — changes may affect external callers or OpenAPI contracts.")
	}

	// Check if it has many dependents (rough: many files in the same directory).
	same := filesInSameDir(listing, f.RelPath)
	if len(same) > 10 {
		risks = append(risks, fmt.Sprintf("Busy directory (%d siblings) — verify blast radius with 'impact' command.", len(same)))
	}

	return risks
}
