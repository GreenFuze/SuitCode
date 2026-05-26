package csprovider

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GreenFuze/SuitCode/core/provider"
)

// csSkipDirs is the set of directory names to skip during the file walk.
// These are either build artefacts (bin, obj, TestResults) or IDE/VCS metadata.
var csSkipDirs = map[string]bool{
	".git":        true,
	".claude":     true,
	"node_modules": true,
	"bin":         true,
	"obj":         true,
	".vs":         true,
	".idea":       true,
	"packages":    true, // NuGet packages directory (pre-PackageReference era)
	"TestResults": true,
}

// csSourceExts is the set of file extensions the C# provider indexes.
// .axaml is the Avalonia-specific XAML extension.
var csSourceExts = map[string]bool{
	".cs":    true,
	".axaml": true,
	".xaml":  true,
}

// PackageRef is one <PackageReference> entry from a .csproj file.
type PackageRef struct {
	Name    string // e.g. "Avalonia.Controls.ItemsRepeater"
	Version string // e.g. "12.0.0" — empty when not declared
}

// csImportIndex is the in-memory bidirectional file-level import graph for a C# repo.
// All maps use absolute file paths as keys. Immutable after construction.
type csImportIndex struct {
	// fileImports maps abs file → sorted abs file paths it "imports" (files in
	// projects that this file's project directly references via <ProjectReference>).
	fileImports map[string][]string

	// fileImporters maps abs file → sorted abs file paths that "import" it (files
	// in projects that directly reference this file's project).
	fileImporters map[string][]string

	// filePeers maps abs file → sorted abs file paths of all other source files
	// in the same .csproj project. These files compile together in the same
	// assembly — a project-manifest fact, not a filesystem heuristic.
	filePeers map[string][]string

	// partners maps .axaml → .axaml.cs and vice-versa (Avalonia code-behind pairs).
	partners map[string]string

	// filePackageRefs maps abs source file path → NuGet package refs from its
	// containing project's <PackageReference> elements.
	filePackageRefs map[string][]PackageRef

	// sourceFileCount is the total number of C#/XAML source files indexed across all projects.
	sourceFileCount int
}

// csprojXML is the minimal XML shape we unmarshal from a .csproj file.
// SDK-style project files nest <ProjectReference> and <PackageReference> elements
// inside <ItemGroup> elements.
type csprojXML struct {
	ItemGroups []struct {
		ProjectRefs []struct {
			Include string `xml:"Include,attr"`
		} `xml:"ProjectReference"`
		PackageRefs []struct {
			Include string `xml:"Include,attr"`
			Version string `xml:"Version,attr"`
		} `xml:"PackageReference"`
	} `xml:"ItemGroup"`
}

// csProjectDef describes one .csproj: its location, referenced projects, and owned files.
type csProjectDef struct {
	absPath     string       // absolute path to the .csproj file
	dir         string       // directory containing the .csproj (base for implicit SDK includes)
	projectRefs []string     // absolute paths of referenced .csproj files
	sourceFiles []string     // absolute paths of .cs/.axaml/.xaml files owned by this project
	packageRefs []PackageRef // NuGet <PackageReference> items declared in this .csproj
}

// buildCSImportGraph walks repoPath, discovers all .csproj files, parses their
// <ProjectReference> elements, collects the source files owned by each project,
// and builds the bidirectional file-level import graph plus the Avalonia partner map.
//
// The import graph is authoritative: it reflects only explicit <ProjectReference>
// declarations, which are the compiler-verified dependency contracts. No heuristics
// such as `using` directive parsing are used.
func buildCSImportGraph(repoPath string) (*csImportIndex, []provider.Limitation, error) {
	// Step 1: Find all .csproj files.
	csprojFiles, err := findCSProjFiles(repoPath)
	if err != nil {
		return nil, nil, fmt.Errorf("csharp: walking repo for .csproj files: %w", err)
	}

	idx := &csImportIndex{
		fileImports:     make(map[string][]string),
		fileImporters:   make(map[string][]string),
		filePeers:       make(map[string][]string),
		partners:        make(map[string]string),
		filePackageRefs: make(map[string][]PackageRef),
	}

	if len(csprojFiles) == 0 {
		return idx, []provider.Limitation{{
			Kind:    "no_csproj_files",
			Message: "no .csproj files found — C# import graph not available",
			Scope:   repoPath,
		}}, nil
	}

	// Build the set of all project directories so each project claims only its
	// immediate subtree (stopping at nested project boundaries during file collection).
	projectDirs := make(map[string]bool, len(csprojFiles))
	for _, cp := range csprojFiles {
		projectDirs[filepath.Dir(cp)] = true
	}

	// Step 2: Parse each .csproj and collect its source files.
	var limitations []provider.Limitation
	projects := make([]*csProjectDef, 0, len(csprojFiles))
	for _, cp := range csprojFiles {
		proj, lim := parseCSProj(cp, repoPath, projectDirs)
		if lim != nil {
			limitations = append(limitations, *lim)
			continue
		}
		projects = append(projects, proj)
	}

	// Step 3: Index projects by their .csproj absolute path for fast ref resolution.
	byPath := make(map[string]*csProjectDef, len(projects))
	for _, p := range projects {
		byPath[p.absPath] = p
		idx.sourceFileCount += len(p.sourceFiles)
	}

	if idx.sourceFileCount == 0 {
		return idx, limitations, nil
	}

	// Step 3b: Populate filePackageRefs — map both the .csproj file itself and
	// every source file it owns to the project's NuGet package references.
	for _, proj := range projects {
		if len(proj.packageRefs) == 0 {
			continue
		}

		// The .csproj file itself is also a valid lookup key (e.g. when the agent
		// runs explain-file on the .csproj directly).
		idx.filePackageRefs[proj.absPath] = proj.packageRefs

		// Every source file owned by this project inherits the same package refs.
		for _, f := range proj.sourceFiles {
			idx.filePackageRefs[f] = proj.packageRefs
		}
	}

	// Step 3c: Build the filePeers map — for every source file in a project,
	// record all other source files in the same project. These files compile into
	// the same assembly; the relationship is established by the .csproj manifest,
	// not by filesystem proximity.
	for _, proj := range projects {
		for _, fa := range proj.sourceFiles {
			for _, fb := range proj.sourceFiles {
				if fa != fb {
					idx.filePeers[fa] = append(idx.filePeers[fa], fb)
				}
			}
		}
	}
	for k := range idx.filePeers {
		idx.filePeers[k] = dedup(idx.filePeers[k])
	}

	// Step 4: Build the bidirectional file-level graph from project-level references.
	// For each project A that references project B:
	//   • every file in A "imports" every file in B
	//   • every file in B has A's files as "importers"
	for _, projA := range projects {
		for _, refPath := range projA.projectRefs {
			projB, ok := byPath[refPath]
			if !ok {
				continue // referenced project not within this repo
			}

			for _, fa := range projA.sourceFiles {
				for _, fb := range projB.sourceFiles {
					idx.fileImports[fa] = append(idx.fileImports[fa], fb)
					idx.fileImporters[fb] = append(idx.fileImporters[fb], fa)
				}
			}
		}
	}

	// Step 5: Deduplicate and sort all adjacency lists for determinism.
	for k := range idx.fileImports {
		idx.fileImports[k] = dedup(idx.fileImports[k])
	}
	for k := range idx.fileImporters {
		idx.fileImporters[k] = dedup(idx.fileImporters[k])
	}

	// Step 6: Build Avalonia .axaml ↔ .axaml.cs partner pairs.
	var allFiles []string
	for _, proj := range projects {
		allFiles = append(allFiles, proj.sourceFiles...)
	}
	addAvaloniaPartners(idx, allFiles)

	return idx, limitations, nil
}

// findCSProjFiles walks repoPath and returns absolute paths of all .csproj files,
// skipping build-artefact and IDE directories defined in csSkipDirs.
func findCSProjFiles(repoPath string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries rather than aborting
		}
		if d.IsDir() {
			if csSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".csproj") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found) // deterministic order across OS directory traversal
	return found, nil
}

// parseCSProj parses a single .csproj file and returns its project definition.
// projectDirs is the global set of all project directory paths; it is used to stop
// source-file collection at nested project boundaries.
// Returns a Limitation (not an error) when the file cannot be read or parsed —
// consistent with the fail-transparent policy for individual file errors.
func parseCSProj(csprojPath, repoPath string, projectDirs map[string]bool) (*csProjectDef, *provider.Limitation) {
	data, err := os.ReadFile(csprojPath)
	if err != nil {
		return nil, &provider.Limitation{
			Kind:    "csproj_unreadable",
			Message: fmt.Sprintf("cannot read %s: %v", csprojPath, err),
			Scope:   csprojPath,
		}
	}

	var xmlDoc csprojXML
	if err := xml.Unmarshal(data, &xmlDoc); err != nil {
		return nil, &provider.Limitation{
			Kind:    "csproj_parse_error",
			Message: fmt.Sprintf("cannot parse XML in %s: %v", csprojPath, err),
			Scope:   csprojPath,
		}
	}

	proj := &csProjectDef{
		absPath: csprojPath,
		dir:     filepath.Dir(csprojPath),
	}

	// Resolve <ProjectReference Include="..."> paths to absolute paths.
	// The Include attribute is relative to the .csproj's own directory.
	for _, ig := range xmlDoc.ItemGroups {
		for _, ref := range ig.ProjectRefs {
			if ref.Include == "" {
				continue
			}
			abs := filepath.Clean(filepath.Join(proj.dir, filepath.FromSlash(ref.Include)))

			// Guard: only include references that are inside this repo.
			if rel, relErr := filepath.Rel(repoPath, abs); relErr == nil && !strings.HasPrefix(rel, "..") {
				proj.projectRefs = append(proj.projectRefs, abs)
			}
		}

		// Collect <PackageReference Include="..." Version="..."> entries.
		for _, pkg := range ig.PackageRefs {
			if pkg.Include == "" {
				continue
			}
			proj.packageRefs = append(proj.packageRefs, PackageRef{
				Name:    pkg.Include,
				Version: pkg.Version,
			})
		}
	}

	// SDK-style .csproj files implicitly include all matching source files in
	// the project tree. We collect them here, stopping at nested project roots.
	sourceFiles, collectErr := collectProjectSourceFiles(proj.dir, projectDirs)
	if collectErr != nil {
		return nil, &provider.Limitation{
			Kind:    "csproj_source_collection_failed",
			Message: fmt.Sprintf("collecting source files for %s: %v", csprojPath, collectErr),
			Scope:   csprojPath,
		}
	}
	proj.sourceFiles = sourceFiles

	return proj, nil
}

// collectProjectSourceFiles returns the .cs/.axaml/.xaml files owned by the project
// rooted at projDir. Files in subdirectories that have their own .csproj are
// excluded — they belong to the nested project.
func collectProjectSourceFiles(projDir string, projectDirs map[string]bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(projDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if csSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			// Stop at a nested project directory (do not skip projDir itself).
			if path != projDir && projectDirs[path] {
				return filepath.SkipDir
			}
			return nil
		}
		if csSourceExts[strings.ToLower(filepath.Ext(path))] {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// addAvaloniaPartners populates idx.partners with bidirectional .axaml ↔ .axaml.cs
// pairs. Avalonia generates a code-behind file for every .axaml file; the pair is
// named identically plus ".cs" (e.g. MainWindow.axaml → MainWindow.axaml.cs).
//
// Detection uses suffix matching rather than filepath.Ext because .axaml.cs is a
// compound extension — filepath.Ext returns ".cs", not ".axaml.cs".
func addAvaloniaPartners(idx *csImportIndex, allFiles []string) {
	// Build a presence set for O(1) existence checks.
	fileSet := make(map[string]bool, len(allFiles))
	for _, f := range allFiles {
		fileSet[f] = true
	}

	for _, f := range allFiles {
		lower := strings.ToLower(f)

		// Only match .axaml files (not .axaml.cs code-behinds, which we'll link
		// from the .axaml side).
		if strings.HasSuffix(lower, ".axaml") {
			partner := f + ".cs" // MainWindow.axaml → MainWindow.axaml.cs
			if fileSet[partner] {
				idx.partners[f] = partner
				idx.partners[partner] = f
			}
		}
	}
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
