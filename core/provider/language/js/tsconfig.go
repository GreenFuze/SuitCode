package jsprovider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// tsConfigAlias represents one resolved path-alias entry from a tsconfig.json.
//
// Two forms are supported:
//
//   - Wildcard: key="@/*", value="./src/*"
//     → prefix="@/", baseDir="/abs/repo/src"
//     → "@/hooks/useGame" resolves to "/abs/repo/src/hooks/useGame" (+ ext probe)
//
//   - Exact: key="@utils", value="./src/utils/index"
//     → prefix="@utils", exactTarget="/abs/repo/src/utils/index"
//     → "@utils" resolves to "/abs/repo/src/utils/index" (+ ext probe)
type tsConfigAlias struct {
	// prefix is the alias string (without trailing wildcard), e.g. "@/" or "@utils".
	prefix string

	// baseDir is the absolute directory to join with the suffix (wildcard aliases).
	// Empty for exact aliases.
	baseDir string

	// exactTarget is the absolute target path for exact (non-wildcard) aliases.
	exactTarget string

	// exact is true when the key had no wildcard.
	exact bool
}

// tsConfigAliases is a slice of resolved alias entries.
type tsConfigAliases []tsConfigAlias

// reLineComment removes C-style single-line comments so JSONC tsconfig files
// parse correctly with encoding/json.
var reLineComment = regexp.MustCompile(`(?m)//[^\r\n]*`)

// tsConfigJSON is the minimal structure we need from tsconfig.json.
type tsConfigJSON struct {
	Extends         string `json:"extends"`
	CompilerOptions struct {
		BaseURL string              `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
}

// loadAllTSConfigs walks repoPath looking for tsconfig.json files and returns
// the merged aliases and baseURL directories from all of them (including any
// configs they extend). Directories in skipDirs and paths deeper than 6
// levels are skipped to keep the search bounded.
func loadAllTSConfigs(repoPath string) (tsConfigAliases, []string) {
	var allAliases tsConfigAliases
	var allBaseURLs []string
	visited := make(map[string]bool)

	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(repoPath, path)
			if strings.Count(filepath.ToSlash(rel), "/") >= 6 {
				return filepath.SkipDir
			}
			return nil
		}

		if !strings.EqualFold(d.Name(), "tsconfig.json") {
			return nil
		}

		aliases, baseURLs := parseTSConfigChain(path, repoPath, visited)
		allAliases = append(allAliases, aliases...)
		allBaseURLs = append(allBaseURLs, baseURLs...)
		return nil
	})

	return allAliases, allBaseURLs
}

// parseTSConfigChain parses a tsconfig.json and any configs it extends,
// returning the merged aliases and baseURL directories. visited prevents
// infinite loops from circular extends.
func parseTSConfigChain(tsconfigPath, repoPath string, visited map[string]bool) (tsConfigAliases, []string) {
	abs, err := filepath.Abs(tsconfigPath)
	if err != nil || visited[abs] {
		return nil, nil
	}
	visited[abs] = true

	aliases, baseURL, extendsPath := parseTSConfigFile(abs, repoPath)

	// Recurse into extended config first so that the child's settings take
	// precedence (they are appended last and checked first by resolve()).
	var parentAliases tsConfigAliases
	var parentBaseURLs []string
	if extendsPath != "" {
		parentAliases, parentBaseURLs = parseTSConfigChain(extendsPath, repoPath, visited)
	}

	var baseURLs []string
	if baseURL != "" {
		baseURLs = append(parentBaseURLs, baseURL)
	} else {
		baseURLs = parentBaseURLs
	}

	return append(parentAliases, aliases...), baseURLs
}

// parseTSConfigFile reads one tsconfig.json and returns:
//   - its path aliases resolved to absolute paths,
//   - the resolved absolute baseUrl directory (empty string if not set),
//   - the resolved absolute path of any "extends" target (empty string if none).
func parseTSConfigFile(tsconfigPath, repoPath string) (tsConfigAliases, string, string) {
	data, err := os.ReadFile(tsconfigPath)
	if err != nil {
		return nil, "", ""
	}

	// Strip single-line comments to handle JSONC-formatted tsconfig files.
	clean := reLineComment.ReplaceAll(data, nil)

	var cfg tsConfigJSON
	if err := json.Unmarshal(clean, &cfg); err != nil {
		return nil, "", ""
	}

	tsconfigDir := filepath.Dir(tsconfigPath)

	// Resolve baseUrl relative to the tsconfig.json's own directory.
	absBaseURL := ""
	if cfg.CompilerOptions.BaseURL != "" {
		absBaseURL = filepath.Clean(filepath.Join(tsconfigDir, filepath.FromSlash(cfg.CompilerOptions.BaseURL)))
		if !isUnderRepo(absBaseURL, repoPath) {
			absBaseURL = ""
		}
	}

	// Resolve "extends" relative to the tsconfig.json's own directory.
	extendsPath := ""
	if cfg.Extends != "" {
		candidate := filepath.Clean(filepath.Join(tsconfigDir, filepath.FromSlash(cfg.Extends)))
		// extends may or may not include the .json extension.
		if !strings.HasSuffix(strings.ToLower(candidate), ".json") {
			candidate += ".json"
		}
		if isUnderRepo(candidate, repoPath) {
			extendsPath = candidate
		}
	}

	if len(cfg.CompilerOptions.Paths) == 0 {
		return nil, absBaseURL, extendsPath
	}

	// Use baseUrl as the base for resolving path values; fall back to the
	// tsconfig's own directory if baseUrl is not set.
	pathBase := tsconfigDir
	if absBaseURL != "" {
		pathBase = absBaseURL
	}

	var result tsConfigAliases

	for key, values := range cfg.CompilerOptions.Paths {
		if len(values) == 0 {
			continue
		}

		// Use the first listed target (TypeScript uses all targets as a fallback
		// chain; the first is the primary and covers the overwhelming majority of
		// real projects).
		value := values[0]

		keyHasWild := strings.HasSuffix(key, "/*")
		valHasWild := strings.HasSuffix(value, "/*")

		if keyHasWild && valHasWild {
			// Wildcard alias: "@/*" → ["./src/*"]
			prefix := strings.TrimSuffix(key, "*") // "@/"
			valBase := strings.TrimSuffix(value, "/*")
			absBase := filepath.Clean(filepath.Join(pathBase, filepath.FromSlash(valBase)))

			if !isUnderRepo(absBase, repoPath) {
				continue
			}

			result = append(result, tsConfigAlias{
				prefix:  prefix,
				baseDir: absBase,
			})
		} else if !keyHasWild {
			// Exact alias: "@utils" → ["./src/utils/index"]
			absTarget := filepath.Clean(filepath.Join(pathBase, filepath.FromSlash(value)))
			result = append(result, tsConfigAlias{
				prefix:      key,
				exact:       true,
				exactTarget: absTarget,
			})
		}
		// Mixed (key has wildcard but value does not, or vice versa) — rare and
		// ambiguous; skip.
	}

	return result, absBaseURL, extendsPath
}

// resolve tries to expand specifier against the alias entries.
// Returns the absolute candidate path (without extension) if an alias matched,
// or "" if no alias applies (meaning the specifier is a bare npm package name).
func (aliases tsConfigAliases) resolve(specifier string) string {
	for _, a := range aliases {
		if a.exact {
			if specifier == a.prefix {
				return a.exactTarget
			}
			continue
		}

		// Wildcard: the specifier must start with the prefix (e.g. "@/").
		if !strings.HasPrefix(specifier, a.prefix) {
			continue
		}

		suffix := specifier[len(a.prefix):]
		return filepath.Join(a.baseDir, filepath.FromSlash(suffix))
	}
	return ""
}
