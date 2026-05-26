package lsp

// ──────────────────────────────────────────────────────────────────────────────
// Shared LSP protocol types used by all language providers.
// ──────────────────────────────────────────────────────────────────────────────

// Position is a zero-based line/character offset within a text document.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a start/end position pair within a text document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// SymbolKind follows the LSP specification integer codes.
type SymbolKind int

const (
	SymbolKindFile          SymbolKind = 1
	SymbolKindModule        SymbolKind = 2
	SymbolKindNamespace     SymbolKind = 3
	SymbolKindPackage       SymbolKind = 4
	SymbolKindClass         SymbolKind = 5
	SymbolKindMethod        SymbolKind = 6
	SymbolKindProperty      SymbolKind = 7
	SymbolKindField         SymbolKind = 8
	SymbolKindConstructor   SymbolKind = 9
	SymbolKindEnum          SymbolKind = 10
	SymbolKindInterface     SymbolKind = 11
	SymbolKindFunction      SymbolKind = 12
	SymbolKindVariable      SymbolKind = 13
	SymbolKindConstant      SymbolKind = 14
	SymbolKindString        SymbolKind = 15
	SymbolKindNumber        SymbolKind = 16
	SymbolKindBoolean       SymbolKind = 17
	SymbolKindArray         SymbolKind = 18
	SymbolKindObject        SymbolKind = 19
	SymbolKindStruct        SymbolKind = 23
	SymbolKindTypeParameter SymbolKind = 26
)

// DocumentSymbol is the hierarchical form returned by textDocument/documentSymbol
// when the client advertises hierarchicalDocumentSymbolSupport: true.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           SymbolKind       `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// InitializeParams is the params object for the "initialize" request.
// Deliberately minimal — only the fields needed for symbol and reference queries.
type InitializeParams struct {
	ProcessID        *int               `json:"processId"`
	RootURI          string             `json:"rootUri"`
	Capabilities     ClientCapabilities `json:"capabilities"`
	WorkspaceFolders []WorkspaceFolder  `json:"workspaceFolders"`
}

// ClientCapabilities advertises what LSP features the client supports.
type ClientCapabilities struct {
	TextDocument TextDocumentCapabilities `json:"textDocument"`
}

// TextDocumentCapabilities is the set of text-document-level capabilities.
type TextDocumentCapabilities struct {
	DocumentSymbol DocumentSymbolCapabilities `json:"documentSymbol"`
}

// DocumentSymbolCapabilities controls the documentSymbol response shape.
type DocumentSymbolCapabilities struct {
	HierarchicalDocumentSymbolSupport bool `json:"hierarchicalDocumentSymbolSupport"`
}

// WorkspaceFolder identifies a workspace folder sent during initialize.
type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Types needed by textDocument/references
// ──────────────────────────────────────────────────────────────────────────────

// Location is an LSP Location — a URI plus a range within that document.
// Returned by textDocument/references and similar requests.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// TextDocumentIdentifier identifies a text document by URI.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// ReferenceContext controls whether the declaration itself is included in
// the results returned by textDocument/references.
type ReferenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

// ReferenceParams is the params struct for a textDocument/references request.
type ReferenceParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
	Context      ReferenceContext       `json:"context"`
}
