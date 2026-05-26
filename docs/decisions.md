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

## 4. LSP-first for all language providers

**Challenge:** Static AST parsing misses dynamic imports, path aliases, conditional compilation, and cross-project references. Project-level C# references flood importers with hundreds of irrelevant files.

**Decision:** Every language provider must use the language's official LSP server as the authoritative source for import graphs, file-level references, and symbol information.

| Language | LSP server | Install |
|---|---|---|
| Go | `gopls` | `go install golang.org/x/tools/gopls@latest` |
| C# | `csharp-ls` (wraps Roslyn) | `dotnet tool install --global csharp-ls` |
| TypeScript/JavaScript | `typescript-language-server` | `npm install -g typescript-language-server typescript` |
| Python | `pylsp` | `pip install python-lsp-server` |

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

## 10. Feedback system for measuring usefulness

**Challenge:** No objective way to measure whether SuitCode context calls actually help agents write better code.

**Decision:** Two complementary signals:
- **Per-call feedback:** `suitcode . feedback good|bad` stored in `.suitcode/calllog.jsonl`.
- **Session analysis:** parses Claude Code `.jsonl` session files to compute heuristic edit-rate signals (edit-tool-used-after, turns-until-next-edit, retry patterns).

**Rationale:**
- Feedback gives an agent-reported quality signal.
- Session analysis gives an independent structural signal (code edits happened after context) that doesn't require agents to remember to call feedback.
- Real-world measurement from the MGA session (89 calls, 2 days): ~54% of feature calls directly preceded code edits, rising to ~80% after the agent calibrated its usage.

---
