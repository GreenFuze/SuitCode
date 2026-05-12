// Package goprovider implements the Go language provider for SuitCode.
// Phase 1 uses golang.org/x/tools/go/packages for static import-graph analysis;
// Phase 2 will add gopls (LSP subprocess) for symbol-level navigation.
package goprovider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/GreenFuze/SuitCode/core/provider"
)

// packageNode holds the essential metadata for one Go package after a
// successful go/packages.Load call. It is immutable once constructed.
type packageNode struct {
	// PkgPath is the fully-qualified import path, e.g.
	// "github.com/GreenFuze/SuitCode/core/features".
	PkgPath string

	// GoFiles holds the absolute paths of non-test .go source files.
	GoFiles []string

	// ImportIDs is the sorted list of import path strings for packages
	// directly imported by this package.
	ImportIDs []string
}

// packageIndex is the in-memory import graph built from a go/packages load.
// All maps use absolute file paths or import path strings as keys.
// The struct is immutable after construction.
type packageIndex struct {
	// byPkgPath maps import path → packageNode.
	byPkgPath map[string]*packageNode

	// byFile maps absolute .go file path → owning packageNode.
	// Allows O(1) lookup: "which package owns this file?"
	byFile map[string]*packageNode

	// reverseImports maps import path → sorted slice of import paths of
	// packages that directly import it.
	reverseImports map[string][]string
}

// loadPackageGraph loads the full import graph for the Go module(s) rooted at
// repoPath using go/packages. Only packages whose directory is under repoPath
// are indexed — stdlib and external dependencies are excluded.
//
// Failure to load (e.g. go binary not found, no go.mod) returns a non-nil
// error. Individual package-level errors (e.g. a single file with a syntax
// error) are returned as Limitations, not as a hard error, so the caller can
// still use a partial index.
func loadPackageGraph(ctx context.Context, repoPath string) (*packageIndex, []provider.Limitation, error) {
	cfg := &packages.Config{
		Context: ctx,
		// NeedName: pkg.PkgPath is set.
		// NeedFiles: pkg.GoFiles (non-test .go files) is set.
		// NeedImports: pkg.Imports map is populated with direct imports.
		// NeedDeps is intentionally omitted — loading ./... already gives us
		// GoFiles for all module packages. NeedDeps would additionally load
		// stdlib and external packages, which we neither need nor want.
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports,
		Dir:  repoPath,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, nil, fmt.Errorf("go/packages load: %w", err)
	}

	idx := &packageIndex{
		byPkgPath:      make(map[string]*packageNode),
		byFile:         make(map[string]*packageNode),
		reverseImports: make(map[string][]string),
	}

	var limitations []provider.Limitation

	// ── First pass: build nodes for every module-local package ───────────────
	for _, pkg := range pkgs {
		// Skip packages whose source is not under the repo root (e.g. stubs
		// injected by the go tool for missing dependencies).
		if pkg.PkgPath == "" || !strings.HasPrefix(pkg.Dir, repoPath) {
			continue
		}

		// Collect any package-level errors as Limitations.
		for _, pkgErr := range pkg.Errors {
			limitations = append(limitations, provider.Limitation{
				Kind:    "package_load_error",
				Message: pkgErr.Error(),
				Scope:   pkg.PkgPath,
			})
		}

		// Collect direct import paths from the Imports map keys.
		importIDs := make([]string, 0, len(pkg.Imports))
		for importPath := range pkg.Imports {
			importIDs = append(importIDs, importPath)
		}
		sort.Strings(importIDs)

		// Sort GoFiles for determinism.
		goFiles := make([]string, len(pkg.GoFiles))
		copy(goFiles, pkg.GoFiles)
		sort.Strings(goFiles)

		node := &packageNode{
			PkgPath:   pkg.PkgPath,
			GoFiles:   goFiles,
			ImportIDs: importIDs,
		}

		idx.byPkgPath[pkg.PkgPath] = node
		for _, f := range goFiles {
			idx.byFile[f] = node
		}
	}

	// ── Second pass: build reverse import index ───────────────────────────────
	//
	// For each node N and each import path I in N.ImportIDs, record that N
	// imports I (i.e. I is imported by N). This lets importerFiles() answer
	// "who imports package P?" in O(1).
	for _, node := range idx.byPkgPath {
		for _, importID := range node.ImportIDs {
			idx.reverseImports[importID] = append(idx.reverseImports[importID], node.PkgPath)
		}
	}

	// Sort each reverse-import list for determinism.
	for k := range idx.reverseImports {
		sort.Strings(idx.reverseImports[k])
	}

	return idx, limitations, nil
}

// fileToNode returns the packageNode that owns absFilePath, or nil when the
// file is not in the index.
func (idx *packageIndex) fileToNode(absFilePath string) *packageNode {
	return idx.byFile[absFilePath]
}

// importedFiles returns the sorted, deduplicated absolute file paths of all
// non-test .go files in packages directly imported by the package containing
// absFilePath. Returns nil (not an error) when absFilePath is not indexed.
func (idx *packageIndex) importedFiles(absFilePath string) []string {
	node := idx.byFile[absFilePath]
	if node == nil {
		return nil
	}

	// Use a set to deduplicate across packages that share files (unusual but
	// possible with build tags).
	seen := make(map[string]bool)
	var result []string

	for _, importID := range node.ImportIDs {
		imported := idx.byPkgPath[importID]
		if imported == nil {
			// stdlib or external dep — not in our module-local index.
			continue
		}
		for _, f := range imported.GoFiles {
			if !seen[f] {
				seen[f] = true
				result = append(result, f)
			}
		}
	}

	sort.Strings(result)
	return result
}

// importerFiles returns the sorted, deduplicated absolute file paths of all
// non-test .go files in packages that directly import the package containing
// absFilePath. Returns nil (not an error) when absFilePath is not indexed.
func (idx *packageIndex) importerFiles(absFilePath string) []string {
	node := idx.byFile[absFilePath]
	if node == nil {
		return nil
	}

	importerPkgPaths := idx.reverseImports[node.PkgPath]
	if len(importerPkgPaths) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var result []string

	for _, pkgPath := range importerPkgPaths {
		importer := idx.byPkgPath[pkgPath]
		if importer == nil {
			continue
		}
		for _, f := range importer.GoFiles {
			if !seen[f] {
				seen[f] = true
				result = append(result, f)
			}
		}
	}

	sort.Strings(result)
	return result
}
