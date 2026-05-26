// Package csprovider implements the C# language provider for SuitCode.
//
// The import graph is derived exclusively from <ProjectReference> elements in .csproj
// files — the authoritative, compiler-verified project dependency declaration. No
// heuristics such as `using` directive parsing are used. Avalonia .axaml ↔ .axaml.cs
// code-behind pairs are linked as direct peers within the same project.
//
// When csharp-ls is installed, FileImporters uses LSP textDocument/references to
// provide file-level (not project-level) reverse dependency data. When csharp-ls is
// not available, FileImporters falls back to the .csproj <ProjectReference> graph
// and records a Limitation so callers know the result is coarser than optimal.
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

	// Attempt to start csharp-ls. Non-fatal if not installed or fails to initialize.
	if bin, err := findCsharpLs(); err == nil {
		client := newCsharpLspClient(bin, repoPath)
		if initErr := client.Initialize(ctx); initErr == nil {
			p.lspClient = client
		}
		// If Initialize fails, log silently and continue without LSP.
		// The provider still works via the .csproj fallback.
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
// When csharp-ls is available: uses textDocument/references for file-level
// precision — only files that actually reference a type in filePath.
//
// Fallback: returns files in projects that directly reference filePath's project
// (project-level granularity) and records a Limitation to inform callers.
func (p *CSHarpLanguageProvider) FileImporters(ctx context.Context, filePath string) (*provider.ProviderResult[[]string], error) {
	start := time.Now()

	// LSP path: file-level references via csharp-ls textDocument/references.
	if p.lspClient != nil {
		files, err := p.lspClient.FileReferences(ctx, filePath)
		if err == nil {
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
		// Fall through to .csproj fallback on LSP error.
	}

	// Fallback: project-level (coarse — all files in referencing projects).
	p.mu.RLock()
	ready := p.ready
	lims := copyLimitations(p.limitations)
	var files []string
	if ready {
		files = p.importerFilesFor(filePath)
	}
	p.mu.RUnlock()

	if !ready {
		return notReadyResult[[]string](lims), nil
	}

	// Append a limitation so callers know this is project-level, not file-level.
	lims = append(lims, provider.Limitation{
		Kind:    "project_level_importers",
		Message: "csharp-ls not available — importers are project-level (all files in referencing .csproj projects), not file-level. Run 'suitcode installdeps' to install csharp-ls for file-level importers.",
		Scope:   filePath,
	})

	return &provider.ProviderResult[[]string]{
		Data: files,
		Provenance: []provider.Provenance{{
			SourceKind:      provider.SourceKindManifest,
			SourceTool:      "csproj-project-reference",
			Authority:       provider.AuthorityDerived,
			EvidenceSummary: fmt.Sprintf("<ProjectReference> graph: files in projects that reference the project containing %s", filePath),
			EvidencePaths:   []string{filePath},
		}},
		Limitations: lims,
		DurationMs:  time.Since(start).Milliseconds(),
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

	// Include the Avalonia code-behind partner (.axaml ↔ .axaml.cs) as a peer.
	if partner, ok := p.idx.partners[filePath]; ok {
		result = append(result, partner)
	}
	return dedup(result)
}

// importerFilesFor returns the deduplicated set of files that "import" filePath
// (files in projects that reference its project), including its Avalonia partner.
// Caller must hold p.mu.RLock.
func (p *CSHarpLanguageProvider) importerFilesFor(filePath string) []string {
	result := make([]string, 0, len(p.idx.fileImporters[filePath])+1)
	result = append(result, p.idx.fileImporters[filePath]...)

	// Include the Avalonia code-behind partner as a peer.
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
