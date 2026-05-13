package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
)

// ──────────────────────────────────────────────────────────────────────────────
// Repo-overview correctness
// ──────────────────────────────────────────────────────────────────────────────

// TestRepoOverview_Content verifies that repo-overview detects the correct
// languages, build systems, test systems, and file count for the SuitCode repo.
func TestRepoOverview_Content(t *testing.T) {
	skipIfShort(t, "requires go/packages load via full investigator")

	inv := sharedInv(t)
	ctx := context.Background()

	resp, err := inv.RepoOverview(ctx, cfeatures.RepoOverviewRequest{
		BaseFeatureRequest: cfeatures.BaseFeatureRequest{
			RepoPath: inv.repoPath,
			Budget:   10_000,
		},
	})
	if err != nil {
		t.Fatalf("RepoOverview: %v", err)
	}

	// Languages
	if !sliceContains(resp.Languages, "Go") {
		t.Errorf("expected 'Go' in Languages, got %v", resp.Languages)
	}
	if !sliceContains(resp.Languages, "Markdown") {
		t.Errorf("expected 'Markdown' in Languages, got %v", resp.Languages)
	}

	// Build systems
	if !sliceContains(resp.BuildSystems, "Go Modules") {
		t.Errorf("expected 'Go Modules' in BuildSystems, got %v", resp.BuildSystems)
	}

	// Test systems
	if !sliceContains(resp.TestSystems, "Go test") {
		t.Errorf("expected 'Go test' in TestSystems, got %v", resp.TestSystems)
	}

	// File count sanity check — SuitCode has > 40 files
	if resp.TotalFiles < 40 {
		t.Errorf("expected TotalFiles >= 40, got %d", resp.TotalFiles)
	}

	// Top-level structure must contain key directories
	topNames := make(map[string]bool, len(resp.TopLevelStructure))
	for _, e := range resp.TopLevelStructure {
		topNames[e.RelPath] = true
	}
	for _, required := range []string{"core", "investigator"} {
		if !topNames[required] {
			t.Errorf("expected top-level entry %q, got entries: %v", required, topLevelNames(resp.TopLevelStructure))
		}
	}
}

// TestRepoOverview_Determinism verifies that repo-overview produces the same
// content hash across multiple consecutive runs.
func TestRepoOverview_Determinism(t *testing.T) {
	skipIfShort(t, "runs repo-overview 5 times to verify hash stability")

	inv := sharedInv(t)
	ctx := context.Background()

	const runs = 5
	hashes := make([]string, 0, runs)

	for i := 0; i < runs; i++ {
		resp, err := inv.RepoOverview(ctx, cfeatures.RepoOverviewRequest{
			BaseFeatureRequest: cfeatures.BaseFeatureRequest{
				RepoPath: inv.repoPath,
				Budget:   3000,
			},
		})
		if err != nil {
			t.Fatalf("run %d: RepoOverview: %v", i+1, err)
		}
		hashes = append(hashes, resp.Metrics.DeterministicHash)
	}

	for i := 1; i < runs; i++ {
		if hashes[i] != hashes[0] {
			t.Errorf("hash changed between run 1 (%s) and run %d (%s) — non-deterministic output",
				hashes[0], i+1, hashes[i])
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Context capsule correctness
// ──────────────────────────────────────────────────────────────────────────────

// TestContext_Determinism verifies the context capsule produces a stable hash
// for the same seed across repeated calls.
func TestContext_Determinism(t *testing.T) {
	skipIfShort(t, "runs context 5 times to verify hash stability")

	inv := sharedInv(t)
	ctx := context.Background()
	seed := "investigator/investigator.go"

	const runs = 5
	hashes := make([]string, 0, runs)

	for i := 0; i < runs; i++ {
		resp, err := inv.Context(ctx, cfeatures.ContextRequest{
			BaseFeatureRequest: cfeatures.BaseFeatureRequest{
				RepoPath: inv.repoPath,
				Budget:   4000,
			},
			Files: []string{seed},
		})
		if err != nil {
			t.Fatalf("run %d: Context: %v", i+1, err)
		}
		hashes = append(hashes, resp.Metrics.DeterministicHash)
	}

	for i := 1; i < runs; i++ {
		if hashes[i] != hashes[0] {
			t.Errorf("context hash changed between run 1 (%s) and run %d (%s) for seed %q",
				hashes[0], i+1, hashes[i], seed)
		}
	}
}

// TestContext_ForwardImportInclusion verifies that context for a source file
// includes files from packages that file's package directly imports.
func TestContext_ForwardImportInclusion(t *testing.T) {
	skipIfShort(t, "requires go/packages import graph")

	inv := sharedInv(t)
	ctx := context.Background()

	// investigator/features/context.go imports core/features and core/provider.
	resp, err := inv.Context(ctx, cfeatures.ContextRequest{
		BaseFeatureRequest: cfeatures.BaseFeatureRequest{
			RepoPath: inv.repoPath,
			Budget:   200_000,
		},
		Files: []string{"investigator/features/context.go"},
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}

	t.Logf("context for investigator/features/context.go: %d files included", resp.FilesIncluded)
	t.Logf("import edges scanned: %d, lsp_enhanced: %v",
		resp.Metrics.ContextReduction.ImportEdgesScanned,
		resp.Metrics.ContextReduction.LspEnhanced)

	required := []string{"core/features/context.go", "core/provider/roles.go"}
	for _, want := range required {
		if !fileExistsInList(resp.IncludedRelPaths, want) {
			t.Errorf("expected %q in capsule, included: %v", want, resp.IncludedRelPaths)
		}
	}
}

// TestContext_SamePackageCohesion verifies that all files in a small package
// travel together in the context capsule when budget allows.
func TestContext_SamePackageCohesion(t *testing.T) {
	skipIfShort(t, "requires go/packages import graph")

	inv := sharedInv(t)
	ctx := context.Background()

	// eval/runner.go is in the investigator/eval package with suites.go and eval.go.
	resp, err := inv.Context(ctx, cfeatures.ContextRequest{
		BaseFeatureRequest: cfeatures.BaseFeatureRequest{
			RepoPath: inv.repoPath,
			Budget:   40_000,
		},
		Files: []string{"investigator/eval/runner.go"},
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}

	t.Logf("context for eval/runner.go: %d files included", resp.FilesIncluded)

	samePackage := []string{
		"investigator/eval/suites.go",
		"investigator/eval/eval.go",
	}
	for _, want := range samePackage {
		if !fileExistsInList(resp.IncludedRelPaths, want) {
			t.Errorf("same-package file %q missing from capsule; included: %v",
				want, resp.IncludedRelPaths)
		}
	}
}

// TestContext_UnrelatedFileExcluded verifies that a file with no import
// relationship to the seed is not included in the capsule.
func TestContext_UnrelatedFileExcluded(t *testing.T) {
	skipIfShort(t, "requires go/packages import graph")

	inv := sharedInv(t)
	ctx := context.Background()

	// lsp_types.go (gopls provider) has no import relationship with eval/eval.go.
	resp, err := inv.Context(ctx, cfeatures.ContextRequest{
		BaseFeatureRequest: cfeatures.BaseFeatureRequest{
			RepoPath: inv.repoPath,
			Budget:   200_000,
		},
		Files: []string{"core/provider/language/go/lsp_types.go"},
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}

	forbidden := "investigator/eval/eval.go"
	if fileExistsInList(resp.IncludedRelPaths, forbidden) {
		t.Errorf("unrelated file %q incorrectly included in capsule for lsp_types.go; "+
			"this indicates false-positive import-graph scoring", forbidden)
	}
}

// TestContext_BudgetRespected verifies the capsule never exceeds the requested
// token budget even when there are many candidate files.
func TestContext_BudgetRespected(t *testing.T) {
	skipIfShort(t, "requires full investigator with file listing")

	inv := sharedInv(t)
	ctx := context.Background()

	const tightBudget = 500

	resp, err := inv.Context(ctx, cfeatures.ContextRequest{
		BaseFeatureRequest: cfeatures.BaseFeatureRequest{
			RepoPath: inv.repoPath,
			Budget:   tightBudget,
		},
		Files: []string{"investigator/investigator.go"},
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}

	if resp.Metrics.Budget.Used > tightBudget {
		t.Errorf("budget exceeded: used %d tokens against limit %d",
			resp.Metrics.Budget.Used, tightBudget)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Import graph correctness
// ──────────────────────────────────────────────────────────────────────────────

// TestGetImports_GoplsPackage verifies that the gopls provider package's imports
// include core/provider (the base provider package it depends on).
func TestGetImports_GoplsPackage(t *testing.T) {
	skipIfShort(t, "requires go/packages load")

	inv := sharedInv(t)
	if inv.langProvider == nil {
		t.Skip("no language provider available")
	}

	ctx := context.Background()
	absPath := filepath.Join(inv.repoPath, "core", "provider", "language", "go", "provider.go")

	result, err := inv.langProvider.GetImports(ctx, absPath)
	if err != nil {
		t.Fatalf("GetImports: %v", err)
	}
	if !inv.langProvider.Ready() {
		t.Skip("language provider not ready")
	}

	// The gopls provider package imports core/provider.
	var hasProviderImport bool
	for _, imp := range result.Data {
		if strings.Contains(imp, "GreenFuze/SuitCode/core/provider") &&
			!strings.Contains(imp, "language") {
			hasProviderImport = true
			break
		}
	}
	if !hasProviderImport {
		t.Errorf("expected core/provider import in gopls package, got: %v", result.Data)
	}

	t.Logf("gopls package imports: %v", result.Data)
}

// TestFileImporters_CoreProvider verifies that the reverse-import index
// correctly identifies files that import the core/provider package.
func TestFileImporters_CoreProvider(t *testing.T) {
	skipIfShort(t, "requires go/packages load for reverse import index")

	inv := sharedInv(t)
	if inv.langProvider == nil {
		t.Skip("no language provider available")
	}

	ctx := context.Background()
	absPath := filepath.Join(inv.repoPath, "core", "provider", "roles.go")

	result, err := inv.langProvider.FileImporters(ctx, absPath)
	if err != nil {
		t.Fatalf("FileImporters: %v", err)
	}
	if !inv.langProvider.Ready() {
		t.Skip("language provider not ready")
	}

	t.Logf("importers of core/provider: %d files", len(result.Data))

	// The filesystem provider imports core/provider.
	var hasFilesystem bool
	for _, f := range result.Data {
		if strings.Contains(osPathToSlash(f), "core/provider/filesystem/") {
			hasFilesystem = true
			break
		}
	}
	if !hasFilesystem {
		t.Errorf("expected core/provider/filesystem/ files in importers of core/provider; "+
			"got: %v", result.Data)
	}

	// The investigator also imports core/provider.
	var hasInvestigator bool
	for _, f := range result.Data {
		if strings.Contains(osPathToSlash(f), "investigator/") &&
			!strings.Contains(osPathToSlash(f), "_test") {
			hasInvestigator = true
			break
		}
	}
	if !hasInvestigator {
		t.Errorf("expected investigator/ files in importers of core/provider; got: %v", result.Data)
	}
}

// TestFileImports_EvalPackage verifies that the investigator/eval package's
// imports include core/features (which the runner depends on).
func TestFileImports_EvalPackage(t *testing.T) {
	skipIfShort(t, "requires go/packages load")

	inv := sharedInv(t)
	if inv.langProvider == nil {
		t.Skip("no language provider available")
	}

	ctx := context.Background()
	absPath := filepath.Join(inv.repoPath, "investigator", "eval", "runner.go")

	result, err := inv.langProvider.FileImports(ctx, absPath)
	if err != nil {
		t.Fatalf("FileImports: %v", err)
	}
	if !inv.langProvider.Ready() {
		t.Skip("language provider not ready")
	}

	t.Logf("files imported by investigator/eval: %d", len(result.Data))

	// eval/runner.go imports core/features.
	var hasCoreFeatures bool
	for _, f := range result.Data {
		if strings.Contains(osPathToSlash(f), "core/features/") {
			hasCoreFeatures = true
			break
		}
	}
	if !hasCoreFeatures {
		t.Errorf("expected core/features/ files in imports of eval/runner.go; got: %v", result.Data)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// GetFileSymbols correctness (gopls-backed, skips gracefully if not ready)
// ──────────────────────────────────────────────────────────────────────────────

// TestGetFileSymbols_InvestigatorFile verifies that GetFileSymbols returns
// sensible symbols for the main investigator file when gopls is available.
func TestGetFileSymbols_InvestigatorFile(t *testing.T) {
	skipIfShort(t, "requires gopls subprocess")

	inv := sharedInv(t)
	ctx := context.Background()

	// Wait up to 30 s for gopls to start.
	deadline := time.Now().Add(30 * time.Second)
	for inv.langProvider != nil && !inv.langProvider.GoplsReady() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}

	if inv.langProvider == nil || !inv.langProvider.GoplsReady() {
		t.Skip("gopls not ready after 30s — skipping symbol test")
	}

	absPath := filepath.Join(inv.repoPath, "investigator", "investigator.go")
	names, err := inv.GetFileSymbols(ctx, absPath)
	if err != nil {
		t.Fatalf("GetFileSymbols: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("GetFileSymbols returned no symbols for investigator.go")
	}

	t.Logf("GetFileSymbols returned %d symbols", len(names))

	// Key symbols expected in investigator.go.
	// NOTE: Warm/Invalidate are defined in warmup.go, not this file.
	expected := []string{"ProjectInvestigator", "NewProjectInvestigator", "Close", "Status", "GetFileSymbols"}
	for _, want := range expected {
		if !symbolInList(names, want) {
			t.Errorf("expected symbol %q in investigator.go symbols; got: %v", want, names)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// topLevelNames extracts the RelPath from each DirectoryEntry for error messages.
func topLevelNames(entries []cfeatures.DirectoryEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.RelPath
	}
	return names
}

// symbolInList reports whether any name in names matches want exactly, or has
// want as a "."-separated suffix (handling gopls's "(*Receiver).Method" format).
func symbolInList(names []string, want string) bool {
	suffix := "." + want
	for _, n := range names {
		if n == want {
			return true
		}
		if len(n) > len(suffix) && n[len(n)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}
