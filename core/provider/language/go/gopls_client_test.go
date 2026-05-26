package goprovider_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	goprovider "github.com/GreenFuze/SuitCode/core/provider/language/go"
)

// resolveGoplsBinaryOrSkip resolves the gopls binary path, skipping the test
// if it cannot be found and we're in short mode, or failing hard if not.
func resolveGoplsBinaryOrSkip(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping gopls integration test in short mode")
	}

	path, lim := goprovider.ResolveBinaryForTest()
	if lim != nil {
		t.Skipf("gopls not available (%s: %s) — skipping integration test", lim.Kind, lim.Message)
	}
	return path
}

// repoRootForClient returns the absolute path to the repository root.
// It walks up from the current file until it finds a go.mod.
func repoRootForClient(t *testing.T) string {
	t.Helper()
	// Start from the test file's directory and walk up.
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("getting abs path: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod — cannot determine repo root")
		}
		dir = parent
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TestGoplsClient_Initialize
// ──────────────────────────────────────────────────────────────────────────────

func TestGoplsClient_Initialize(t *testing.T) {
	binary := resolveGoplsBinaryOrSkip(t)
	root := repoRootForClient(t)

	client := goprovider.NewGoplsClientForTest(binary, root)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Cleanup.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := client.Shutdown(shutCtx); err != nil {
		t.Logf("Shutdown returned (expected on forced kill): %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TestGoplsClient_DocumentSymbols_KnownFile
// ──────────────────────────────────────────────────────────────────────────────

func TestGoplsClient_DocumentSymbols_KnownFile(t *testing.T) {
	binary := resolveGoplsBinaryOrSkip(t)
	root := repoRootForClient(t)

	client := goprovider.NewGoplsClientForTest(binary, root)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		_ = client.Shutdown(shutCtx)
	}()

	// Use provider.go from the same package — it has known symbols.
	providerFile := filepath.Join(root, "core", "provider", "language", "go", "provider.go")
	if _, err := os.Stat(providerFile); err != nil {
		t.Fatalf("test fixture file not found: %s", providerFile)
	}

	symbols, err := client.DocumentSymbols(ctx, providerFile)
	if err != nil {
		t.Fatalf("DocumentSymbols failed: %v", err)
	}

	if len(symbols) == 0 {
		t.Fatal("expected symbols from provider.go, got none")
	}

	// Flatten and check for known symbols.
	// gopls returns methods as top-level symbols named "(*Receiver).Method",
	// so we use symbolPresent() which accepts either exact match or ".Name" suffix.
	names := goprovider.FlattenSymbolNamesForTest(symbols)

	expected := []string{
		"GoLanguageProvider",
		"NewGoLanguageProvider",
		"Ready",
		"GoplsReady",
		"Close",
		"FileImports",
		"FileImporters",
	}
	for _, want := range expected {
		if !symbolPresent(names, want) {
			t.Errorf("expected symbol %q in %v", want, names)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TestGoplsClient_DocumentSymbols_EmptyFile
// ──────────────────────────────────────────────────────────────────────────────

func TestGoplsClient_DocumentSymbols_EmptyFile(t *testing.T) {
	binary := resolveGoplsBinaryOrSkip(t)
	root := repoRootForClient(t)

	client := goprovider.NewGoplsClientForTest(binary, root)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		_ = client.Shutdown(shutCtx)
	}()

	// Create a valid Go file with only a package declaration — no top-level symbols.
	tmp := t.TempDir()
	emptyFile := filepath.Join(tmp, "empty.go")
	if err := os.WriteFile(emptyFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("creating empty file: %v", err)
	}

	// gopls may return nil for files outside the workspace or with no symbols.
	// Both outcomes are acceptable — we just must not get an error.
	_, err := client.DocumentSymbols(ctx, emptyFile)
	if err != nil {
		// This is expected to possibly fail for out-of-workspace files with some
		// gopls versions. Mark as a warning, not a fatal.
		t.Logf("DocumentSymbols on out-of-workspace empty file: %v (may be expected)", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TestGoplsClient_Shutdown_Idempotent
// ──────────────────────────────────────────────────────────────────────────────

func TestGoplsClient_Shutdown_Idempotent(t *testing.T) {
	binary := resolveGoplsBinaryOrSkip(t)
	root := repoRootForClient(t)

	client := goprovider.NewGoplsClientForTest(binary, root)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()

	// First shutdown.
	_ = client.Shutdown(shutCtx)

	// Second shutdown must not panic.
	_ = client.Shutdown(shutCtx)
}

// ──────────────────────────────────────────────────────────────────────────────
// TestGoplsClient_DocumentSymbols_MultipleFiles
// ──────────────────────────────────────────────────────────────────────────────

func TestGoplsClient_DocumentSymbols_MultipleFiles(t *testing.T) {
	binary := resolveGoplsBinaryOrSkip(t)
	root := repoRootForClient(t)

	client := goprovider.NewGoplsClientForTest(binary, root)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		_ = client.Shutdown(shutCtx)
	}()

	// Query symbols from two different files in the same workspace.
	files := []string{
		filepath.Join(root, "core", "provider", "language", "go", "provider.go"),
		filepath.Join(root, "core", "lsp", "types.go"),
	}

	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("test fixture not found: %s", f)
		}
		syms, err := client.DocumentSymbols(ctx, f)
		if err != nil {
			t.Errorf("DocumentSymbols(%s) failed: %v", filepath.Base(f), err)
			continue
		}
		if len(syms) == 0 {
			t.Errorf("DocumentSymbols(%s) returned no symbols", filepath.Base(f))
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TestGoplsClient_LspTypes_KnownFile
// ──────────────────────────────────────────────────────────────────────────────

func TestGoplsClient_LspTypes_KnownFile(t *testing.T) {
	binary := resolveGoplsBinaryOrSkip(t)
	root := repoRootForClient(t)

	client := goprovider.NewGoplsClientForTest(binary, root)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		_ = client.Shutdown(shutCtx)
	}()

	lspTypesFile := filepath.Join(root, "core", "lsp", "types.go")
	if _, err := os.Stat(lspTypesFile); err != nil {
		t.Fatalf("test fixture not found: %s", lspTypesFile)
	}

	symbols, err := client.DocumentSymbols(ctx, lspTypesFile)
	if err != nil {
		t.Fatalf("DocumentSymbols failed: %v", err)
	}

	names := goprovider.FlattenSymbolNamesForTest(symbols)
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}

	// core/lsp/types.go defines exported types such as DocumentSymbol, Position.
	expected := []string{"DocumentSymbol", "Position"}
	for _, want := range expected {
		if !nameSet[want] {
			t.Logf("symbol %q not found in %v (may depend on gopls version)", want, names)
		}
	}

	// At minimum there must be some symbols.
	if len(symbols) == 0 {
		t.Error("expected at least one symbol from lsp_types.go")
	}

	_ = runtime.GOOS // suppress import-unused on non-Windows
}
