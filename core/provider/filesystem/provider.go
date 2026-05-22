// Package filesystem provides a FilesystemProvider implementation that walks a
// repository directory, classifies files by language and role, detects build
// and test systems, and respects .gitignore patterns.
package filesystem

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

const id provider.ProviderID = "filesystem"

// languageByExt maps lowercase file extensions to display language names.
var languageByExt = map[string]string{
	".go":     "Go",
	".py":     "Python",
	".ts":     "TypeScript",
	".tsx":    "TypeScript",
	".js":     "JavaScript",
	".jsx":    "JavaScript",
	".mjs":    "JavaScript",
	".rs":     "Rust",
	".java":   "Java",
	".cs":     "C#",
	".cpp":    "C++",
	".cc":     "C++",
	".cxx":    "C++",
	".c":      "C",
	".h":      "C/C++",
	".hpp":    "C++",
	".rb":     "Ruby",
	".kt":     "Kotlin",
	".kts":    "Kotlin",
	".swift":  "Swift",
	".php":    "PHP",
	".scala":  "Scala",
	".hs":     "Haskell",
	".ex":     "Elixir",
	".exs":    "Elixir",
	".clj":    "Clojure",
	".ml":     "OCaml",
	".mli":    "OCaml",
	".fs":     "F#",
	".fsx":    "F#",
	".lua":    "Lua",
	".r":      "R",
	".dart":   "Dart",
	".md":     "Markdown",
	".mdx":    "Markdown",
	".json":   "JSON",
	".yaml":   "YAML",
	".yml":    "YAML",
	".toml":   "TOML",
	".xml":    "XML",
	".html":   "HTML",
	".htm":    "HTML",
	".css":    "CSS",
	".scss":   "SCSS",
	".sass":   "SCSS",
	".sql":    "SQL",
	".sh":     "Shell",
	".bash":   "Shell",
	".zsh":    "Shell",
	".fish":   "Shell",
	".ps1":    "PowerShell",
	".proto":  "Protobuf",
	".tf":     "Terraform",
	".tfvars": "Terraform",
}

// buildSystemMarkers maps build-system display names to the files that signal
// their presence. Files are matched against the repository root only.
var buildSystemMarkers = map[string][]string{
	"Go Modules":     {"go.mod"},
	"npm":            {"package.json"},
	"Cargo":          {"Cargo.toml"},
	"Maven":          {"pom.xml"},
	"Gradle":         {"build.gradle", "build.gradle.kts"},
	"Make":           {"Makefile", "makefile", "GNUmakefile"},
	"CMake":          {"CMakeLists.txt"},
	"Bazel":          {"BUILD", "WORKSPACE", "MODULE.bazel"},
	"Poetry":         {"pyproject.toml"},
	"Pipenv":         {"Pipfile"},
	"pip":            {"requirements.txt", "setup.py", "setup.cfg"},
	"Bundler":        {"Gemfile"},
	"Composer":       {"composer.json"},
	"Mix":            {"mix.exs"},
	"sbt":            {"build.sbt"},
	"Docker":         {"Dockerfile"},
	"Docker Compose": {"docker-compose.yml", "docker-compose.yaml"},
}

// testSystemMarkers maps test-framework names to indicator files/patterns at
// the repository root.
var testSystemMarkers = map[string][]string{
	"pytest":  {"pytest.ini", "pyproject.toml", "setup.cfg"},
	"Jest":    {"jest.config.js", "jest.config.ts", "jest.config.mjs"},
	"Vitest":  {"vitest.config.ts", "vitest.config.js"},
	"RSpec":   {".rspec"},
	"Mocha":   {".mocharc.js", ".mocharc.yml", ".mocharc.yaml"},
	"PHPUnit": {"phpunit.xml", "phpunit.xml.dist"},
}

// alwaysSkipDirs are directory names that are always excluded from walking.
var alwaysSkipDirs = map[string]bool{
	".git":          true,
	".hg":           true,
	".svn":          true,
	"node_modules":  true,
	"vendor":        true,
	".suitcode":     true,
	".suit":         true,
	"__pycache__":   true,
	".mypy_cache":   true,
	".pytest_cache": true,
	".ruff_cache":   true,
	"dist":          true,
	".dist":         true,
	"build":         true,
	".build":        true,
	"target":        true,
	".idea":         true,
	".vscode":       true,
	".DS_Store":     true,
}

// Provider implements provider.FilesystemProvider.
type Provider struct {
	repoPath string
	ready    bool
}

// NewFilesystemProvider creates a FilesystemProvider that is fully initialised
// and ready to answer queries for the given repository root. An error is
// returned when repoPath does not exist or is not a directory.
func NewFilesystemProvider(_ context.Context, repoPath string) (*Provider, error) {
	info, err := os.Stat(repoPath)
	if err != nil {
		return nil, fmt.Errorf("filesystem provider: cannot stat %q: %w", repoPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("filesystem provider: %q is not a directory", repoPath)
	}
	return &Provider{repoPath: repoPath, ready: true}, nil
}

// Capabilities satisfies provider.Provider.
func (p *Provider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		ID:          id,
		DisplayName: "Filesystem Provider",
		Roles:       []provider.ProviderRole{provider.RoleFilesystem},
	}
}

// Ready reports whether the provider is attached and usable.
func (p *Provider) Ready() bool { return p.ready }

// Close is a no-op for the filesystem provider.
func (p *Provider) Close() error { return nil }

// ListFiles walks the repository, classifies every non-ignored file, and
// returns a FilesystemListing with language and build-system metadata.
func (p *Provider) ListFiles(ctx context.Context) (*provider.ProviderResult[provider.FilesystemListing], error) {
	if !p.ready {
		return nil, fmt.Errorf("filesystem provider: not attached — call Attach first")
	}

	start := time.Now()

	// Load .gitignore patterns from the repository root.
	ignorePatterns := p.loadGitignore()

	var files []provider.FilesystemFile
	langCount := make(map[string]int)
	totalDirs := 0

	// Collect root-level file names for build/test system detection.
	rootFiles := make(map[string]bool)

	walkErr := filepath.WalkDir(p.repoPath, func(path string, d os.DirEntry, err error) error {
		// Check context cancellation on every entry.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err != nil {
			// Skip entries we cannot read rather than aborting the whole walk.
			return nil
		}

		relPath, _ := filepath.Rel(p.repoPath, path)
		relPath = filepath.ToSlash(relPath)

		// Skip the root itself.
		if relPath == "." {
			return nil
		}

		if d.IsDir() {
			// Skip always-ignored directories by base name.
			if alwaysSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			// Skip directories that match .gitignore patterns.
			if matchesAnyPattern(relPath+"/", ignorePatterns) {
				return filepath.SkipDir
			}
			totalDirs++
			return nil
		}

		// Skip files that match .gitignore patterns.
		if matchesAnyPattern(relPath, ignorePatterns) {
			return nil
		}

		// Track files directly at the repository root for system detection.
		if !strings.Contains(relPath, "/") {
			rootFiles[d.Name()] = true
		}

		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		lang := languageByExt[ext]
		role := classifyFile(relPath, d.Name(), lang)

		if lang != "" {
			langCount[lang]++
		}

		files = append(files, provider.FilesystemFile{
			Path:     path,
			RelPath:  relPath,
			Size:     info.Size(),
			Language: lang,
			Role:     role,
		})

		return nil
	})

	if walkErr != nil {
		return nil, fmt.Errorf("filesystem provider: walking %q: %w", p.repoPath, walkErr)
	}

	buildSystems := detectSystems(rootFiles, buildSystemMarkers)
	testSystems := detectSystems(rootFiles, testSystemMarkers)

	// Go test is detected by file suffix, not a root-level marker.
	for _, f := range files {
		if strings.HasSuffix(f.RelPath, "_test.go") {
			testSystems = appendUnique(testSystems, "Go test")
			break
		}
	}

	listing := provider.FilesystemListing{
		Files:        files,
		TotalFiles:   len(files),
		TotalDirs:    totalDirs,
		Languages:    sortedByFrequency(langCount),
		BuildSystems: buildSystems,
		TestSystems:  testSystems,
		IgnoredPaths: ignorePatterns,
	}

	return &provider.ProviderResult[provider.FilesystemListing]{
		Data: listing,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindFilesystem,
			SourceTool:      "filepath.WalkDir",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("walked %d files in %s", len(files), p.repoPath),
			EvidencePaths:   []string{p.repoPath},
		}},
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// loadGitignore reads .gitignore at the repository root and returns a slice of
// trimmed, non-comment patterns. Missing or unreadable .gitignore is silently
// ignored.
func (p *Provider) loadGitignore() []string {
	path := filepath.Join(p.repoPath, ".gitignore")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// matchesAnyPattern returns true when relPath matches any gitignore-style
// pattern. Only basic prefix and suffix matching is implemented in v1.
func matchesAnyPattern(relPath string, patterns []string) bool {
	for _, pat := range patterns {
		if matchPattern(relPath, pat) {
			return true
		}
	}
	return false
}

// matchPattern performs simplified gitignore matching.
func matchPattern(relPath, pattern string) bool {
	// Normalise pattern separators.
	pattern = filepath.ToSlash(pattern)

	// Trailing slash means directory-only; we already append "/" to dirs in
	// the walker, so we can match directly.
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(relPath, pattern) ||
			strings.Contains(relPath, "/"+pattern)
	}

	// Pattern without slash: match basename anywhere in the tree.
	if !strings.Contains(pattern, "/") {
		base := relPath
		if idx := strings.LastIndex(relPath, "/"); idx >= 0 {
			base = relPath[idx+1:]
		}
		return matchSimpleGlob(base, pattern)
	}

	// Pattern with leading slash: anchored to repo root.
	if strings.HasPrefix(pattern, "/") {
		return matchSimpleGlob(relPath, strings.TrimPrefix(pattern, "/"))
	}

	// Pattern with internal slash: match relative path or suffix.
	return matchSimpleGlob(relPath, pattern) ||
		strings.HasSuffix(relPath, "/"+pattern)
}

// matchSimpleGlob handles only "*" wildcards (no "**" or "?" in v1).
func matchSimpleGlob(s, pattern string) bool {
	if !strings.Contains(pattern, "*") {
		return s == pattern
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 2 {
		return strings.HasPrefix(s, parts[0]) && strings.HasSuffix(s, parts[1])
	}
	// Multi-wildcard: fall back to simple prefix match for v1.
	return strings.HasPrefix(s, parts[0])
}

// classifyFile assigns a role string based on file path and language.
func classifyFile(relPath, name, lang string) string {
	lower := strings.ToLower(relPath)

	// Generated file indicators.
	if strings.Contains(lower, "generated") ||
		strings.Contains(lower, ".gen.") ||
		strings.HasSuffix(lower, ".pb.go") ||
		strings.HasSuffix(lower, "_gen.go") ||
		strings.HasSuffix(lower, ".generated.ts") {
		return "generated"
	}

	// Vendor directories.
	if strings.HasPrefix(lower, "vendor/") ||
		strings.Contains(lower, "/vendor/") {
		return "vendor"
	}

	// Test files.
	if strings.HasSuffix(lower, "_test.go") ||
		strings.HasPrefix(name, "test_") ||
		strings.HasSuffix(lower, ".test.ts") ||
		strings.HasSuffix(lower, ".spec.ts") ||
		strings.HasSuffix(lower, ".test.js") ||
		strings.HasSuffix(lower, ".spec.js") ||
		strings.Contains(lower, "/test/") ||
		strings.Contains(lower, "/tests/") ||
		strings.Contains(lower, "/spec/") ||
		strings.Contains(lower, "/__tests__/") {
		return "test"
	}

	// Documentation.
	if lang == "Markdown" ||
		strings.Contains(lower, "/docs/") ||
		strings.Contains(lower, "/doc/") ||
		name == "README" || name == "CHANGELOG" || name == "LICENSE" {
		return "docs"
	}

	// Configuration files.
	if lang == "YAML" || lang == "TOML" || lang == "JSON" ||
		strings.HasSuffix(lower, ".env") ||
		strings.HasPrefix(name, ".") ||
		name == "Makefile" || name == "Dockerfile" {
		return "config"
	}

	if lang != "" {
		return "source"
	}
	return "other"
}

// detectSystems checks rootFiles for the presence of known marker files and
// returns the names of matched systems.
func detectSystems(rootFiles map[string]bool, markers map[string][]string) []string {
	var found []string
	for sysName, markerFiles := range markers {
		for _, m := range markerFiles {
			if rootFiles[m] {
				found = appendUnique(found, sysName)
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

// sortedByFrequency returns the keys of m sorted by their count, descending.
func sortedByFrequency(m map[string]int) []string {
	type kv struct {
		k string
		v int
	}
	var pairs []kv
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k
	}
	return out
}

// appendUnique appends s to slice only if it is not already present.
func appendUnique(slice []string, s string) []string {
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}
