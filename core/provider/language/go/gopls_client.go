package goprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// goplsClient is a high-level client for a running gopls subprocess.
// It wraps an lspTransport and provides Initialize, Shutdown, and
// DocumentSymbols operations. All methods are safe for concurrent use
// after Initialize() returns successfully.
type goplsClient struct {
	transport *lspTransport
	rootPath  string // absolute filesystem path of the workspace root
	rootURI   string // file:// URI of the workspace root
}

// newGoplsClient creates a client for the given binary and workspace root.
// The subprocess is NOT started — call Initialize() to start it and perform
// the LSP handshake.
func newGoplsClient(binaryPath, rootPath string) *goplsClient {
	return &goplsClient{
		transport: newLspTransport(binaryPath),
		rootPath:  rootPath,
		rootURI:   pathToURI(rootPath),
	}
}

// Initialize starts the gopls subprocess and performs the LSP initialize
// handshake:
//
//  1. Start subprocess
//  2. Send "initialize" request (with hierarchicalDocumentSymbolSupport: true)
//  3. Send "initialized" notification
//
// Returns an error if the subprocess fails to start or the handshake fails.
func (c *goplsClient) Initialize(ctx context.Context) error {
	if err := c.transport.Start(); err != nil {
		return fmt.Errorf("gopls: start process: %w", err)
	}

	params := lspInitializeParams{
		ProcessID: nil, // null — gopls does not need our PID
		RootURI:   c.rootURI,
		Capabilities: lspClientCapabilities{
			TextDocument: lspTextDocumentCapabilities{
				DocumentSymbol: lspDocumentSymbolCapabilities{
					HierarchicalDocumentSymbolSupport: true,
				},
			},
		},
		WorkspaceFolders: []lspWorkspaceFolder{
			{URI: c.rootURI, Name: filepath.Base(c.rootPath)},
		},
	}

	if _, err := c.transport.SendRequest(ctx, "initialize", params); err != nil {
		c.transport.Stop() // clean up the subprocess on handshake failure
		return fmt.Errorf("gopls: initialize request: %w", err)
	}

	// The "initialized" notification signals to gopls that the client is ready.
	if err := c.transport.SendNotify("initialized", struct{}{}); err != nil {
		c.transport.Stop()
		return fmt.Errorf("gopls: initialized notification: %w", err)
	}

	return nil
}

// Shutdown sends the LSP shutdown request + exit notification, then stops
// the transport. Safe to call multiple times.
func (c *goplsClient) Shutdown(ctx context.Context) error {
	// Best-effort: send the graceful shutdown sequence.
	// Errors are ignored because we're stopping regardless.
	_, _ = c.transport.SendRequest(ctx, "shutdown", nil)
	_ = c.transport.SendNotify("exit", nil)

	// Force-stop the subprocess and wait for cleanup.
	c.transport.Stop()
	return nil
}

// DocumentSymbols returns the symbols defined in the file at absPath.
//
// Protocol:
//  1. textDocument/didOpen  (gopls requires the document to be open)
//  2. textDocument/documentSymbol  (request symbols)
//  3. textDocument/didClose  (deferred — runs even on error)
//
// Returns nil slice (no error) when the file has no symbols.
func (c *goplsClient) DocumentSymbols(ctx context.Context, absPath string) ([]lspDocumentSymbol, error) {
	// Read file content — gopls needs the full text in didOpen.
	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("gopls: reading %s: %w", absPath, err)
	}

	uri := pathToURI(absPath)

	// Open the document — gopls requires this before answering symbol queries.
	didOpenParams := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": "go",
			"version":    1,
			"text":       string(content),
		},
	}
	if err := c.transport.SendNotify("textDocument/didOpen", didOpenParams); err != nil {
		return nil, fmt.Errorf("gopls: didOpen %s: %w", absPath, err)
	}

	// Always close the document when done (even on error).
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
		return nil, fmt.Errorf("gopls: documentSymbol %s: %w", absPath, err)
	}

	// gopls returns JSON null for files with no symbols.
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var symbols []lspDocumentSymbol
	if err := json.Unmarshal(raw, &symbols); err != nil {
		return nil, fmt.Errorf("gopls: unmarshalling symbols for %s: %w", absPath, err)
	}
	return symbols, nil
}
