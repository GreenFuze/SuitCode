package csprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GreenFuze/SuitCode/core/lsp"
)

// csharpLspClient is a high-level client for a running csharp-ls subprocess.
// It wraps a shared lsp.Transport and provides Initialize, Shutdown, and
// FileReferences operations. All methods are safe for concurrent use after
// Initialize() returns successfully.
type csharpLspClient struct {
	transport *lsp.Transport
	rootPath  string // absolute filesystem path of the workspace root
	rootURI   string // file:// URI of the workspace root
}

// newCsharpLspClient creates a client for the given binary and workspace root.
// The subprocess is NOT started — call Initialize() to start it and perform
// the LSP handshake.
func newCsharpLspClient(binaryPath, rootPath string) *csharpLspClient {
	return &csharpLspClient{
		transport: lsp.NewTransport(binaryPath),
		rootPath:  rootPath,
		rootURI:   lsp.PathToURI(rootPath),
	}
}

// Initialize starts the csharp-ls subprocess and performs the LSP initialize
// handshake:
//
//  1. Start subprocess
//  2. Send "initialize" request with rootUri, workspace folders, and capabilities
//  3. Send "initialized" notification
//
// Returns an error if the subprocess fails to start or the handshake fails.
func (c *csharpLspClient) Initialize(ctx context.Context) error {
	if err := c.transport.Start(); err != nil {
		return fmt.Errorf("csharp-ls: start process: %w", err)
	}

	params := lsp.InitializeParams{
		ProcessID: nil,
		RootURI:   c.rootURI,
		Capabilities: lsp.ClientCapabilities{
			TextDocument: lsp.TextDocumentCapabilities{
				DocumentSymbol: lsp.DocumentSymbolCapabilities{
					HierarchicalDocumentSymbolSupport: true,
				},
			},
		},
		WorkspaceFolders: []lsp.WorkspaceFolder{
			{URI: c.rootURI, Name: filepath.Base(c.rootPath)},
		},
	}

	if _, err := c.transport.SendRequest(ctx, "initialize", params); err != nil {
		c.transport.Stop()
		return fmt.Errorf("csharp-ls: initialize request: %w", err)
	}

	// The "initialized" notification signals that the client is ready.
	if err := c.transport.SendNotify("initialized", struct{}{}); err != nil {
		c.transport.Stop()
		return fmt.Errorf("csharp-ls: initialized notification: %w", err)
	}

	return nil
}

// Shutdown sends the LSP shutdown request + exit notification, then stops the
// transport. Best-effort — errors from the graceful sequence are ignored.
func (c *csharpLspClient) Shutdown(ctx context.Context) error {
	_, _ = c.transport.SendRequest(ctx, "shutdown", nil)
	_ = c.transport.SendNotify("exit", nil)
	c.transport.Stop()
	return nil
}

// FileReferences returns the absolute paths of all files that reference any
// exported type defined in the file at absPath.
//
// Protocol:
//  1. textDocument/didOpen    — open the seed file
//  2. textDocument/documentSymbol — enumerate top-level types
//  3. For each top-level type: textDocument/references at selectionRange.start
//  4. textDocument/didClose   — always deferred
//  5. Deduplicate URIs → abs paths, exclude the seed file itself
func (c *csharpLspClient) FileReferences(ctx context.Context, absPath string) ([]string, error) {
	// Read the file content — csharp-ls needs the full text in didOpen.
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("csharp-ls: reading %s: %w", absPath, err)
	}

	uri := lsp.PathToURI(absPath)

	// Open the document so csharp-ls can serve symbol and reference queries.
	didOpenParams := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": "csharp",
			"version":    1,
			"text":       string(content),
		},
	}
	if err := c.transport.SendNotify("textDocument/didOpen", didOpenParams); err != nil {
		return nil, fmt.Errorf("csharp-ls: didOpen %s: %w", absPath, err)
	}

	// Always close the document when done.
	defer func() {
		_ = c.transport.SendNotify("textDocument/didClose", map[string]any{
			"textDocument": map[string]any{"uri": uri},
		})
	}()

	// Request document symbols to enumerate top-level type declarations.
	symParams := map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}
	raw, err := c.transport.SendRequest(ctx, "textDocument/documentSymbol", symParams)
	if err != nil {
		return nil, fmt.Errorf("csharp-ls: documentSymbol %s: %w", absPath, err)
	}

	// csharp-ls returns JSON null for files with no symbols.
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var allSymbols []lsp.DocumentSymbol
	if err := json.Unmarshal(raw, &allSymbols); err != nil {
		return nil, fmt.Errorf("csharp-ls: unmarshalling symbols for %s: %w", absPath, err)
	}

	// Filter to top-level type declarations only: Class, Interface, Enum, Struct.
	// These are the declarations that other files can import/reference.
	topLevelTypes := filterTopLevelTypes(allSymbols)

	// Collect all reference URIs across all top-level types.
	seen := make(map[string]bool)
	seedAbsPath := filepath.Clean(absPath)

	for _, sym := range topLevelTypes {
		refParams := lsp.ReferenceParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: uri},
			Position:     sym.SelectionRange.Start,
			Context:      lsp.ReferenceContext{IncludeDeclaration: false},
		}

		refRaw, err := c.transport.SendRequest(ctx, "textDocument/references", refParams)
		if err != nil || len(refRaw) == 0 || string(refRaw) == "null" {
			// Empty or error for this type — continue to the next.
			continue
		}

		var locations []lsp.Location
		if err := json.Unmarshal(refRaw, &locations); err != nil {
			continue
		}

		// Convert each location URI to an absolute path and deduplicate.
		for _, loc := range locations {
			absRef := uriToPath(loc.URI)
			if absRef == "" {
				continue
			}
			absRef = filepath.Clean(absRef)
			if absRef == seedAbsPath {
				continue // exclude the seed file itself
			}
			seen[absRef] = true
		}
	}

	// Convert the deduplication set to a sorted slice.
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}

	return result, nil
}

// DocumentSymbols returns the full document symbol tree for the file at absPath.
//
// Protocol:
//  1. textDocument/didOpen       — open the seed file
//  2. textDocument/documentSymbol — enumerate all symbols
//  3. textDocument/didClose      — always deferred
//
// Returns nil slice (no error) when the file has no symbols.
func (c *csharpLspClient) DocumentSymbols(ctx context.Context, absPath string) ([]lsp.DocumentSymbol, error) {
	// Read the file content — csharp-ls needs the full text in didOpen.
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("csharp-ls: reading %s: %w", absPath, err)
	}

	uri := lsp.PathToURI(absPath)

	// Open the document so csharp-ls can serve symbol queries.
	didOpenParams := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": "csharp",
			"version":    1,
			"text":       string(content),
		},
	}
	if err := c.transport.SendNotify("textDocument/didOpen", didOpenParams); err != nil {
		return nil, fmt.Errorf("csharp-ls: didOpen %s: %w", absPath, err)
	}

	// Always close the document when done.
	defer func() {
		_ = c.transport.SendNotify("textDocument/didClose", map[string]any{
			"textDocument": map[string]any{"uri": uri},
		})
	}()

	// Request document symbols.
	symParams := map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}
	raw, err := c.transport.SendRequest(ctx, "textDocument/documentSymbol", symParams)
	if err != nil {
		return nil, fmt.Errorf("csharp-ls: documentSymbol %s: %w", absPath, err)
	}

	// csharp-ls returns JSON null for files with no symbols.
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var symbols []lsp.DocumentSymbol
	if err := json.Unmarshal(raw, &symbols); err != nil {
		return nil, fmt.Errorf("csharp-ls: unmarshalling symbols for %s: %w", absPath, err)
	}

	return symbols, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// filterTopLevelTypes returns the symbols from syms that represent top-level
// type declarations — Class (5), Interface (11), Enum (10), Struct (23).
// Children (nested types, methods, fields) are deliberately excluded so that
// we only send references requests for the publicly accessible entry points.
func filterTopLevelTypes(syms []lsp.DocumentSymbol) []lsp.DocumentSymbol {
	var result []lsp.DocumentSymbol
	for _, sym := range syms {
		switch sym.Kind {
		case lsp.SymbolKindClass,
			lsp.SymbolKindInterface,
			lsp.SymbolKindEnum,
			lsp.SymbolKindStruct:
			result = append(result, sym)
		}
	}
	return result
}

// uriToPath converts an LSP file:// URI back to an absolute filesystem path.
// This is the inverse of lsp.PathToURI.
//
// On Windows, file:///C:/foo/bar.cs → C:\foo\bar.cs
// On POSIX,   file:///foo/bar.cs   → /foo/bar.cs
func uriToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}

	// Parse the URI to handle percent-encoding correctly.
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}

	p := u.Path

	// On Windows, the path looks like /C:/foo/bar.cs — strip the leading slash
	// before the drive letter.
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}

	// Convert forward slashes to the OS path separator.
	return filepath.FromSlash(p)
}
