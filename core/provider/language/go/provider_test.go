package goprovider_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
	goprovider "github.com/GreenFuze/SuitCode/core/provider/language/go"
)

// attachedProvider creates and attaches a GoLanguageProvider to the SuitCode
// repo root. Marks the test as fatal if attach fails unexpectedly.
func attachedProvider(t *testing.T) *goprovider.GoLanguageProvider {
	t.Helper()
	root := repoRoot(t)
	p := goprovider.New()
	if err := p.Attach(context.Background(), root); err != nil {
		t.Fatalf("Attach(%q): %v", root, err)
	}
	return p
}

// TestGoLanguageProvider_AttachAndReady verifies that attaching to the
// SuitCode module marks the provider as ready.
func TestGoLanguageProvider_AttachAndReady(t *testing.T) {
	p := attachedProvider(t)
	if !p.Ready() {
		t.Error("expected Ready() == true after successful Attach")
	}
}

// TestGoLanguageProvider_Capabilities verifies static metadata.
func TestGoLanguageProvider_Capabilities(t *testing.T) {
	p := goprovider.New()
	caps := p.Capabilities()

	if caps.ID == "" {
		t.Error("Capabilities().ID must not be empty")
	}
	if caps.DisplayName == "" {
		t.Error("Capabilities().DisplayName must not be empty")
	}
	if len(caps.Languages) == 0 {
		t.Error("expected at least one language in Capabilities().Languages")
	}
}

// TestGoLanguageProvider_GetImports verifies that GetImports returns non-empty
// import path strings for a known file.
func TestGoLanguageProvider_GetImports(t *testing.T) {
	p := attachedProvider(t)
	root := repoRoot(t)

	absPath := filepath.Join(root, "core", "features", "context.go")
	result, err := p.GetImports(context.Background(), absPath)
	if err != nil {
		t.Fatalf("GetImports: %v", err)
	}
	if len(result.Data) == 0 {
		t.Fatal("expected non-empty import list, got none")
	}

	// Every import path should be a non-empty string.
	for _, imp := range result.Data {
		if imp == "" {
			t.Error("import path must not be empty")
		}
	}

	// At least one import should reference the SuitCode module.
	var hasModuleImport bool
	for _, imp := range result.Data {
		if strings.Contains(imp, "GreenFuze/SuitCode") {
			hasModuleImport = true
		}
	}
	if !hasModuleImport {
		t.Logf("imports: %v", result.Data)
		t.Error("expected at least one import from the SuitCode module")
	}
}

// TestGoLanguageProvider_GetSymbols verifies symbol retrieval works when gopls
// is available, or degrades gracefully when it is not yet ready.
func TestGoLanguageProvider_GetSymbols(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping gopls integration test in short mode")
	}

	p := attachedProvider(t)
	defer func() {
		if err := p.Close(); err != nil {
			t.Logf("Close(): %v", err)
		}
	}()

	root := repoRoot(t)
	absPath := filepath.Join(root, "core", "provider", "language", "go", "provider.go")

	// Poll for gopls readiness — startup typically takes 2–5 s.
	const pollInterval = 200 * time.Millisecond
	const maxWait = 60 * time.Second
	deadline := time.Now().Add(maxWait)
	for !p.GoplsReadyForTest() && time.Now().Before(deadline) {
		time.Sleep(pollInterval)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := p.GetSymbols(ctx, absPath)
	if err != nil {
		t.Fatalf("GetSymbols: %v", err)
	}
	if result == nil {
		t.Fatal("GetSymbols returned nil result")
	}

	if p.GoplsReadyForTest() {
		// gopls is available — verify real symbols are returned.
		if len(result.Data) == 0 {
			t.Errorf("expected symbols from provider.go, got none (limitations: %v)", result.Limitations)
		}
		if hasLimitationKind(result.Limitations, "gopls_not_ready") {
			t.Errorf("unexpected 'gopls_not_ready' limitation when gopls is ready: %v", result.Limitations)
		}

		// gopls returns methods as "(*GoLanguageProvider).Attach" etc., so we
		// use symbolPresent() which also matches on ".SymbolName" suffix.
		expected := []string{"GoLanguageProvider", "New", "Attach", "Ready", "GoplsReady", "Close"}
		for _, want := range expected {
			if !symbolPresent(result.Data, want) {
				t.Errorf("expected symbol %q in result %v", want, result.Data)
			}
		}

		// Provenance must reference the LSP source kind.
		if len(result.Provenance) == 0 {
			t.Error("expected non-empty Provenance when gopls returns symbols")
		}
		for _, prov := range result.Provenance {
			if prov.SourceKind != provider.SourceKindLSP {
				t.Errorf("expected SourceKindLSP provenance, got %q", prov.SourceKind)
			}
		}
	} else {
		// gopls failed to start within the timeout — graceful degradation.
		t.Logf("gopls not ready after %s; checking for graceful limitation", maxWait)
		if len(result.Limitations) == 0 {
			t.Error("expected at least one limitation when gopls is not ready")
		}
	}
}

// TestGoLanguageProvider_FileImports verifies that FileImports returns files
// from packages that the seed file is known to import.
func TestGoLanguageProvider_FileImports(t *testing.T) {
	p := attachedProvider(t)
	root := repoRoot(t)

	// investigator/features/context.go imports core/features and core/provider.
	seed := filepath.Join(root, "investigator", "features", "context.go")
	result, err := p.FileImports(context.Background(), seed)
	if err != nil {
		t.Fatalf("FileImports: %v", err)
	}
	if len(result.Data) == 0 {
		t.Fatal("expected non-empty FileImports result")
	}

	// At least one file should be from core/features/.
	var hasCoreFeatures bool
	for _, f := range result.Data {
		rel, _ := filepath.Rel(root, f)
		if strings.HasPrefix(filepath.ToSlash(rel), "core/features/") {
			hasCoreFeatures = true
		}
	}
	if !hasCoreFeatures {
		t.Errorf("expected at least one file from core/features/ in FileImports result; got: %v", result.Data)
	}

	// Provenance should be populated.
	if len(result.Provenance) == 0 {
		t.Error("expected non-empty Provenance in FileImports result")
	}
}

// TestGoLanguageProvider_FileImporters verifies that FileImporters returns
// files from packages known to import the seed's package.
func TestGoLanguageProvider_FileImporters(t *testing.T) {
	p := attachedProvider(t)
	root := repoRoot(t)

	// core/provider/roles.go is in core/provider.
	// core/provider/filesystem/ imports core/provider.
	seed := filepath.Join(root, "core", "provider", "roles.go")
	result, err := p.FileImporters(context.Background(), seed)
	if err != nil {
		t.Fatalf("FileImporters: %v", err)
	}
	if len(result.Data) == 0 {
		t.Fatal("expected non-empty FileImporters result for core/provider/roles.go")
	}

	var hasFilesystem bool
	for _, f := range result.Data {
		rel, _ := filepath.Rel(root, f)
		if strings.HasPrefix(filepath.ToSlash(rel), "core/provider/filesystem/") {
			hasFilesystem = true
		}
	}
	if !hasFilesystem {
		t.Errorf("expected at least one file from core/provider/filesystem/ in FileImporters result; got: %v", result.Data)
	}
}

// TestGoLanguageProvider_NotReadyGraceful verifies that calling methods on an
// un-attached provider returns safe results with limitations, not panics.
func TestGoLanguageProvider_NotReadyGraceful(t *testing.T) {
	p := goprovider.New()

	if p.Ready() {
		t.Error("fresh provider must not be ready before Attach")
	}

	ctx := context.Background()
	dummyPath := "/some/file.go"

	importsResult, err := p.FileImports(ctx, dummyPath)
	if err != nil {
		t.Fatalf("FileImports on not-ready provider returned error: %v", err)
	}
	if importsResult == nil {
		t.Fatal("FileImports must not return nil result")
	}
	if !hasLimitationKind(importsResult.Limitations, "provider_not_ready") {
		t.Errorf("expected 'provider_not_ready' limitation, got: %v", importsResult.Limitations)
	}

	importersResult, err := p.FileImporters(ctx, dummyPath)
	if err != nil {
		t.Fatalf("FileImporters on not-ready provider returned error: %v", err)
	}
	if importersResult == nil {
		t.Fatal("FileImporters must not return nil result")
	}
	if !hasLimitationKind(importersResult.Limitations, "provider_not_ready") {
		t.Errorf("expected 'provider_not_ready' limitation, got: %v", importersResult.Limitations)
	}
}

// TestGoLanguageProvider_CloseSafe verifies Close is a no-op and returns nil.
func TestGoLanguageProvider_CloseSafe(t *testing.T) {
	p := attachedProvider(t)
	if err := p.Close(); err != nil {
		t.Errorf("Close() returned unexpected error: %v", err)
	}
	// Second close should also be safe.
	if err := p.Close(); err != nil {
		t.Errorf("second Close() returned unexpected error: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// hasLimitationKind returns true if any limitation in lims has the given Kind.
func hasLimitationKind(lims []provider.Limitation, kind string) bool {
	for _, l := range lims {
		if l.Kind == kind {
			return true
		}
	}
	return false
}
