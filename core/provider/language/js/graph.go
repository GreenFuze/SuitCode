package jsprovider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/GreenFuze/SuitCode/core/provider"
)

// jsImportIndex is the in-memory bidirectional import graph for a JS/TS repo.
// All maps use absolute file paths as keys. Immutable after construction.
type jsImportIndex struct {
	// fileImports maps abs file → sorted abs file paths it directly imports.
	fileImports map[string][]string

	// fileImporters maps abs file → sorted abs file paths that directly import it.
	fileImporters map[string][]string

	// sourceFileCount is the number of JS/TS source files found during the walk.
	sourceFileCount int
}

// JS/TS file extensions the provider indexes.
var jsExtensions = map[string]bool{
	".ts": true, ".tsx": true,
	".js": true, ".jsx": true,
	".mts": true, ".cts": true,
	".mjs": true, ".cjs": true,
}

// Directories to skip during the file walk.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	".claude":      true, // Claude Code worktrees — must not be indexed
	"dist":         true,
	"build":        true,
	"out":          true,
	".next":        true,
	".nuxt":        true,
	".svelte-kit":  true,
	"coverage":     true,
	".turbo":       true,
	".cache":       true,
}

var (
	// ES6 static import: import X from './module', import './side-effect',
	// import type X from './module', import '@/module' (alias), etc.
	// Captures relative paths (starting with .) and common alias prefixes (@, ~, #).
	reImportFrom = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?(?:[^'"` + "`" + `]*?\s+from\s+)?['"]([.@~#][^'"` + "`" + `]+)['"]`)

	// CommonJS / dynamic require: require('./module') or require('@/module')
	reRequire = regexp.MustCompile(`require\s*\(\s*['"]([.@~#][^'"]+)['"]\s*\)`)

	// Re-exports: export { X } from './module', export * from '@/module'
	reExportFrom = regexp.MustCompile(`(?m)^\s*export\s+(?:\*|\{[^}]*\})\s+from\s+['"]([.@~#][^'"]+)['"]`)
)

// buildImportGraph walks repoPath, parses JS/TS source files, and builds the
// bidirectional import graph. Relative imports and tsconfig path-alias imports
// are both resolved; bare specifiers (npm packages) are ignored.
func buildImportGraph(repoPath string) (*jsImportIndex, []provider.Limitation, error) {
	// Load tsconfig path aliases once for the whole repo walk.
	// This enables resolution of "@/*", "~/*", and other non-relative imports.
	aliases := loadAllTSConfigAliases(repoPath)

	// Collect all JS/TS source files, excluding generated/vendor directories.
	var sourceFiles []string
	err := filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		if jsExtensions[strings.ToLower(filepath.Ext(path))] {
			sourceFiles = append(sourceFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walking repo: %w", err)
	}

	idx := &jsImportIndex{
		fileImports:     make(map[string][]string),
		fileImporters:   make(map[string][]string),
		sourceFileCount: len(sourceFiles),
	}

	if len(sourceFiles) == 0 {
		return idx, []provider.Limitation{{
			Kind:    "no_js_ts_files",
			Message: "no JavaScript or TypeScript source files found",
			Scope:   repoPath,
		}}, nil
	}

	var limitations []provider.Limitation

	for _, src := range sourceFiles {
		specifiers, lim := parseImportSpecifiers(src)
		if lim != nil {
			limitations = append(limitations, *lim)
			continue
		}

		var resolved []string
		for _, spec := range specifiers {
			if target := resolveSpecifier(src, spec, repoPath, aliases); target != "" {
				resolved = append(resolved, target)
				idx.fileImporters[target] = append(idx.fileImporters[target], src)
			}
		}

		// Deduplicate and sort for determinism.
		idx.fileImports[src] = dedup(resolved)
	}

	// Sort all importer lists for determinism.
	for k := range idx.fileImporters {
		idx.fileImporters[k] = dedup(idx.fileImporters[k])
	}

	return idx, limitations, nil
}

// parseImportSpecifiers reads a JS/TS file and returns all import specifiers
// that are candidates for local resolution: relative paths (starting with .)
// and common alias prefixes (@, ~, #).
// Large files (>1 MB) are skipped with a Limitation rather than a hard error.
func parseImportSpecifiers(filePath string) ([]string, *provider.Limitation) {
	const maxBytes = 1 << 20 // 1 MB

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, &provider.Limitation{
			Kind:    "file_unreadable",
			Message: fmt.Sprintf("stat %s: %v", filePath, err),
			Scope:   filePath,
		}
	}
	if info.Size() > maxBytes {
		return nil, &provider.Limitation{
			Kind:    "file_too_large",
			Message: fmt.Sprintf("%s exceeds 1 MB — likely a generated bundle; skipping", filePath),
			Scope:   filePath,
		}
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, &provider.Limitation{
			Kind:    "file_unreadable",
			Message: fmt.Sprintf("read %s: %v", filePath, err),
			Scope:   filePath,
		}
	}

	// Collect unique specifiers from all import forms.
	seen := make(map[string]bool)
	var specifiers []string

	for _, re := range []*regexp.Regexp{reImportFrom, reRequire, reExportFrom} {
		for _, m := range re.FindAllSubmatch(data, -1) {
			if len(m) >= 2 {
				spec := string(m[1])
				if !seen[spec] {
					seen[spec] = true
					specifiers = append(specifiers, spec)
				}
			}
		}
	}

	return specifiers, nil
}

// resolveSpecifier resolves an import specifier from fromFile and returns the
// absolute path of the target file. Returns "" when the target cannot be found.
//
// Two resolution strategies are applied in order:
//  1. Relative specifiers (starting with "."): resolved relative to fromFile's directory.
//  2. Alias specifiers (e.g. "@/foo"): expanded via tsconfig path aliases then probed.
//
// Bare specifiers ("react", "lodash") always return "".
func resolveSpecifier(fromFile, specifier, repoPath string, aliases tsConfigAliases) string {
	var candidate string

	if strings.HasPrefix(specifier, ".") {
		// Relative import — resolve from the file's own directory.
		base := filepath.Dir(fromFile)
		candidate = filepath.Clean(filepath.Join(base, filepath.FromSlash(specifier)))
	} else {
		// Non-relative — try tsconfig alias expansion.
		// If no alias matches, this is a bare npm package name; skip it.
		if len(aliases) == 0 {
			return ""
		}
		expanded := aliases.resolve(specifier)
		if expanded == "" {
			return ""
		}
		candidate = expanded
	}

	return probeCandidate(candidate, repoPath)
}

// probeCandidate checks whether candidate (which may lack an extension) resolves
// to an existing JS/TS file in the repo. The candidate is tried as-is first,
// then with common source extensions appended, then as a directory index file.
func probeCandidate(candidate, repoPath string) string {
	// Guard against path traversal outside the repo.
	if !isUnderRepo(candidate, repoPath) {
		return ""
	}

	// If the candidate already carries a known JS/TS extension, check directly.
	if jsExtensions[strings.ToLower(filepath.Ext(candidate))] {
		if fileExistsAt(candidate) {
			return candidate
		}
		return ""
	}

	// Try appending common source extensions.
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mts", ".cts"} {
		if p := candidate + ext; fileExistsAt(p) {
			return p
		}
	}

	// Try as a directory with an index file.
	for _, name := range []string{"index.ts", "index.tsx", "index.js", "index.jsx"} {
		if p := filepath.Join(candidate, name); fileExistsAt(p) {
			return p
		}
	}

	return ""
}

// fileExistsAt reports whether a regular file exists at the given path.
func fileExistsAt(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// isUnderRepo reports whether path is inside repoPath (no "../.." escape).
func isUnderRepo(path, repoPath string) bool {
	rel, err := filepath.Rel(repoPath, path)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// dedup returns a sorted, deduplicated copy of ss. Returns nil for empty input.
func dedup(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
