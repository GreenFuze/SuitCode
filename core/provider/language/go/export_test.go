// export_test.go exposes internal types and functions for use in external
// test packages (package goprovider_test). This file is only compiled during
// testing.
package goprovider

import (
	"context"

	"github.com/GreenFuze/SuitCode/core/provider"
)

// PackageNodeForTest is the exported view of packageNode for tests.
type PackageNodeForTest struct {
	PkgPath   string
	GoFiles   []string
	ImportIDs []string
}

// PackageIndexForTest wraps packageIndex and exposes test helpers.
type PackageIndexForTest struct {
	idx *packageIndex
}

// LoadPackageGraphForTest is the exported entry point for tests.
func LoadPackageGraphForTest(ctx context.Context, repoPath string) (*PackageIndexForTest, []provider.Limitation, error) {
	idx, lims, err := loadPackageGraph(ctx, repoPath)
	if err != nil {
		return nil, lims, err
	}
	return &PackageIndexForTest{idx: idx}, lims, nil
}

func (w *PackageIndexForTest) PkgPathCount() int {
	return len(w.idx.byPkgPath)
}

func (w *PackageIndexForTest) FileCount() int {
	return len(w.idx.byFile)
}

func (w *PackageIndexForTest) ReverseImportCount() int {
	return len(w.idx.reverseImports)
}

func (w *PackageIndexForTest) FileToNodeForTest(absPath string) *PackageNodeForTest {
	n := w.idx.fileToNode(absPath)
	if n == nil {
		return nil
	}
	return &PackageNodeForTest{
		PkgPath:   n.PkgPath,
		GoFiles:   n.GoFiles,
		ImportIDs: n.ImportIDs,
	}
}

func (w *PackageIndexForTest) ImportedFilesForTest(absPath string) []string {
	return w.idx.importedFiles(absPath)
}

func (w *PackageIndexForTest) ImporterFilesForTest(absPath string) []string {
	return w.idx.importerFiles(absPath)
}
