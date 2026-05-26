# SuitCode — Architecture Decisions

This document captures every significant architectural decision made during the development of SuitCode,
including the challenge it solved and the rationale for the chosen approach. Update this file whenever
a non-obvious decision is made, so future contributors (and LLM agents) don't re-litigate settled choices.

---

## 1. CLI binary, not MCP server

**Challenge:** How should coding agents (Claude Code, Codex, Cursor) integrate with SuitCode?

**Decision:** SuitCode is a standalone CLI binary that agents invoke directly via Bash/PowerShell.

**Rationale:**
- MCP adds an abstraction layer and requires the host IDE/agent to route requests through a protocol adapter. This means deployment complexity, version coupling, and IDE-specific wiring.
- A CLI binary works in every agent environment that can run a shell command — no special integration, no config, no server registration.
- Agents already know how to call CLI tools and parse their output (JSON on stdout, progress on stderr).
- The coordinator auto-starts on first use; from the agent's perspective it's just a command that always works.

---

## 2. Coordinator + Investigator daemon model

**Challenge:** Building the import graph (go/packages, LSP warmup, .csproj parsing) takes 5–30 seconds. Repeating this per-call is unacceptable.

**Decision:** A two-tier daemon model:
- **Coordinator** — long-lived HTTP routing daemon, manages investigator lifetimes, port assignments, health checks.
- **Investigator** — one per project, holds the warmed file index, import graph, and LSP connections. Lives as long as the coordinator.

**Rationale:**
- Warm once, answer fast. The import graph is built on `warmup` and cached in memory for the investigator's lifetime.
- Multiple agents working on the same project share one investigator — no duplicate work.
- The coordinator handles auto-start, crash recovery, and cleanup transparently.
- Coordinator exposes an HTTP API, so the CLI (`suitcode`) is stateless and can be a thin wrapper.

---

## 3. Zero heuristics — authoritative or explicit limitation

**Challenge:** It's tempting to fall back to directory-proximity, naming conventions, or regex when a compiler-backed answer isn't available.

**Decision:** If SuitCode cannot get an authoritative answer from a compiler, LSP server, build-system manifest, or structured tool, it reports a `Limitation` and returns what it knows. It never silently returns a guessed answer as if it were verified.

**Rationale:**
- Agents treat SuitCode output as facts and make irreversible decisions (file edits, refactors) based on them.
- A wrong-but-confident answer pollutes context and causes cascading errors. An honest "not available" lets the agent fall back gracefully.
- Every result carries a `Provenance` block (`SourceKind`, `SourceTool`, `Authority`) so the agent always knows *why* a file was included.

**Key phrase:** "Authoritative or explicit limitation. Never a confident guess."

---

## 3a. No fallback for missing 3rd party tools — fail fast with `tool_not_available`

**Challenge:** When a required 3rd party tool (LSP server, compiler, runtime, CLI binary) is not installed, it's tempting to return a degraded result rather than nothing.

**Decision:** When a required external tool is not available, SuitCode returns `Limitation{Kind: "tool_not_available"}` and **empty data**. No fallback to a lower-quality implementation is ever used.

**The concrete example that forced this decision:** C# `FileImporters` had a `.csproj` project-level fallback: when `csharp-ls` was not installed, it returned every file in every referencing project. For a 150-file `MGA.Desktop` project, this meant `App.axaml`, `Program.cs`, `LoadingSpinner.cs`, `Themes/` all appeared as "importers" of `MgaApiService.cs` even though they never referenced it. The agent received 150 files instead of 3. This was far worse than returning nothing — it actively polluted the context.

**The general principle:** A degraded-quality answer is not "better than nothing". In many cases it is *worse than nothing* because:
1. The agent trusts the result and makes decisions based on it.
2. A wrong-but-voluminous result (150 files) crowds out the correct context (3 files).
3. The agent has no way to know the result quality was degraded.

**In code:** Every method that depends on a 3rd party tool must check for availability first:
```go
if p.lspClient == nil {
    return &provider.ProviderResult[[]string]{
        Data: []string{},
        Limitations: []provider.Limitation{{
            Kind:    "tool_not_available",
            Message: "csharp-ls is required but not installed. Run 'suitcode installdeps'.",
        }},
    }, nil
}
```

**This applies to:** LSP servers, compilers (`go build`, `dotnet`), runtime interpreters, CLI tools (any external binary). If it's not part of the Go binary, it's a 3rd party tool and must fail fast when absent.

**Does not apply to:** Static AST parsing for JS/TS and Python — those are the *current primary implementation* of those providers, not a fallback for a missing tool. They are replaced (not kept alongside) as LSP integration is added.

---

## 4. LSP-first for all language providers

**Challenge:** Static AST parsing misses dynamic imports, path aliases, conditional compilation, and cross-project references. Project-level C# references flood importers with hundreds of irrelevant files.

**Decision:** Every language provider must use the language's official LSP server as the authoritative source for import graphs, file-level references, and symbol information.

| Language | LSP server | Install | Prerequisite |
|---|---|---|---|
| Go | `gopls` | `go install golang.org/x/tools/gopls@latest` | Go toolchain |
| C# | `csharp-ls` (wraps Roslyn) | `dotnet tool install --global csharp-ls` | **.NET 10 SDK** |
| TypeScript/JavaScript | `typescript-language-server` | `npm install -g typescript-language-server typescript` | Node.js |
| Python | `pylsp` | `pip install python-lsp-server` | Python |

**C# note:** csharp-ls 0.22+ requires .NET 10 SDK. SuitCode requires the latest csharp-ls — .NET 10 is a hard prerequisite for C# support (`winget install Microsoft.DotNet.SDK.10`).

**Rationale:**
- LSP servers wrap the actual compiler. Their import graphs are the same graph the compiler uses.
- `textDocument/references` gives exact file-level callers — no project-level flooding.
- `textDocument/documentSymbol` gives the exact exported API surface without parsing.
- The cost (startup time) is paid once per investigator lifetime. All subsequent calls are fast.

**LSP daemon contract:**
- LSP servers are started by the investigator on first use, not on every request.
- Their lifetime is tied to the investigator: they start when the investigator starts, they stop when the investigator stops.
- No console window, no user-visible output (stderr is discarded or redirected to the coordinator log).

---

## 5. Shared LSP transport (`core/lsp/`)

**Challenge:** The Content-Length framed JSON-RPC 2.0 transport needed by all LSP clients is non-trivial to implement correctly (concurrent pending map, crash detection, clean shutdown).

**Decision:** One implementation in `core/lsp/transport.go`, imported by every language provider.

**Rationale:** Duplication here is high-risk. The transport handles concurrency, crash propagation, and teardown — duplicating it means duplicating bugs. Every provider gets the same tested implementation.

---

## 6. Tool dependency via `suitcode installdeps`

**Challenge:** SuitCode depends on external LSP servers that may not be installed on a new machine.

**Decision:** `suitcode installdeps` detects which languages are present in the repo and installs the required LSP servers using the language's own package manager.

**Rationale:**
- Avoids "works on my machine." Agents can run `suitcode installdeps` as part of onboarding.
- Each LSP server is installed via its native package manager (go install, dotnet tool, npm, pip) so versioning is handled correctly.
- Missing LSP servers degrade gracefully — the provider reports a Limitation, not a crash.

---

## 7. Tiered budget model (Tier 1 always in, Tier 2 budget-gated)

**Challenge:** Seeds and their direct imports must always be included regardless of budget. Peers and test files are "nice to have."

**Decision:**
- **Tier 1** (score ≥ 0.80: seeds, direct imports, production importers) — always included, budget is advisory.
- **Tier 2** (score < 0.80: peers, test files, test-importers) — included up to remaining budget; trimmed when tight.
- When trimmed, the response reports the exact `--budget` value needed to include everything.

**Score ladder:**
```
1.00  seed file (explicitly requested)
0.90  file is in a package directly imported by a seed
0.80  production file that directly imports a seed
0.75  peer: same compilation unit as a seed
0.70  test file (seed's tests) or test-importer (test that imports seed)
```

**Rationale:**
- Agents should never miss critical-path context (imports, callers) due to budget. They may miss nice-to-have context, and the response tells them exactly how much budget would include it.

---

## 8. Test-importer demotion

**Challenge:** Test files that import a production file were scored as production importers (Tier 1, 0.80), displacing structural peers like `MainWindowViewModel.cs` from the response.

**Decision:** When `f.Role == "test"` and the file appears in the importers set, its score is set to `scoreTest` (0.70, Tier 2) instead of `scoreImporterOf` (0.80, Tier 1).

**Rationale:**
- Production importers are the callers that matter for understanding a change.
- Test files are contextual (they verify behaviour, not define it).
- Within Tier 2, peers (0.75) rank above test-importers (0.70) so structural co-residents are preferred.
- Identified in MGA session feedback (Phase 20): `MgaApiServiceTests.cs` was displacing `MainWindowViewModel.cs`.

---

## 9. C# FileImporters: project-level → file-level via csharp-ls

**Challenge:** The .csproj-based `FileImporters` returns ALL files in any project that has a `<ProjectReference>` to the seed's project. For a 150-file `MGA.Desktop`, this floods the response with `App.axaml`, `Program.cs`, `LoadingSpinner.cs`, `Themes/` etc. — none of which reference the specific service being changed.

**Root cause:** C# project references express compilation dependencies, not type-level usage. Every file in a referencing project is technically a "consumer" at the project level.

**Decision:** Upgrade `FileImporters` in the C# provider to use `csharp-ls textDocument/references`:
1. Open the seed file.
2. Call `textDocument/documentSymbol` to enumerate exported types.
3. For each exported type, call `textDocument/references` at `selectionRange.start`.
4. Aggregate and deduplicate the referencing file paths.

The .csproj graph is retained as a fallback when `csharp-ls` is not installed.

**Rationale:**
- `textDocument/references` returns exactly the files that reference a specific type — not all files in a project. `App.axaml` won't appear unless it actually uses `MgaApiService`.
- Identified in MGA session feedback (Phase 21).

---

## 10. DaemonWaiter interface — generalised daemon readiness

**Challenge:** `WaitForGopls` was hardcoded in `warmup.go` and `LanguageDispatcher`, meaning adding a new LSP server (e.g. csharp-ls) required modifying the warmup code. The naming also implied only gopls was waited on.

**Decision:** Introduce a `provider.DaemonWaiter` interface (`WaitForDaemons(ctx context.Context) bool`). Both `GoLanguageProvider` and `CSHarpLanguageProvider` implement it. `LanguageDispatcher.WaitForAllDaemons` fans out to all registered waiters concurrently — the total wait is bounded by the slowest daemon, not their sum.

**Rationale:**
- `csharp-ls` initialises synchronously in the constructor; its `WaitForDaemons` returns immediately. `gopls` is async (the compiler walk happens in a goroutine); its `WaitForDaemons` blocks until the `goplsDone` channel closes.
- New LSP servers added in the future implement `DaemonWaiter` and are automatically waited on — `warmup.go` doesn't change.
- Concurrent fan-out means startup time stays bounded by the single slowest daemon, not the cumulative sum.

---

## 11. Working directory / project root resolution

**Challenge:** Agents often start in a subdirectory of a project (e.g. `src/auth/`) and run `suitcode .` — which creates an investigator scoped to that subdirectory, not the project root. Later, if the agent moves to the root, a second investigator starts for the full project. Two investigators serve overlapping but inconsistent views of the same repository, and the agent gets different (and contradictory) answers depending on which investigator handles each request.

Three sub-problems:
1. **Subdirectory start:** agent starts at `/a/b`, investigator built for `/a/b` — misses everything in `/a` outside of `/a/b`.
2. **Parent-redirect:** if the agent later asks for `/a`, and an investigator is already running at `/a/b` (child), the coordinator used to spawn a second investigator at `/a` — now two conflict.
3. **Child-upgrade:** if an investigator at `/a` is already running and the agent asks for `/a/b`, the child should transparently use the parent's full context.

**Decision:** Three-layer resolution, each acting as a safety net for the one above:

**Layer 1 — CLI `.suitcode/` walk-up (`suitcode/main.go`):**
Before connecting to the coordinator, the CLI walks up from the requested `repoPath`'s *parent* looking for a `.suitcode/` directory at an ancestor. If found, `repoPath` is silently updated to the ancestor and three advisory lines are printed to stderr. This handles the most common case (second session starting in a subdirectory) without any coordinator involvement.

**Layer 2 — Parent-redirect in the coordinator (`coordinator/registry.go`):**
`GetOrSpawn` checks all running investigators for any whose path is a proper ancestor of the requested path. If found, that investigator is returned directly — no spawn, no wait. This is the safety net when Layer 1 doesn't apply (no `.suitcode/` at a parent, but an investigator is already running at a parent from an earlier call).

**Layer 3 — Child-upgrade in the coordinator (`coordinator/registry.go`):**
When starting a new investigator at path P, any running investigators at children of P are stopped first. A parent always supersedes its children. The parent investigator is then started fresh.

**Key sub-decisions:**

- **No `.suitcode/` migration on child-upgrade.** When a child investigator at `/a/b` is stopped and a parent investigator is started at `/a`, the `.suitcode/` directory at `/a/b` is left in place as historical data. A fresh `.suitcode/` is created at `/a`. Rationale: the child's analysis data is scoped to `/a/b` — merging it into `/a` would require rewriting all stored paths and risks partial-migration bugs. Starting fresh at `/a` is simpler, correct, and the re-warm cost is bounded (30–90 s).

- **Always absolute paths at the CLI boundary.** All file path flags (`--path`, `--files`, `--log`) are resolved to absolute via `filepath.Abs` at the suitcode CLI process before being sent to the coordinator. This means the investigator always receives absolute paths and can resolve them correctly regardless of which directory the agent was in when it called suitcode. Git refs (`--from`) and relation names (`--relations`) are not file paths and are left unchanged.

- **Walk-up starts from parent, not self.** `findSuitCodeRoot` walks from `filepath.Dir(repoPath)` upward, never checking `repoPath` itself. A `.suitcode/` at the exact requested directory is correct and does not trigger a redirect.

**Rationale for the three-layer design:**
- Layer 1 handles the common case before any network call, giving the agent an immediate advisory message.
- Layer 2 handles concurrent agents at different levels within a single session.
- Layer 3 ensures correctness when the path hierarchy evolves within a session (rare but possible).
- Each layer is independent — if Layer 1 fires, Layers 2 and 3 are mostly no-ops.

---

## 12. Feedback system for measuring usefulness

**Challenge:** No objective way to measure whether SuitCode context calls actually help agents write better code.

**Decision:** Two complementary signals:
- **Per-call feedback:** `suitcode . feedback good|bad` stored in `.suitcode/calllog.jsonl`.
- **Session analysis:** parses Claude Code `.jsonl` session files to compute heuristic edit-rate signals (edit-tool-used-after, turns-until-next-edit, retry patterns).

**Rationale:**
- Feedback gives an agent-reported quality signal.
- Session analysis gives an independent structural signal (code edits happened after context) that doesn't require agents to remember to call feedback.
- Real-world measurement from the MGA session (89 calls, 2 days): ~54% of feature calls directly preceded code edits, rising to ~80% after the agent calibrated its usage.

---
