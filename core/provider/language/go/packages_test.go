package goprovider_test

import (
	"context"
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
