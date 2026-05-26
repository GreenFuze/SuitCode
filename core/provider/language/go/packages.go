// Package goprovider implements the Go language provider for SuitCode.
// Phase 1 uses golang.org/x/tools/go/packages for static import-graph analysis;
// Phase 2 will add gopls (LSP subprocess) for symbol-level navigation.
package goprovider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

	// Dir is the absolute path to the directory containing this package.
	// Used to locate *_test.go files (Go language spec mandates they live
	// in the same directory as the package they test).
	Dir string

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

// goSkipDirs are directories that should not be traversed when searching for
// go.mod files. Mirrors the skip lists used by the JS and Python providers
// plus the worktree guard.
var goSkipDirs = map[string]bool{
	".git":          true,
	".claude":       true, // Claude Code worktrees — must not be indexed
	"node_modules":  true,
	"vendor":        true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	"env":           true,
	".env":          true,
	"dist":          true,
	"build":         true,
	".next":         true,
	".nuxt":         true,
	".turbo":        true,
	".cache":        true,
	"coverage":      true,
	"testdata":      true,
	".pytest_cache": true,
	"site-packages": true,
}

// findModuleRoots walks repoPath and returns the directory path of every
// go.mod file found, in lexicographic order.
//
// If no go.mod is found anywhere under repoPath (e.g. the user opened a
// loose directory of Go snippets) the function returns []string{repoPath}
// so the caller can still attempt a best-effort load from the root — this
// preserves the original single-module behaviour.
func findModuleRoots(repoPath string) []string {
	var roots []string

	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if goSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			roots = append(roots, filepath.Dir(path))
		}
		return nil
	})

	if len(roots) == 0 {
		// No go.mod found: fall back to repo root so callers still get a
		// meaningful (though likely empty) package load attempt.
		return []string{repoPath}
	}

	sort.Strings(roots)
	return roots
}

// loadPackageGraph loads the full import graph for all Go module(s) found
// under repoPath using go/packages.
//
// For polyglot or multi-module repos (e.g. a monorepo where the Go backend
// lives under server/ with its own go.mod, or a repo with many plugin
// modules), the function discovers every go.mod via findModuleRoots, runs a
// separate go/packages.Load for each module root, and merges the results into
// a single unified packageIndex.
//
// Cross-module import edges are resolved correctly: the reverse-import map is
// rebuilt from scratch after all module indices are merged, so package A
// importing package B from a sibling module is captured in both directions.
//
// Individual module load failures are returned as Limitations, not as a hard
// error, so the caller can still use whatever partial index was built. A hard
// error is returned only when every discovered module fails to load.
func loadPackageGraph(ctx context.Context, repoPath string) (*packageIndex, []provider.Limitation, error) {
	moduleRoots := findModuleRoots(repoPath)

	// Accumulated merged index.
	merged := &packageIndex{
		byPkgPath:      make(map[string]*packageNode),
		byFile:         make(map[string]*packageNode),
		reverseImports: make(map[string][]string),
	}

	var allLimitations []provider.Limitation
	successCount := 0

	for _, root := range moduleRoots {
		idx, lims, err := loadSingleModuleGraph(ctx, root, repoPath)
		allLimitations = append(allLimitations, lims...)

		if err != nil {
			allLimitations = append(allLimitations, provider.Limitation{
				Kind:    "go_packages_load_failed",
				Message: fmt.Sprintf("module at %s: %v", root, err),
				Scope:   root,
			})
			continue
		}

		successCount++
		mergePackageIndex(merged, idx)
	}

	if successCount == 0 {
		return nil, allLimitations, fmt.Errorf("all %d Go module(s) failed to load", len(moduleRoots))
	}

	// Rebuild reverseImports after merging all modules so that cross-module
	// edges (e.g. a plugin importing the server module) are captured correctly.
	merged.reverseImports = buildReverseImports(merged.byPkgPath)

	return merged, allLimitations, nil
}

// loadSingleModuleGraph loads the package graph for one Go module rooted at
// moduleDir. Only packages whose directory is under repoPath are indexed;
// stdlib and external dependencies are excluded.
//
// Failure to load (e.g. go binary not found) returns a non-nil error.
// Individual package-level errors (e.g. a single file with a syntax error)
// are returned as Limitations, not a hard error, so the caller can use a
// partial index.
func loadSingleModuleGraph(ctx context.Context, moduleDir, repoPath string) (*packageIndex, []provider.Limitation, error) {
	cfg := &packages.Config{
		Context: ctx,
		// NeedName: pkg.PkgPath and pkg.Name are set.
		// NeedFiles: pkg.GoFiles (non-test .go source files) and pkg.Dir are set.
		// NeedImports: pkg.Imports map is populated with direct imports.
		// NeedDeps is intentionally omitted — loading ./... already gives us
		// GoFiles for all module packages. NeedDeps would additionally load
		// stdlib and external packages, which we neither need nor want.
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports,
		Dir:  moduleDir,
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

	// ── Build nodes for every repo-local package ──────────────────────────────
	for _, pkg := range pkgs {
		// Skip packages whose source is not under the repo root (e.g. stubs
		// injected by the go tool for missing dependencies, or stdlib).
		if pkg.PkgPath == "" || !strings.HasPrefix(pkg.Dir, repoPath) {
			continue
		}

		// Collect any package-level errors as Limitations (non-fatal).
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
			Dir:       pkg.Dir,
			GoFiles:   goFiles,
			ImportIDs: importIDs,
		}

		idx.byPkgPath[pkg.PkgPath] = node
		for _, f := range goFiles {
			idx.byFile[f] = node
		}
	}

	return idx, limitations, nil
}

// mergePackageIndex copies all entries from src into dst. When a package
// path or file path already exists in dst (from an earlier module load), the
// dst entry is preserved. The reverseImports map is intentionally left empty
// in dst — it is rebuilt once after all modules are merged via
// buildReverseImports.
func mergePackageIndex(dst, src *packageIndex) {
	for k, v := range src.byPkgPath {
		if _, exists := dst.byPkgPath[k]; !exists {
			dst.byPkgPath[k] = v
		}
	}
	for k, v := range src.byFile {
		if _, exists := dst.byFile[k]; !exists {
			dst.byFile[k] = v
		}
	}
}

// buildReverseImports constructs the reverse-import index from scratch given
// the fully-merged byPkgPath map. For every package N and every import path I
// in N.ImportIDs, it records that N imports I (i.e. I is imported by N).
// Cross-module edges are handled automatically because Go import path strings
// are canonical across modules.
func buildReverseImports(byPkgPath map[string]*packageNode) map[string][]string {
	rev := make(map[string][]string, len(byPkgPath))

	for _, node := range byPkgPath {
		for _, importID := range node.ImportIDs {
			rev[importID] = append(rev[importID], node.PkgPath)
		}
	}

	for k := range rev {
		sort.Strings(rev[k])
		rev[k] = dedupStrings(rev[k])
	}

	return rev
}

// peerFiles returns the sorted absolute paths of all other non-test .go source
// files in the same Go package as absFilePath. The file itself is excluded.
// Returns nil when absFilePath is not indexed.
func (idx *packageIndex) peerFiles(absFilePath string) []string {
	node := idx.nodeForFile(absFilePath)
	if node == nil {
		return nil
	}

	var result []string
	for _, f := range node.GoFiles {
		if f != absFilePath {
			result = append(result, f)
		}
	}
	// result is already sorted because node.GoFiles is sorted at build time.
	return result
}

// testFiles returns the sorted absolute paths of all *_test.go files that live
// in the same directory as absFilePath. The Go spec mandates that test files
// for a package reside in the package directory — this is spec, not heuristic.
// Returns nil when absFilePath is not indexed or the package has no test files.
func (idx *packageIndex) testFiles(absFilePath string) []string {
	node := idx.nodeForFile(absFilePath)
	if node == nil || node.Dir == "" {
		return nil
	}

	// Read the package directory for *_test.go files. This is a single
	// directory read — fast and authoritative (Go spec §10.3).
	entries, err := os.ReadDir(node.Dir)
	if err != nil {
		return nil
	}

	var result []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
			result = append(result, filepath.Join(node.Dir, e.Name()))
		}
	}
	sort.Strings(result)
	return result
}

// dedupStrings returns a deduplicated copy of a sorted string slice. The
// input must already be sorted. Returns nil for empty input.
func dedupStrings(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	out := ss[:1]
	for i := 1; i < len(ss); i++ {
		if ss[i] != ss[i-1] {
			out = append(out, ss[i])
		}
	}
	return out
}

// nodeForFile returns the packageNode that owns absFilePath. It checks both
// source files (byFile) and the caller's own directory scan for test files.
// Returns nil when absFilePath is not indexed.
func (idx *packageIndex) nodeForFile(absFilePath string) *packageNode {
	if n := idx.byFile[absFilePath]; n != nil {
		return n
	}
	// absFilePath may itself be a *_test.go file — look up by directory.
	if strings.HasSuffix(absFilePath, "_test.go") {
		dir := filepath.Dir(absFilePath)
		for _, node := range idx.byFile {
			if node.Dir == dir {
				return node
			}
		}
	}
	return nil
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
			// stdlib or external dep — not in our repo-local index.
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
