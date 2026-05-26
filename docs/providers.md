# SuitCode — Provider Capabilities

This document is the authoritative reference for every language provider: what it does, how it does it,
what it can't do yet, and what session experience taught us. Consult this before implementing a new
provider or modifying an existing one.

---

## Capability matrix

| Capability | Go | C# | TypeScript/JS | Python |
|---|---|---|---|---|
| FileImports (file-level) | ✓ gopls | ✓ .csproj (project-level) | ✓ static AST | ✓ static AST |
| FileImporters (file-level) | ✓ go/packages | **csharp-ls** textDocument/references | ✓ static AST | ✓ static AST |
| FilePeers | ✓ same package | ✓ same .csproj | — (each file is its own module) | — |
| FileTests | ✓ *_test.go | — (use FileImporters + framework detection) | — | — |
| FileSymbols | ✓ gopls documentSymbol | planned (csharp-ls) | planned (tsserver) | planned (pylsp) |
| Test file classification | ✓ _test.go suffix | ✓ .Tests/ dir, *Tests.cs filename | ✓ .test.ts, .spec.ts | ✓ /tests/, test_*.py |

**Bold** = recently upgraded or in progress.

---

## Go provider (`core/provider/language/go/`)

### Mechanism
- **Import graph:** `go/packages` with `NeedImports | NeedDeps | NeedFiles` — the Go compiler's own dependency resolver. Multi-module workspace aware (`go.work`).
- **Symbols:** `gopls` LSP via `textDocument/documentSymbol`. gopls is started as a subprocess on investigator init.

### LSP server
- **Binary:** `gopls`
- **Install:** `go install golang.org/x/tools/gopls@latest`
- **Startup:** `gopls -mode=stdio` — stdio JSON-RPC 2.0.
- **Lifetime:** Tied to the investigator.

### Capabilities (all file-level)
- **FileImports:** Files in packages directly imported by the seed file's package. Authoritative — same graph as `go build`.
- **FileImporters:** Files in packages that directly import the seed file's package. Authoritative.
- **FilePeers:** All other non-test `.go` files in the same package (same `package` declaration directory). Authoritative.
- **FileTests:** All `*_test.go` files in the same package directory. Authoritative.
- **FileSymbols:** Via gopls `textDocument/documentSymbol` + `textDocument/didOpen`. Returns hierarchical symbol tree.

### Known limitations
- Workspace must be loadable by `go/packages` (valid GOPATH or module mode, no build errors).
- Cross-module imports only work when `go.work` is present or the modules are on the module cache.
- gopls startup takes 3–10s on first warmup for large repos; subsequent calls are fast.

### Session learnings
- None specific yet — Go provider has been stable.

---

## C# provider (`core/provider/language/csharp/`)

### Mechanism
- **Project graph (Phase 1):** `.csproj` `<ProjectReference>` elements — the MSBuild project dependency graph. Parsed at index time.
- **File-level importers (Phase 2):** `csharp-ls` LSP `textDocument/references` — wraps Roslyn, the official C# compiler API. Returns exact file-level callers of each exported type in the seed.
- **Avalonia partner detection:** `.axaml` ↔ `.axaml.cs` code-behind pairs are linked as peers within the same project.

### LSP server
- **Binary:** `csharp-ls` ([github.com/razzmatazz/csharp-language-server](https://github.com/razzmatazz/csharp-language-server))
- **Install:** `dotnet tool install --global csharp-ls`
- **Requires:** .NET SDK in PATH (which is already required to build C# projects).
- **Startup:** `csharp-ls` — stdio JSON-RPC 2.0, workspace root passed via LSP `initialize.rootUri`.
- **Lifetime:** Tied to the investigator.

### Capabilities
- **FileImports:** Files in projects referenced by the seed file's project (project-level, from .csproj). This is the correct C# unit of compilation — project-level imports are authoritative at the project boundary.
- **FileImporters:** **When csharp-ls is available:** file-level via `textDocument/references` on each exported type. **Fallback (csharp-ls not installed):** project-level (all files in referencing projects) — produces noise, see Known Limitations.
- **FilePeers:** All other source files compiled in the same `.csproj` project. Authoritative from the .csproj manifest.
- **FileTests:** Not implemented — test projects are separate `.csproj` assemblies. Callers can use FileImporters and filter by test-framework package presence.
- **FileSymbols:** Not implemented — planned via `csharp-ls textDocument/documentSymbol`.

### Known limitations
- **Project-level FileImporters (fallback):** When `csharp-ls` is not installed, `FileImporters` returns ALL files in any project with a `<ProjectReference>` to the seed's project. For a 150-file `MGA.Desktop` project, this means every file (App.axaml, Program.cs, Themes/, LoadingSpinner.cs) floods in as an "importer" even if it never references the specific type. This is a granularity issue, not incorrect data — it's just too coarse. **Solution: install csharp-ls via `suitcode installdeps`.**
- No FileTests — test projects must be identified via FileImporters + framework detection.
- csharp-ls may take 10–30s to load a large solution on first use; subsequent calls are fast.

### Session learnings (MGA project, Phase 20–21)
- **Importer noise:** `MgaApiServiceTests.cs` and `SseEventBusTests.cs` were scoring 0.80 (Tier 1 production importers) because the C# provider didn't classify `.Tests/` directories as test files. **Fix:** extended `classifyFile` with C# test directory/filename conventions (`*.Tests/`, `*Tests.cs`).
- **Project-level flooding:** `App.axaml`, `Program.cs`, `LoadingSpinner.cs`, `TitleBar.cs`, `Themes/` all appeared as importers of `MgaApiService.cs` — even though they never reference it — because `MGA.Desktop.csproj` has a `<ProjectReference>` to `MGA.Core.csproj`. **Fix:** `csharp-ls textDocument/references` returns exact file-level callers.
- **Models.cs token growth:** A single seed file (`Models.cs`) grew to 8752 tokens (~50% of a typical budget). Splitting by domain area is a codebase concern, not a tooling one. Consider warning when a seed file exceeds X% of the requested budget.

### Implementation checklist for new features
- [ ] FileSymbols via csharp-ls `textDocument/documentSymbol`
- [ ] FileTests via FileImporters + test-framework package detection
- [ ] Seed file size warning (tokens > N% of budget)

---

## TypeScript/JavaScript provider (`core/provider/language/js/`)

### Mechanism
- **Import graph:** Static AST parsing of `import`/`require` statements + `tsconfig.json` path alias resolution. No LSP yet.

### LSP server (planned)
- **Binary:** `typescript-language-server`
- **Install:** `npm install -g typescript-language-server typescript`
- **Reason needed:** Dynamic imports, complex path aliases, barrel file re-exports, and symbol information all require the TypeScript compiler, not static AST.

### Capabilities (current — static AST)
- **FileImports:** Repo-local files directly imported by the seed. Good for simple relative imports; may miss aliased paths.
- **FileImporters:** Repo-local files that directly import the seed. Same accuracy caveat.
- **FilePeers:** Not implemented — in JS/TS each file is its own module.
- **FileTests:** Not implemented.
- **FileSymbols:** Not implemented.

### Known limitations
- Dynamic imports (`import(path)`) are not tracked.
- Complex tsconfig `paths` aliases may not resolve correctly.
- Barrel files (`index.ts` re-exporting many symbols) may cause import graph inflation.

### Planned improvements
- Upgrade FileImports/FileImporters to use `typescript-language-server` `textDocument/references`.
- Add FileSymbols via `textDocument/documentSymbol`.

---

## Python provider (`core/provider/language/python/`)

### Mechanism
- **Import graph:** Static AST parsing of `import` and `from X import Y` statements + relative import resolution. No LSP yet.

### LSP server (planned)
- **Binary:** `pylsp` (python-language-server)
- **Install:** `pip install python-lsp-server`
- **Alternative:** `pyright` (Microsoft) — stricter typing, better for typed codebases.
- **Reason needed:** Conditional imports, `__init__.py` aggregation, and star imports (`from X import *`) require execution-time analysis.

### Capabilities (current — static AST)
- **FileImports:** Repo-local `.py` files directly imported by the seed. Good for simple absolute imports.
- **FileImporters:** Repo-local `.py` files that directly import the seed.
- **FilePeers:** Not implemented.
- **FileTests:** Not implemented.
- **FileSymbols:** Not implemented.

### Known limitations
- `from X import *` is not tracked.
- Conditional imports (`if TYPE_CHECKING: import X`) may be missed.
- `__init__.py` aggregation can cause under-reporting (package-level importers vs file-level).

### Planned improvements
- Upgrade to `pylsp` or `pyright` for authoritative references.
- Add FileTests via pytest naming conventions (`test_*.py`, `*_test.py`).

---

## Implementing a new provider — checklist

When adding support for a new language, use this checklist. The minimum bar is **all four import graph methods implemented via LSP**; heuristics and static AST are not acceptable as final implementations.

### Phase 1 — Project structure (no LSP needed)
- [ ] Parse the build manifest (`.csproj`, `pom.xml`, `Cargo.toml`, etc.) to build a coarse project/package graph.
- [ ] Implement `FilePeers` from the manifest (files that compile together).
- [ ] Implement test file classification in `filesystem/provider.go:classifyFile` using the language's test conventions.

### Phase 2 — LSP integration (authoritative)
- [ ] Identify the official LSP server for the language.
- [ ] Add the LSP server binary to `suitcode installdeps`.
- [ ] Implement `FileImports` via `textDocument/definition` on import statements, or via the package graph + `workspace/symbol`.
- [ ] Implement `FileImporters` via `textDocument/references` on all exported types in the seed file.
- [ ] Implement `FileSymbols` via `textDocument/documentSymbol`.

### Phase 3 — FileTests (authoritative)
- [ ] Implement `FileTests` via the LSP reference graph + test framework detection, or via the build system's test target declarations.

### Quality bar
- All methods return `Provenance` explaining the tool and authority level.
- Non-implemented methods return a `not_implemented` Limitation (never return empty-but-silently).
- LSP server path is discovered via `suitcode installdeps` path detection, not hardcoded.
- LSP server is started once per investigator lifetime; `Close()` shuts it down.

---

## `suitcode installdeps` — managed dependencies

| LSP server | Language | Package manager | Command |
|---|---|---|---|
| `gopls` | Go | go install | `go install golang.org/x/tools/gopls@latest` |
| `csharp-ls` | C# | dotnet tool | `dotnet tool install --global csharp-ls` |
| `typescript-language-server` | TypeScript/JS | npm | `npm install -g typescript-language-server typescript` |
| `pylsp` | Python | pip | `pip install python-lsp-server` |

`installdeps` detects which package managers are available and which languages appear in the repository before deciding what to install. It never fails hard — missing package managers are reported, not fatal.
