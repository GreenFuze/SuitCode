# SuitCode — Project System Prompt

This file is loaded by Claude Code at the start of every session on this repository.
It captures non-negotiable engineering principles, key architecture decisions, and
development workflow. Read this before touching any code.

---

## Key reference documents

Always consult these before making architectural decisions:

- **[`docs/decisions.md`](docs/decisions.md)** — Every significant architecture decision: the challenge, the decision, and the rationale. Update this when making a non-obvious choice.
- **[`docs/providers.md`](docs/providers.md)** — Per-provider capability matrix, known limitations, session learnings, and the implementation checklist for new providers.

---

## Non-negotiable engineering principles

### 1. No heuristics — ever

If SuitCode cannot get an authoritative answer from a compiler, LSP server, build-system manifest, or other structured tool, it reports a `Limitation` and returns what it *does* know. It **never** silently falls back to regex scanning, directory-naming conventions, file proximity, or any other heuristic.

**Why:** Agents treat SuitCode output as facts. A wrong-but-confident answer causes cascading errors in code edits and refactors. An honest "I don't know" lets the agent fall back gracefully.

**In code:** Non-implemented capabilities return a `Limitation{Kind: "not_implemented", ...}`, never an empty result that looks authoritative.

### 2. LSP-first for every language provider

Every language provider **must** use the language's official LSP server as the authoritative source for import graphs, file references, and symbol information. LSP servers wrap the actual compiler — their answers are compiler-verified.

| Language | LSP server | Install | Prerequisite |
|---|---|---|---|
| Go | `gopls` | `go install golang.org/x/tools/gopls@latest` | Go toolchain |
| C# | `csharp-ls` (wraps Roslyn) | `dotnet tool install --global csharp-ls` | **.NET 10 SDK** |
| TypeScript/JavaScript | `typescript-language-server` | `npm install -g typescript-language-server typescript` | Node.js |
| Python | `pylsp` | `pip install python-lsp-server` | Python |

**C# note:** csharp-ls 0.22+ requires .NET 10 SDK (`winget install Microsoft.DotNet.SDK.10`). SuitCode requires the latest csharp-ls — .NET 10 is a hard prerequisite for C# support.

Static AST parsing (JS/TS, Python) and manifest-based graphs (.csproj) are **current primary implementations** for capabilities where no LSP integration exists yet — they are NOT fallbacks for a missing LSP tool. As LSP integration is added, the static implementations are replaced, not kept as degraded alternatives.

### 3. 3rd party tools are REQUIRED — no silent fallbacks, ever

This is the single most important policy in the codebase. It applies to **every** 3rd party tool SuitCode uses — LSP servers, compilers, runtimes, CLI tools, anything external:

- If a required 3rd party tool is not installed → return `Limitation{Kind: "tool_not_available", Message: "... run 'suitcode installdeps'"}` and **empty data**.
- If a tool is installed but a call fails → return `Limitation{Kind: "lsp_error" / "tool_error"}` and **empty data**.
- **Never** silently fall back to a lower-quality implementation (e.g. project-level instead of file-level, proximity heuristic instead of import graph).

**Why:** A degraded-quality fallback is worse than an honest "not available". The C# `.csproj` fallback for `FileImporters` (all files in referencing projects) was removed precisely because it returned 150 irrelevant files instead of 3 relevant ones — a confident wrong answer that corrupted context. The same logic applies to every external tool.

**In code:**
```go
if p.lspClient == nil {
    return &provider.ProviderResult[[]string]{
        Data: []string{},
        Limitations: []provider.Limitation{{
            Kind:    "tool_not_available",
            Message: "<tool> is required but not installed. Run 'suitcode installdeps'.",
            Scope:   filePath,
        }},
    }, nil
}
```

### 4. External tools are daemons tied to the investigator

Long-running tools (LSP servers, language daemons) are started **once** when the investigator initialises and stopped when it shuts down. They are **never** started per-call. Rules:
- No visible windows, no console output (stderr → coordinator log or discarded).
- Lifetime is tied to the investigator via `Close()`.
- The shared LSP transport in `core/lsp/transport.go` must be used for all LSP clients — never copy it into a provider.

### 5. `suitcode installdeps` / `suitcode verifydeps` manage external tools

All external tools (LSP servers and any other 3rd party binary SuitCode depends on) must be registered in `suitcode installdeps`. When adding a new tool dependency:

1. Add it to the `lspTools` slice in `suitcode/main.go`.
2. Implement fail-fast detection: if missing, return `tool_not_available`, never degrade.
3. Run `suitcode installdeps` on a new machine before `warmup`.
4. Use `suitcode verifydeps` in CI to assert the environment is complete.

Never silently degrade when a tool is absent. Never require users to figure out what's missing on their own.

### 6. Fail-fast with full provenance

Every `ProviderResult` carries:
- `Provenance` — which tool produced this data, at what `Authority` level.
- `Limitations` — anything the provider could not determine, with `Kind` and `Message`.

Do not return empty data silently. Do not swallow errors. Prefer a clear limitation over a partial silent result.

### 7. SuitCode is a CLI tool, not an MCP server

See `decisions.md §1` for the full rationale. The key point: agents call SuitCode directly via Bash/PowerShell. There is no MCP adapter, no IDE plugin, no protocol translation layer. Do not add one.

---

## Architecture at a glance

```
suitcode/           — CLI client (auto-starts coordinator on first use)
coordinator/        — HTTP routing daemon + desktop tray (-tags systray)
investigator/       — per-project daemon: file index, import graph, LSP connections, features
  features/         — context, explain-file, impact, tests, verify-plan, …
sessionanalysis/    — Claude Code session parser: extracts suitcode calls, quality signals
calllog/            — per-call metric log with feedback ratings
core/
  lsp/              — shared Content-Length framed JSON-RPC 2.0 transport (ALL providers use this)
  provider/
    language/go/    — Go: go/packages + gopls LSP
    language/csharp/— C#: .csproj graph + csharp-ls LSP
    language/js/    — JS/TS: static AST (→ typescript-language-server planned)
    language/python/— Python: static AST (→ pylsp planned)
    language/multi/ — composite provider for polyglot repos
    filesystem/     — file listing with .gitignore + build-artifact exclusion
    vcs/            — git status and diff
```

---

## When adding a new language provider

1. Read `docs/providers.md` — the implementation checklist is there.
2. Phase 1: manifest-based project graph + `FilePeers` + test classification in `classifyFile`.
3. Phase 2: LSP integration — `FileImports`, `FileImporters` via `textDocument/references`, `FileSymbols`.
4. Add the LSP server to `suitcode installdeps`.
5. Document the provider in `docs/providers.md` (mechanism, capabilities, limitations, install command).
6. Use `core/lsp/transport.go` — do not write a new transport.

The minimum bar for a provider to be considered complete is **all four import graph methods implemented via LSP** (not static AST).

---

## Development workflow

```sh
./dev-install.sh             # build + install all three binaries
./dev-install.sh --restart   # rebuild and restart coordinator (kills suitcode/coordinator/investigator first)
./dev-install.sh --ci        # CGo-free headless build (no tray)

suitcode installdeps         # install required LSP servers for this machine
suitcode . warmup            # warm the investigator (run this before any other command)
```

State is written to `.suitcode/` in each analysed repo. Add it to `.gitignore`.

---

## Key reminders

- The shared LSP transport is `core/lsp/transport.go`. **Never** copy it into a provider package.
- **No silent fallbacks, ever.** If a 3rd party tool is not installed or fails, return `Limitation{Kind: "tool_not_available"}` + empty data. This is non-negotiable — see Principle 3. The C# `.csproj` project-level fallback for `FileImporters` was removed for this reason.
- Every new external tool dependency **must** be added to `suitcode installdeps` (in `suitcode/main.go`). Use `suitcode verifydeps` to validate the environment.
- `suitcode . feedback good|bad` should follow every feature call. Session analysis in the tray gives an independent edit-rate signal.
- Run `suitcode installdeps` before `warmup` on any new machine or after adding a new language provider.
- When a result is imprecise, check `docs/providers.md` — the limitation is probably already documented with a planned fix.
