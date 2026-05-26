// export_test.go exposes internal types and functions for use in external
// test packages (package goprovider_test). This file is only compiled during
// testing.
package goprovider

import (
	"context"

	"github.com/GreenFuze/SuitCode/core/lsp"
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

// FindModuleRootsForTest exposes findModuleRoots for unit tests.
func FindModuleRootsForTest(repoPath string) []string {
	return findModuleRoots(repoPath)
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

// ──────────────────────────────────────────────────────────────────────────────
// Phase 2 (gopls) test exports
// ──────────────────────────────────────────────────────────────────────────────

// GoplsReadyForTest reports whether gopls has been successfully initialized.
func (p *GoLanguageProvider) GoplsReadyForTest() bool {
	return p.goplsReady.Load()
}

// ResolveBinaryForTest exposes the internal gopls binary resolver.
func ResolveBinaryForTest() (string, *provider.Limitation) {
	return resolveBinary()
}

// NewGoplsClientForTest creates a goplsClient for use in tests.
func NewGoplsClientForTest(binaryPath, rootPath string) *goplsClient {
	return newGoplsClient(binaryPath, rootPath)
}

// PathToURIForTest exposes the PathToURI helper from the shared lsp package.
func PathToURIForTest(absPath string) string {
	return lsp.PathToURI(absPath)
}

// FlattenSymbolNamesForTest exposes the internal symbol-flattening helper.
func FlattenSymbolNamesForTest(syms []lsp.DocumentSymbol) []string {
	return flattenSymbolNames(syms)
}

// LspDocumentSymbolForTest is an alias for lsp.DocumentSymbol so tests can
// construct symbol trees without importing the lsp package directly.
type LspDocumentSymbolForTest = lsp.DocumentSymbol

// ManagedGoplsBinDirForTest exposes managedGoplsBinDir for unit tests.
func ManagedGoplsBinDirForTest() string {
	return managedGoplsBinDir()
}
