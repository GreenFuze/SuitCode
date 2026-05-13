package goprovider

import (
	"net/url"
	"path/filepath"
	"runtime"
)

// ──────────────────────────────────────────────────────────────────────────────
// Minimal LSP protocol types for Phase 2.
// Only the subset used by textDocument/documentSymbol is included.
// ──────────────────────────────────────────────────────────────────────────────

// lspPosition is a zero-based line/character offset within a text document.
type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// lspRange is a start/end position pair within a text document.
type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

// lspSymbolKind follows the LSP specification integer codes.
// Only values that gopls returns for Go source files are listed.
type lspSymbolKind int

const (
	lspSymbolKindFile          lspSymbolKind = 1
	lspSymbolKindModule        lspSymbolKind = 2
	lspSymbolKindNamespace     lspSymbolKind = 3
	lspSymbolKindPackage       lspSymbolKind = 4
	lspSymbolKindClass         lspSymbolKind = 5
	lspSymbolKindMethod        lspSymbolKind = 6
	lspSymbolKindProperty      lspSymbolKind = 7
	lspSymbolKindField         lspSymbolKind = 8
	lspSymbolKindConstructor   lspSymbolKind = 9
	lspSymbolKindEnum          lspSymbolKind = 10
	lspSymbolKindInterface     lspSymbolKind = 11
	lspSymbolKindFunction      lspSymbolKind = 12
	lspSymbolKindVariable      lspSymbolKind = 13
	lspSymbolKindConstant      lspSymbolKind = 14
	lspSymbolKindString        lspSymbolKind = 15
	lspSymbolKindNumber        lspSymbolKind = 16
	lspSymbolKindBoolean       lspSymbolKind = 17
	lspSymbolKindArray         lspSymbolKind = 18
	lspSymbolKindObject        lspSymbolKind = 19
	lspSymbolKindStruct        lspSymbolKind = 23
	lspSymbolKindTypeParameter lspSymbolKind = 26
)

// lspDocumentSymbol is the hierarchical form returned by
// textDocument/documentSymbol when the client advertises
// hierarchicalDocumentSymbolSupport: true.
type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Detail         string              `json:"detail,omitempty"`
	Kind           lspSymbolKind       `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children,omitempty"`
}

// lspInitializeParams is the params object for the "initialize" request.
// Deliberately minimal — only the fields gopls needs for symbol queries.
type lspInitializeParams struct {
	ProcessID        *int                  `json:"processId"`
	RootURI          string                `json:"rootUri"`
	Capabilities     lspClientCapabilities `json:"capabilities"`
	WorkspaceFolders []lspWorkspaceFolder  `json:"workspaceFolders"`
}

type lspClientCapabilities struct {
	TextDocument lspTextDocumentCapabilities `json:"textDocument"`
}

type lspTextDocumentCapabilities struct {
	DocumentSymbol lspDocumentSymbolCapabilities `json:"documentSymbol"`
}

type lspDocumentSymbolCapabilities struct {
	HierarchicalDocumentSymbolSupport bool `json:"hierarchicalDocumentSymbolSupport"`
}

type lspWorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// ──────────────────────────────────────────────────────────────────────────────
// File-URI helper
// ──────────────────────────────────────────────────────────────────────────────

// pathToURI converts an absolute filesystem path to an LSP file:// URI.
//
//   - POSIX: /foo/bar.go  →  file:///foo/bar.go
//   - Windows: C:\foo\bar.go  →  file:///C:/foo/bar.go
//
// The implementation uses net/url so special characters (spaces, etc.) are
// percent-encoded correctly.
func pathToURI(absPath string) string {
	p := filepath.ToSlash(absPath)

	// Windows drive letter: "C:/foo" needs a leading "/" to give "/C:/foo"
	// so that url.URL produces three slashes (file:// + /C:/...).
	if runtime.GOOS == "windows" && len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}

	u := &url.URL{Scheme: "file", Path: p}
	return u.String()
}
