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

| Language | LSP server | Install |
|---|---|---|
| Go | `gopls` | `go install golang.org/x/tools/gopls@latest` |
| C# | `csharp-ls` (wraps Roslyn) | `dotnet tool install --global csharp-ls` |
| TypeScript/JavaScript | `typescript-language-server` | `npm install -g typescript-language-server typescript` |
| Python | `pylsp` | `pip install python-lsp-server` |

Static AST parsing and .csproj graph parsing are acceptable as **fallbacks only** when the LSP server is not installed. They must be clearly labelled in `Provenance.Authority` as `AuthorityDerived`, never `AuthorityVerified`.

### 3. LSP servers are daemons tied to the investigator

LSP servers are started **once** when the investigator initialises, and stopped when the investigator shuts down. They are **never** started per-call. Rules:
- No visible windows, no console output (stderr → coordinator log or discarded).
- Lifetime is tied to the investigator via `Close()`.
- The shared transport in `core/lsp/transport.go` must be used — never copy it into a provider.

### 4. `suitcode installdeps` manages external dependencies

External LSP servers are not expected to be pre-installed. `suitcode installdeps` detects which languages are present in the repo and installs the required LSP servers using the language's native package manager. Run this on a new machine before `warmup`.

Never require a user to manually install a dependency that `installdeps` can handle.

### 5. Fail-fast with full provenance

Every `ProviderResult` carries:
- `Provenance` — which tool produced this data, at what `Authority` level.
- `Limitations` — anything the provider could not determine, with `Kind` and `Message`.

Do not return empty data silently. Do not swallow errors. Prefer a clear limitation over a partial silent result.

### 6. SuitCode is a CLI tool, not an MCP server

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
- C# `FileImporters` uses `csharp-ls textDocument/references` when available. The .csproj fallback is intentionally coarser and will flood the response with noise — see `docs/decisions.md §9`.
- `suitcode . feedback good|bad` should follow every feature call. Session analysis in the tray gives an independent edit-rate signal.
- Run `suitcode installdeps` before `warmup` on any new machine or after adding a new language provider.
- When a result is imprecise, check `docs/providers.md` — the limitation is probably already documented with a planned fix.
