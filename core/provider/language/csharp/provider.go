// Package csprovider implements the C# language provider for SuitCode.
//
// The import graph is derived exclusively from <ProjectReference> elements in .csproj
// files — the authoritative, compiler-verified project dependency declaration. No
// heuristics such as `using` directive parsing are used. Avalonia .axaml ↔ .axaml.cs
// code-behind pairs are linked as direct peers within the same project.
//
// csharp-ls (the Roslyn-based LSP server) is REQUIRED for FileImporters. If it is
// not installed, FileImporters returns a "tool_not_available" Limitation and empty
// data — it does NOT fall back to the coarse .csproj project-level graph, which
// would flood callers with irrelevant files. Run "suitcode installdeps" to install
// csharp-ls via "dotnet tool install --global csharp-ls".
package csprovider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/GreenFuze/SuitCode/core/provider"
)

const providerID provider.ProviderID = "csharp-language"

// CSHarpLanguageProvider implements provider.ImportGraphProvider for C# repositories,
// including Avalonia UI projects.
//
// CSHarpLanguageProvider is safe for concurrent use after construction.
type CSHarpLanguageProvider struct {
	repoPath  string
	lspClient *csharpLspClient // nil when csharp-ls is not installed

	mu          sync.RWMutex
	idx         *csImportIndex // nil until load succeeds
	limitations []provider.Limitation
	ready       bool
	loadOnce    sync.Once
}

// NewCSHarpLanguageProvider creates a CSHarpLanguageProvider for the given repository
// root. The import graph (Phase 1) is built synchronously before returning.
// An error is returned only when repoPath is not a valid directory.
// All other failures (no .csproj files, parse errors) are captured as Limitations
// and do not prevent the provider from being returned.
func NewCSHarpLanguageProvider(ctx context.Context, repoPath string) (*CSHarpLanguageProvider, error) {
	info, err := os.Stat(repoPath)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("csharp language provider: path is not a valid directory: %s", repoPath)
	}

	p := &CSHarpLanguageProvider{repoPath: repoPath}

	// Attempt to start csharp-ls. If not installed or initialization fails,
	// record a limitation so Status() surfaces the gap to operators and agents.
	// FileImporters will return "tool_not_available" when lspClient is nil.
	if bin, err := findCsharpLs(); err != nil {
		p.limitations = append(p.limitations, provider.Limitation{
			Kind:    "tool_not_available",
			Message: "csharp-ls is not installed — FileImporters will return empty results. Run 'suitcode installdeps' to install it.",
			Scope:   repoPath,
		})
	} else {
		client := newCsharpLspClient(bin, repoPath)
		if initErr := client.Initialize(ctx); initErr != nil {
			p.limitations = append(p.limitations, provider.Limitation{
				Kind:    "lsp_init_failed",
				Message: fmt.Sprintf("csharp-ls failed to initialize: %v — FileImporters will return empty results.", initErr),
				Scope:   repoPath,
			})
		} else {
			p.lspClient = client
		}
	}

	p.ensureLoaded()
	return p, nil
}

// findCsharpLs locates the csharp-ls binary using:
//  1. exec.LookPath("csharp-ls") — standard PATH lookup
//  2. Platform-specific dotnet global tools directory
//
// Returns an error when csharp-ls is not found in any of these locations.
func findCsharpLs() (string, error) {
	// 1. Standard PATH lookup.
	if path, err := exec.LookPath("csharp-ls"); err == nil {
		return path, nil
	}

	// 2. Platform-specific dotnet global tools directory.
	var toolPath string
	if runtime.GOOS == "windows" {
		home := os.Getenv("USERPROFILE")
		if home != "" {
			toolPath = filepath.Join(home, ".dotnet", "tools", "csharp-ls.exe")
		}
	} else {
		home, err := os.UserHomeDir()
		if err == nil {
			toolPath = filepath.Join(home, ".dotnet", "tools", "csharp-ls")
		}
	}

	if toolPath != "" {
		if _, err := os.Stat(toolPath); err == nil {
			return toolPath, nil
		}
	}

	return "", fmt.Errorf("csharp-ls not found in PATH or dotnet global tools directory")
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.Provider implementation
// ──────────────────────────────────────────────────────────────────────────────

// Capabilities returns the static metadata for this provider.
func (p *CSHarpLanguageProvider) Capabilities() provider.ProviderCapabilities {
	return provider.ProviderCapabilities{
		ID:          providerID,
		DisplayName: "C# Language Provider (csproj graph + csharp-ls LSP for file-level references)",
		Roles:       []provider.ProviderRole{provider.RoleLanguage},
		Languages:   []string{"C#", "Avalonia XAML"},
	}
}

// Ready reports whether the Phase 1 import graph was built with at least one
// C# source file found. Returns false for repos with no .csproj files.
func (p *CSHarpLanguageProvider) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ready
}

// Close shuts down the LSP client (if running) and releases resources.
// Safe to call multiple times.
func (p *CSHarpLanguageProvider) Close() error {
	if p.lspClient != nil {
		p.lspClient.Shutdown(context.Background())
	}
	return nil
}

// DaemonInfo returns information about the csharp-ls subprocess managed by
// this provider. Returns a not-running DaemonInfo when csharp-ls is not available.
func (p *CSHarpLanguageProvider) DaemonInfo() provider.DaemonInfo {
	if p.lspClient == nil {
		return provider.DaemonInfo{Name: "csharp-ls", Running: false}
	}
	return provider.DaemonInfo{
		Name:    "csharp-ls",
		Binary:  p.lspClient.transport.BinaryPath(),
		Running: p.lspClient.transport.Running(),
		PID:     p.lspClient.transport.PID(),
	}
}

// WaitForDaemons implements provider.DaemonWaiter. csharp-ls is initialised
// synchronously in the constructor (Initialize blocks until the LSP handshake
// completes), so this method returns immediately. It exists so the dispatcher
// can treat all providers uniformly via the DaemonWaiter interface, and so
// that future async startup can be added here without changing call-sites.
func (p *CSHarpLanguageProvider) WaitForDaemons(_ context.Context) bool {
	return p.lspClient != nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.LanguageProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// GetImports returns the absolute paths of files in projects that filePath's
// project directly references. This is the closest C# analogue to "imports".
// The Avalonia partner (.axaml ↔ .axaml.cs) is included when present.
func (p *CSHarpLanguageProvider) GetImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := copyLimitations(p.limitations)
	var files []string
	if ready {
		files = p.importedFilesFor(filePath)
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindManifest,
			SourceTool:      "csproj-project-reference",
			Authority:       provider.AuthorityDerived,
			EvidenceSummary: fmt.Sprintf("<ProjectReference> graph: files in projects referenced by the project containing %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// GetSymbols is not implemented — symbol extraction for C# requires Roslyn or an
// LSP server, which are not available without a .NET SDK in PATH.
// Always returns a "not_implemented" Limitation rather than an empty-but-authoritative
// result, so callers know the absence of symbols is a tooling gap, not an empty file.
func (p *CSHarpLanguageProvider) GetSymbols(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	p.mu.RLock()
	lims := copyLimitations(p.limitations)
	p.mu.RUnlock()

	lims = append(lims, provider.Limitation{
		Kind:    "not_implemented",
		Message: "symbol extraction for C# requires Roslyn or a Language Server Protocol server — not yet integrated into SuitCode",
		Scope:   filePath,
	})
	return &provider.ProviderResult[[]string]{Limitations: lims}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// provider.ImportGraphProvider implementation
// ──────────────────────────────────────────────────────────────────────────────

// FileImports returns the absolute paths of files in projects that filePath's
// project directly references. Avalonia partner is included when present.
func (p *CSHarpLanguageProvider) FileImports(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := copyLimitations(p.limitations)
	var files []string
	if ready {
		files = p.importedFilesFor(filePath)
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindManifest,
			SourceTool:      "csproj-project-reference",
			Authority:       provider.AuthorityDerived,
			EvidenceSummary: fmt.Sprintf("<ProjectReference> graph: files directly accessible from project containing %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// GetPackageRefs returns the NuGet <PackageReference> items declared in the
// .csproj that contains filePath. Returns nil when the provider is not ready
// or filePath is not part of any tracked C# project.
func (p *CSHarpLanguageProvider) GetPackageRefs(filePath string) []PackageRef {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.ready {
		return nil
	}
	return p.idx.filePackageRefs[filePath]
}

// FileImporters returns the absolute paths of files that reference exported
// types defined in filePath.
//
// Requires csharp-ls: uses textDocument/references for file-level precision —
// only files that actually reference an exported type in filePath are returned.
//
// Fail-fast: if csharp-ls is not installed, returns empty data and a
// "tool_not_available" Limitation. No .csproj project-level fallback is used —
// that fallback returns every file in any referencing project (including
// unrelated files like App.axaml, Program.cs, Themes/) and produces noisy,
// misleading context. Run "suitcode installdeps" to install csharp-ls.
func (p *CSHarpLanguageProvider) FileImporters(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	// Fail fast when csharp-ls is not available.
	if p.lspClient == nil {
		return &provider.ProviderResult[[]string]{
			Data: []string{},
			Limitations: []provider.Limitation{{
				Kind:    "tool_not_available",
				Message: "csharp-ls is required for file-level importers but is not installed. Run 'suitcode installdeps' to install it via 'dotnet tool install --global csharp-ls'.",
				Scope:   filePath,
			}},
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	// LSP path: file-level references via csharp-ls textDocument/references.
	files, err := p.lspClient.FileReferences(ctx, filePath)
	if err != nil {
		// LSP error — return the error as a limitation, not as a Go error.
		// No fallback: a failed LSP call does not justify returning the
		// entire project's files (which would be far worse than nothing).
		return &provider.ProviderResult[[]string]{
			Data: []string{},
			Limitations: []provider.Limitation{{
				Kind:    "lsp_error",
				Message: fmt.Sprintf("csharp-ls textDocument/references failed for %s: %v", filePath, err),
				Scope:   filePath,
			}},
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindLSP,
			SourceTool:      "csharp-ls:textDocument/references",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("csharp-ls references: files that reference exported types in %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// FilePeers returns the absolute paths of all other source files in the same
// .csproj project as filePath. These files compile into the same assembly —
// a manifest fact, not a filesystem heuristic.
func (p *CSHarpLanguageProvider) FilePeers(_ context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	p.mu.RLock()
	ready := p.ready
	lims := copyLimitations(p.limitations)
	var files []string
	if ready {
		files = p.idx.filePeers[filePath]
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindManifest,
			SourceTool:      "csproj-project-manifest",
			Authority:       provider.AuthorityVerified,
			EvidenceSummary: fmt.Sprintf("source files compiled in the same .csproj project as %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
	}, nil
}

// FileTests returns an empty result for C# — test projects are separate
// .csproj assemblies whose relationship to the code project is expressed via
// <ProjectReference>. Use FileImporters to find projects that reference this
// project, then filter by test framework package presence.
func (p *CSHarpLanguageProvider) FileTests(_ context.Context, _ string) (*provider.ProviderResult[[]string], error) {
	return &provider.ProviderResult[[]string]{Data: []string{}}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ──────────────────────────────────────────────────────────────────────────────

// ensureLoaded builds the import graph exactly once via sync.Once.
func (p *CSHarpLanguageProvider) ensureLoaded() {
	p.loadOnce.Do(func() {
		idx, lims, err := buildCSImportGraph(p.repoPath)

		p.mu.Lock()
		defer p.mu.Unlock()

		p.limitations = append(p.limitations, lims...)

		if err != nil {
			p.limitations = append(p.limitations, provider.Limitation{
				Kind:    "cs_graph_load_failed",
				Message: fmt.Sprintf("import graph build failed: %v", err),
				Scope:   p.repoPath,
			})
			return
		}

		// Not ready when no C# source files were found — nothing to offer.
		if idx.sourceFileCount == 0 {
			return
		}

		p.idx = idx
		p.ready = true
	})
}

// importedFilesFor returns the deduplicated set of files "imported" by filePath
// (files in projects its project references), including its Avalonia partner if any.
// Caller must hold p.mu.RLock.
func (p *CSHarpLanguageProvider) importedFilesFor(filePath string) []string {
	result := make([]string, 0, len(p.idx.fileImports[filePath])+1)
	result = append(result, p.idx.fileImports[filePath]...)

	// Include the Avalonia code-behind partner (.axaml ↔ .axaml.cs) as an import
	// (Tier 1) so the pair is never separated by budget trimming.
	if partner, ok := p.idx.partners[filePath]; ok {
		result = append(result, partner)
	}
	return dedup(result)
}

// copyLimitations returns a shallow copy of accumulated limitations.
// Caller must hold p.mu.RLock before calling.
func copyLimitations(lims []provider.Limitation) []provider.Limitation {
	if len(lims) == 0 {
		return nil
	}
	out := make([]provider.Limitation, len(lims))
	copy(out, lims)
	return out
}

// notReadyResult returns a ProviderResult with a "provider_not_ready" Limitation
// plus any accumulated load-time limitations.
func notReadyResult[T any](accumulated []provider.Limitation) *provider.ProviderResult[T] {
	lims := make([]provider.Limitation, 0, 1+len(accumulated))
	lims = append(lims, provider.Limitation{
		Kind:    "provider_not_ready",
		Message: "CSHarpLanguageProvider is not ready — no .csproj files found or import graph failed to load",
	})
	lims = append(lims, accumulated...)
	return &provider.ProviderResult[T]{Limitations: lims}
}

// ──────────────────────────────────────────────────────────────────────────────
// Compile-time interface assertions
// ──────────────────────────────────────────────────────────────────────────────

var _ provider.LanguageProvider = (*CSHarpLanguageProvider)(nil)
var _ provider.ImportGraphProvider = (*CSHarpLanguageProvider)(nil)
var _ provider.DaemonWaiter = (*CSHarpLanguageProvider)(nil)
