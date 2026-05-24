package goprovider_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	goprovider "github.com/GreenFuze/SuitCode/core/provider/language/go"
)

// repoRoot walks up from this test file's location to find the repository root
// (the directory containing go.mod). Panics if not found.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repository root (go.mod not found)")
		}
		dir = parent
	}
}

// loadIndex is a test helper that loads the package graph for the SuitCode repo.
func loadIndex(t *testing.T) *goprovider.PackageIndexForTest {
	t.Helper()
	root := repoRoot(t)
	idx, lims, err := goprovider.LoadPackageGraphForTest(context.Background(), root)
	if err != nil {
		t.Fatalf("loadPackageGraph: %v", err)
	}
	for _, lim := range lims {
		t.Logf("limitation [%s]: %s", lim.Kind, lim.Message)
	}
	return idx
}

// TestLoadPackageGraph_SuitCode verifies that the package graph loads
// successfully against the SuitCode module itself.
func TestLoadPackageGraph_SuitCode(t *testing.T) {
	idx := loadIndex(t)

	if n := idx.PkgPathCount(); n < 5 {
		t.Errorf("expected at least 5 packages, got %d", n)
	}
	if n := idx.FileCount(); n < 10 {
		t.Errorf("expected at least 10 .go files indexed, got %d", n)
	}
	if n := idx.ReverseImportCount(); n == 0 {
		t.Error("expected a non-empty reverse import map")
	}

	t.Logf("packages=%d files=%d reverseEdges=%d",
		idx.PkgPathCount(), idx.FileCount(), idx.ReverseImportCount())
}

// TestPackageIndex_fileToNode verifies that a known SuitCode file resolves to
// a non-nil package node.
func TestPackageIndex_fileToNode(t *testing.T) {
	idx := loadIndex(t)
	root := repoRoot(t)

	absPath := filepath.Join(root, "investigator", "investigator.go")
	node := idx.FileToNodeForTest(absPath)
	if node == nil {
		t.Fatalf("fileToNode(%q) returned nil; file not indexed", absPath)
	}
	t.Logf("node.PkgPath=%q GoFiles=%d", node.PkgPath, len(node.GoFiles))
}

// TestPackageIndex_importedFiles verifies that the forward import lookup
// returns files from expected packages.
func TestPackageIndex_importedFiles(t *testing.T) {
	idx := loadIndex(t)
	root := repoRoot(t)

	// investigator/features/context.go imports core/features and core/provider.
	seed := filepath.Join(root, "investigator", "features", "context.go")
	files := idx.ImportedFilesForTest(seed)

	if len(files) == 0 {
		t.Fatalf("importedFiles(%q) returned no files", seed)
	}

	// At least one file should be from core/features/.
	var hasCoreFeatures bool
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "core/features/") {
			hasCoreFeatures = true
		}
	}
	if !hasCoreFeatures {
		t.Errorf("expected at least one file from core/features/ in results; got: %v", files)
	}

	t.Logf("importedFiles returned %d files", len(files))
}

// TestPackageIndex_importerFiles verifies that the reverse import lookup
// returns files from packages known to import the target package.
func TestPackageIndex_importerFiles(t *testing.T) {
	idx := loadIndex(t)
	root := repoRoot(t)

	// core/provider/roles.go is in package "core/provider".
	// The filesystem provider (core/provider/filesystem/) imports core/provider.
	seed := filepath.Join(root, "core", "provider", "roles.go")
	files := idx.ImporterFilesForTest(seed)

	if len(files) == 0 {
		t.Fatalf("importerFiles(%q) returned no files", seed)
	}

	var hasFilesystem bool
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "core/provider/filesystem/") {
			hasFilesystem = true
		}
	}
	if !hasFilesystem {
		t.Errorf("expected at least one file from core/provider/filesystem/ in importer results; got: %v", files)
	}

	t.Logf("importerFiles returned %d files", len(files))
}

// TestPackageIndex_unknownFile verifies that all lookup methods handle an
// unknown path gracefully without panicking.
func TestPackageIndex_unknownFile(t *testing.T) {
	idx := loadIndex(t)

	unknown := "/absolutely/nonexistent/path/file.go"

	if node := idx.FileToNodeForTest(unknown); node != nil {
		t.Errorf("expected nil node for unknown file, got %+v", node)
	}
	if files := idx.ImportedFilesForTest(unknown); len(files) != 0 {
		t.Errorf("expected empty importedFiles for unknown file, got %v", files)
	}
	if files := idx.ImporterFilesForTest(unknown); len(files) != 0 {
		t.Errorf("expected empty importerFiles for unknown file, got %v", files)
	}
}

// TestFindModuleRoots_SingleModule verifies that a standard single-module repo
// (go.mod at the root) returns only the root directory.
func TestFindModuleRoots_SingleModule(t *testing.T) {
	root := repoRoot(t)
	roots := goprovider.FindModuleRootsForTest(root)

	if len(roots) != 1 {
		t.Fatalf("expected 1 module root, got %d: %v", len(roots), roots)
	}
	if roots[0] != root {
		t.Errorf("expected root %q, got %q", root, roots[0])
	}
}

// TestFindModuleRoots_MultiModule verifies that a repo with multiple go.mod
// files (one per module) correctly discovers all module roots.
func TestFindModuleRoots_MultiModule(t *testing.T) {
	// Build a temp directory tree that mimics a multi-module monorepo:
	//   <tmp>/
	//     server/go.mod
	//     server/plugins/plugin1/go.mod
	//     server/plugins/plugin2/go.mod
	//     frontend/         (no go.mod — JS-only)
	//     .git/go.mod       (must be skipped)
	//     .claude/go.mod    (must be skipped)

	tmp := t.TempDir()

	mkGoMod := func(dir, modulePath string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
		content := fmt.Sprintf("module %s\n\ngo 1.21\n", modulePath)
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile go.mod: %v", err)
		}
	}

	mkGoMod(filepath.Join(tmp, "server"), "example.com/myapp/server")
	mkGoMod(filepath.Join(tmp, "server", "plugins", "plugin1"), "example.com/myapp/plugin1")
	mkGoMod(filepath.Join(tmp, "server", "plugins", "plugin2"), "example.com/myapp/plugin2")

	// These should be skipped:
	mkGoMod(filepath.Join(tmp, ".git"), "example.com/myapp/should-not-appear")
	mkGoMod(filepath.Join(tmp, ".claude"), "example.com/myapp/also-should-not-appear")

	// frontend is a plain directory with no go.mod — should not contribute.
	if err := os.MkdirAll(filepath.Join(tmp, "frontend", "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll frontend: %v", err)
	}

	roots := goprovider.FindModuleRootsForTest(tmp)

	if len(roots) != 3 {
		t.Fatalf("expected 3 module roots, got %d: %v", len(roots), roots)
	}

	want := []string{
		filepath.Join(tmp, "server"),
		filepath.Join(tmp, "server", "plugins", "plugin1"),
		filepath.Join(tmp, "server", "plugins", "plugin2"),
	}
	for i, w := range want {
		if roots[i] != w {
			t.Errorf("roots[%d]: want %q, got %q", i, w, roots[i])
		}
	}
}

// TestFindModuleRoots_NoModule verifies that a directory tree with no go.mod
// returns the repo root itself (fallback behaviour).
func TestFindModuleRoots_NoModule(t *testing.T) {
	tmp := t.TempDir()

	// Create a plausible-looking directory tree but no go.mod.
	if err := os.MkdirAll(filepath.Join(tmp, "src", "main"), 0o755); err != nil {
		t.Fatal(err)
	}

	roots := goprovider.FindModuleRootsForTest(tmp)

	if len(roots) != 1 || roots[0] != tmp {
		t.Errorf("expected fallback to repo root %q, got %v", tmp, roots)
	}
}

// TestPackageIndex_Determinism verifies that repeated calls to importedFiles
// and importerFiles produce the same result.
func TestPackageIndex_Determinism(t *testing.T) {
	idx := loadIndex(t)
	root := repoRoot(t)

	seed := filepath.Join(root, "investigator", "features", "context.go")

	first := idx.ImportedFilesForTest(seed)
	second := idx.ImportedFilesForTest(seed)

	if len(first) != len(second) {
		t.Fatalf("determinism failure: first=%d files, second=%d files", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("determinism failure at index %d: %q != %q", i, first[i], second[i])
		}
	}
}

// TestLoadPackageGraph_MultiModuleMonorepo is an optional integration test that
// exercises the multi-module code path against the MGA repository when it is
// available on the local machine. It is skipped in CI environments where the
// MGA source tree is not present.
func TestLoadPackageGraph_MultiModuleMonorepo(t *testing.T) {
	const mgaPath = `C:\src\github.com\GreenFuze\MyGamesAnywhere`

	info, err := os.Stat(mgaPath)
	if err != nil || !info.IsDir() {
		t.Skipf("MGA repo not found at %s — skipping integration test", mgaPath)
	}

	// ── Part 1: verify module discovery ──────────────────────────────────────
	roots := goprovider.FindModuleRootsForTest(mgaPath)

	// MGA has server/go.mod + several plugin go.mod files.
	if len(roots) < 2 {
		t.Fatalf("expected at least 2 module roots in MGA, got %d: %v", len(roots), roots)
	}
	t.Logf("found %d module roots: %v", len(roots), roots)

	// ── Part 2: verify the package graph loads without a hard error ───────────
	idx, lims, err := goprovider.LoadPackageGraphForTest(context.Background(), mgaPath)
	if err != nil {
		t.Fatalf("loadPackageGraph on MGA: %v", err)
	}
	for _, lim := range lims {
		t.Logf("limitation [%s] (%s): %s", lim.Kind, lim.Scope, lim.Message)
	}

	pkgs := idx.PkgPathCount()
	files := idx.FileCount()
	edges := idx.ReverseImportCount()

	if pkgs == 0 {
		t.Error("expected at least one Go package to be indexed")
	}
	if files == 0 {
		t.Error("expected at least one .go file to be indexed")
	}

	t.Logf("packages=%d files=%d reverseEdges=%d", pkgs, files, edges)
}
