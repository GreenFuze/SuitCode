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
	CompilerOptions struct {
		BaseURL string              `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
}

// loadAllTSConfigAliases walks repoPath looking for tsconfig.json files and
// returns the merged set of alias entries from all of them.
// Directories in skipDirs (node_modules, dist, etc.) and paths deeper than
// 6 levels are skipped to keep the search bounded.
func loadAllTSConfigAliases(repoPath string) tsConfigAliases {
	var all tsConfigAliases

	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if d.IsDir() {
			// Respect the same skip list as the import graph walker.
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}

			// Don't recurse beyond 6 directory levels to stay fast.
			rel, _ := filepath.Rel(repoPath, path)
			if strings.Count(filepath.ToSlash(rel), "/") >= 6 {
				return filepath.SkipDir
			}
			return nil
		}

		if !strings.EqualFold(d.Name(), "tsconfig.json") {
			return nil
		}

		all = append(all, parseTSConfigAliases(path, repoPath)...)
		return nil
	})

	return all
}

// parseTSConfigAliases reads and parses a single tsconfig.json file and
// returns its alias entries with all paths resolved to absolute locations.
func parseTSConfigAliases(tsconfigPath, repoPath string) tsConfigAliases {
	data, err := os.ReadFile(tsconfigPath)
	if err != nil {
		return nil
	}

	// Strip single-line comments to handle JSONC-formatted tsconfig files.
	clean := reLineComment.ReplaceAll(data, nil)

	var cfg tsConfigJSON
	if err := json.Unmarshal(clean, &cfg); err != nil {
		return nil
	}

	if len(cfg.CompilerOptions.Paths) == 0 {
		return nil
	}

	// Resolve baseUrl relative to the tsconfig.json's own directory.
	tsconfigDir := filepath.Dir(tsconfigPath)
	baseURL := tsconfigDir
	if cfg.CompilerOptions.BaseURL != "" {
		baseURL = filepath.Clean(filepath.Join(tsconfigDir, filepath.FromSlash(cfg.CompilerOptions.BaseURL)))
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
			// prefix becomes "@/" (key without the trailing "*").
			prefix := strings.TrimSuffix(key, "*") // "@/"
			valBase := strings.TrimSuffix(value, "/*")
			absBase := filepath.Clean(filepath.Join(baseURL, filepath.FromSlash(valBase)))

			// Guard: the resolved directory must still be inside the repo.
			if !isUnderRepo(absBase, repoPath) {
				continue
			}

			result = append(result, tsConfigAlias{
				prefix:  prefix,
				baseDir: absBase,
			})
		} else if !keyHasWild {
			// Exact alias: "@utils" → ["./src/utils/index"]
			absTarget := filepath.Clean(filepath.Join(baseURL, filepath.FromSlash(value)))
			result = append(result, tsConfigAlias{
				prefix:      key,
				exact:       true,
				exactTarget: absTarget,
			})
		}
		// Mixed (key has wildcard but value does not, or vice versa) — rare and
		// ambiguous; skip.
	}

	return result
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
