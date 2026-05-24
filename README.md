# SuitCode

**Your coding agent actually knows your codebase. Not guesses — knows.**

---

## The problem with AI coding agents

When Claude Code, Codex, or Cursor reads your repo, they do text search.
They find files by name patterns and keyword matches.
They guess what's related based on proximity and conventions.

That's fine for a demo. In a real codebase with 500+ files, it means:
- Loading 200 files when only 12 are relevant
- Missing the file that actually imports the one you're changing
- Hitting token limits before reaching the code that matters
- Hallucinations caused by irrelevant context crowding out the real signal

## What SuitCode does differently

SuitCode speaks your toolchain. It doesn't guess — it asks the tools your language already has:

| Signal | Source | Certainty |
|--------|--------|-----------|
| Which files import this one | `go/packages`, TypeScript compiler, Python AST | **Authoritative** |
| What tests cover this code | Import graph + naming conventions | **Authoritative** |
| What breaks if this changes | Reverse import graph (full transitive) | **Authoritative** |
| Symbol definitions and types | `gopls` LSP | **Authoritative** |

SuitCode never returns a result it can't back with a verified signal. If the import graph isn't available, it says so — it does **not** fall back silently to a regex scan and pretend that's the same thing. Every result carries its provenance.

## Quick install

**Requires:** Go 1.21+

```sh
go install github.com/GreenFuze/SuitCode/suitcode@latest
go install github.com/GreenFuze/SuitCode/coordinator@latest
go install github.com/GreenFuze/SuitCode/investigator@latest
```

That's it. All three binaries land in `$GOPATH/bin` (usually `~/go/bin`). The coordinator auto-starts on first use — no config, no daemon management.

**Desktop tray** (optional — shows live investigator status in your menu bar):

```sh
# macOS / Windows — no extra dependencies
go install -tags systray github.com/GreenFuze/SuitCode/tray@latest

# Linux — AppIndicator first
sudo apt install libayatana-appindicator3-dev   # Debian/Ubuntu
go install -tags systray github.com/GreenFuze/SuitCode/tray@latest
```

**CI / headless servers** — CGo-free, no tray:

```sh
CGO_ENABLED=0 go install github.com/GreenFuze/SuitCode/suitcode@latest
CGO_ENABLED=0 go install github.com/GreenFuze/SuitCode/coordinator@latest
CGO_ENABLED=0 go install github.com/GreenFuze/SuitCode/investigator@latest
```

---

## Why SuitCode?

### For agents that need to stay within token budgets

Every SuitCode response comes with a **token budget** you control. Ask for 8,000 tokens of context — SuitCode returns the highest-signal files that fit, ordered by import-graph proximity to your seed files. The rest of the repo doesn't enter the LLM's context window at all.

```sh
suitcode . context \
  --files internal/auth/auth.go,internal/auth/tokens.go \
  --budget 8000
```

Typical compression ratio: **10–40×**. 500-file repo → 15 files in context.

### For agents working across languages

SuitCode is polyglot from day one. In a repo with a Go backend, TypeScript frontend, and Python scripts, SuitCode runs all three import-graph providers simultaneously and merges the results. No "pick one language" compromise.

### For multi-module monorepos

Go workspaces with 16 plugin modules? SuitCode walks every `go.mod` and builds a unified cross-module import graph. A plugin importing from the core module shows as a direct edge — not missing data.

### For teams, not just solo devs

SuitCode runs as a local daemon. Every agent that needs context hits the same coordinator — investigators are shared per project, warmed once, served to everyone. No re-indexing per-session.

---

## What the agents see

### Context capsule

```sh
suitcode . context --files src/game/game.ts --budget 6000
```

Returns a ranked list of files that are likely relevant to editing `game.ts`:
- Files it directly imports (import graph — authoritative)
- Files that import it (reverse edges — authoritative)
- Co-located test files (naming convention — labeled as such)
- Related files in the same module

Each file entry includes: path, language, role, token estimate, and **provenance** — exactly how SuitCode decided it was relevant.

### Blast radius

```sh
suitcode . impact --files internal/store/store.go --git-ref HEAD~1
```

Shows every file that transitively depends on what changed. Run this before a refactor to know exactly what your PR will break.

### Test finder

```sh
suitcode . tests --path internal/auth/auth.go
```

Finds test files that cover `auth.go` — by import graph (tests that import the package) and by naming convention. Scored separately so you know which ones are certain.

### Failure context

```sh
suitcode . failure-context --log build-output.txt
```

Parses build or test failure output, resolves file paths against the real index, and compiles a context capsule focused on the failing code. Stack traces become navigation.

---

## For Codex / Claude Code integration

SuitCode exposes all features as an HTTP API. The coordinator runs on `127.0.0.1:7878` by default.

```sh
# Start the coordinator (or let `suitcode` auto-start it)
coordinator --port 7878

# Warm a project (indexes files + import graph)
curl -s -X POST http://127.0.0.1:7878/api/v1/warmup \
  -H 'Content-Type: application/json' \
  -d '{"repo_path": "/path/to/your/repo"}'

# Request a context capsule
curl -s -X POST http://127.0.0.1:7878/api/v1/context \
  -H 'Content-Type: application/json' \
  -d '{
    "repo_path": "/path/to/your/repo",
    "files": ["src/app.ts"],
    "budget": 8000
  }'
```

All responses include a `metrics` block: tokens used, files considered, files included, compression ratio, and whether the import graph was available for this result.

---

## Supported languages

| Language | Import graph | Symbols | Test detection |
|----------|-------------|---------|----------------|
| Go | `go/packages` (full, multi-module) | `gopls` | ✓ |
| TypeScript / JavaScript | Static AST + tsconfig path aliases | — | ✓ |
| Python | Static AST + relative import resolution | — | ✓ |

More languages are on the roadmap. SuitCode's provider model makes it straightforward to add new language backends without changing the core.

---

## Architecture

```
suitcode/           — CLI client (auto-starts coordinator on first use)
coordinator/        — HTTP daemon routing requests to per-project investigators
investigator/       — per-project daemon: file index, import graph, gopls, features
  features/         — context capsule, explain-file, related, tests, impact, …
tray/               — desktop status companion (build with -tags systray)
core/
  provider/
    language/go/    — Go import graph via go/packages + gopls LSP
    language/js/    — JS/TS import graph + tsconfig alias resolution
    language/python/— Python import graph (stdlib + relative imports)
    language/multi/ — composite provider for polyglot repos
    filesystem/     — file listing with .gitignore + build-artifact exclusion
    vcs/            — git status and diff
```

---

## Development

```sh
go test ./... -short           # fast suite (skips go/packages and gopls)
go test ./...                  # full suite including eval correctness checks
go build ./suitcode            # CLI
go build ./coordinator         # coordinator daemon
go build ./investigator        # investigator daemon
go build -tags systray ./tray  # desktop tray (requires CGo)
```

State is written to `.suitcode/` in each analyzed repo. Add it to `.gitignore`:

```
.suitcode/
```

---

*SuitCode is open source. PRs welcome.*
