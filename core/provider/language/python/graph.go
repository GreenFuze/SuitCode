package pyprovider

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/GreenFuze/SuitCode/core/provider"
)

// pyImportIndex is the in-memory bidirectional import graph for a Python repo.
// All maps use absolute file paths as keys. Immutable after construction.
type pyImportIndex struct {
	// fileImports maps abs file → sorted abs file paths it directly imports.
	fileImports map[string][]string

	// fileImporters maps abs file → sorted abs file paths that directly import it.
	fileImporters map[string][]string

	// sourceFileCount is the number of .py source files found during the walk.
	sourceFileCount int

	// moduleMap maps a dotted module path (e.g. "foo.bar") to the absolute file
	// path (e.g. "/repo/foo/bar.py"). Used when resolving absolute imports.
	moduleMap map[string]string
}

// Directories to skip when walking the repo.
var pySkipDirs = map[string]bool{
	".git":          true,
	".claude":       true, // Claude Code worktrees — must not be indexed
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	"env":           true,
	".env":          true,
	".tox":          true,
	"dist":          true,
	"build":         true,
	".pytest_cache": true,
	"site-packages": true,
	"node_modules":  true,
}

var (
	// import foo, import foo.bar, import foo as x, import foo.bar as x
	reImport = regexp.MustCompile(`^(?:import)\s+([\w.]+)`)

	// from foo import bar, from foo.bar import baz, from . import foo, from ..foo import bar
	reFromImport = regexp.MustCompile(`^from\s+(\.{0,3}[\w.]*)\s+import\s+`)
)

// buildPythonImportGraph walks repoPath, parses Python import statements, and
// builds the bidirectional import graph.
//
// The approach uses heuristic resolution: the repo root and any src/ / lib/
// subdirectory are treated as Python path roots. Absolute imports are matched
// against the discovered module map; relative imports (starting with ".")
// are resolved from the current file's package directory.
func buildPythonImportGraph(repoPath string) (*pyImportIndex, []provider.Limitation, error) {
	// ── Step 1: walk to collect source files and build the module map ─────────
	var sourceFiles []string
	moduleMap := make(map[string]string) // dotted module name → abs file path

	err := filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if pySkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		if strings.ToLower(filepath.Ext(path)) != ".py" {
			return nil
		}

		sourceFiles = append(sourceFiles, path)

		// Derive the dotted module name from the file's path relative to repoPath.
		// foo/bar/baz.py  → foo.bar.baz
		// foo/bar/__init__.py → foo.bar
		rel, _ := filepath.Rel(repoPath, path)
		mod := pathToModule(rel)
		if mod != "" {
			moduleMap[mod] = path
		}

		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walking repo: %w", err)
	}

	idx := &pyImportIndex{
		fileImports:     make(map[string][]string),
		fileImporters:   make(map[string][]string),
		sourceFileCount: len(sourceFiles),
		moduleMap:       moduleMap,
	}

	if len(sourceFiles) == 0 {
		return idx, []provider.Limitation{{
			Kind:    "no_python_files",
			Message: "no Python source files found",
			Scope:   repoPath,
		}}, nil
	}

	// ── Step 2: parse imports for each file and build the graph ───────────────
	var limitations []provider.Limitation

	for _, src := range sourceFiles {
		targets, lim := resolveFileImports(src, repoPath, moduleMap)
		if lim != nil {
			limitations = append(limitations, *lim)
			continue
		}

		// Record forward edges.
		idx.fileImports[src] = dedup(targets)

		// Record reverse edges.
		for _, t := range targets {
			idx.fileImporters[t] = append(idx.fileImporters[t], src)
		}
	}

	// Sort all importer lists for determinism.
	for k := range idx.fileImporters {
		idx.fileImporters[k] = dedup(idx.fileImporters[k])
	}

	return idx, limitations, nil
}

// resolveFileImports parses srcFile and resolves each import to an abs path.
// Returns nil lim on success; a Limitation on read/parse error (not fatal).
func resolveFileImports(srcFile, repoPath string, moduleMap map[string]string) ([]string, *provider.Limitation) {
	const maxBytes = 512 * 1024 // 512 KB — Python files are rarely larger

	info, err := os.Stat(srcFile)
	if err != nil {
		return nil, &provider.Limitation{Kind: "file_unreadable", Message: err.Error(), Scope: srcFile}
	}
	if info.Size() > maxBytes {
		return nil, &provider.Limitation{
			Kind:    "file_too_large",
			Message: fmt.Sprintf("%s exceeds 512 KB — skipping", srcFile),
			Scope:   srcFile,
		}
	}

	f, err := os.Open(srcFile)
	if err != nil {
		return nil, &provider.Limitation{Kind: "file_unreadable", Message: err.Error(), Scope: srcFile}
	}
	defer f.Close()

	var targets []string
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and blank lines early.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// "import foo" / "import foo.bar as x"
		if m := reImport.FindStringSubmatch(line); m != nil {
			modName := m[1]
			if t := moduleMap[modName]; t != "" {
				targets = append(targets, t)
			}
			// Also try each prefix: "import foo.bar" → look up "foo" and "foo.bar".
			parts := strings.Split(modName, ".")
			for i := 1; i <= len(parts); i++ {
				prefix := strings.Join(parts[:i], ".")
				if t := moduleMap[prefix]; t != "" {
					targets = append(targets, t)
				}
			}
			continue
		}

		// "from foo import bar" / "from .module import X" / "from .. import Y"
		if m := reFromImport.FindStringSubmatch(line); m != nil {
			spec := m[1]

			if strings.HasPrefix(spec, ".") {
				// Relative import.
				if t := resolveRelativeImport(srcFile, repoPath, spec); t != "" {
					targets = append(targets, t)
				}
			} else if spec != "" {
				// Absolute import.
				if t := moduleMap[spec]; t != "" {
					targets = append(targets, t)
				}
				// Try prefixes too.
				parts := strings.Split(spec, ".")
				for i := 1; i <= len(parts); i++ {
					prefix := strings.Join(parts[:i], ".")
					if t := moduleMap[prefix]; t != "" {
						targets = append(targets, t)
					}
				}
			}
		}
	}

	return targets, nil
}

// resolveRelativeImport resolves "from .foo import X" style imports.
// spec examples: ".", "..", ".foo", "..bar", "...baz"
// Returns the abs path of the resolved module file, or "" if not found.
func resolveRelativeImport(srcFile, repoPath, spec string) string {
	// Count leading dots to determine how many package levels to go up.
	dots := 0
	for dots < len(spec) && spec[dots] == '.' {
		dots++
	}
	modSuffix := spec[dots:] // e.g. "" for ".", "foo" for ".foo", "bar" for "..bar"

	// Start from the directory containing srcFile; go up (dots-1) more levels.
	// One dot = same package (same directory); two dots = parent package.
	dir := filepath.Dir(srcFile)
	for i := 1; i < dots; i++ {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // at filesystem root
		}
		dir = parent
	}

	// Guard: resolved dir must still be inside the repo.
	if rel, err := filepath.Rel(repoPath, dir); err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}

	if modSuffix == "" {
		// "from . import X" — the package is the current dir's __init__.py
		p := filepath.Join(dir, "__init__.py")
		if fileExistsAt(p) {
			return p
		}
		return ""
	}

	// "from .foo import X" → look for foo.py or foo/__init__.py in dir.
	parts := strings.Split(modSuffix, ".")
	candidate := filepath.Join(append([]string{dir}, parts...)...)

	if p := candidate + ".py"; fileExistsAt(p) {
		return p
	}
	if p := filepath.Join(candidate, "__init__.py"); fileExistsAt(p) {
		return p
	}
	return ""
}

// pathToModule converts a relative file path (using OS separator) to a dotted
// Python module name.
//
// Examples:
//
//	foo/bar/baz.py      → foo.bar.baz
//	foo/bar/__init__.py → foo.bar
//	script.py           → script
func pathToModule(relPath string) string {
	relPath = filepath.ToSlash(relPath)
	relPath = strings.TrimSuffix(relPath, ".py")

	// __init__ → the package itself
	parts := strings.Split(relPath, "/")
	if len(parts) > 0 && parts[len(parts)-1] == "__init__" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ".")
}

// fileExistsAt reports whether a regular file exists at the given path.
func fileExistsAt(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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
