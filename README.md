# SuitCode

**Deterministic repository intelligence for coding agents.**

SuitCode is a local CLI and HTTP server that answers repository questions by
walking the actual toolchain — import graphs, build systems, test frameworks,
language servers — instead of doing text search.

It is designed to be called by coding agents (Claude Code, Cursor, Codex) to
retrieve a tight "context capsule": the most relevant files for a given task,
ranked by import-graph proximity, within a token budget. The core value metric
is compression ratio: how much of the repo did the agent *not* have to load?

## Install

```sh
go install github.com/GreenFuze/SuitCode/cmd/suitcode@latest
```

This installs the `suitcode` binary to `$GOPATH/bin` (or `$HOME/go/bin`).

**Requirements:** Go 1.21+, `gopls` on PATH for symbol-level queries.

## Usage

```
suitcode <repo-path> <command> [flags]
```

The repo-path argument is always required and must come before the command.

### Core commands

| Command | Description |
|---------|-------------|
| `status` | Show readiness and provider status |
| `context` | Compile a bounded context capsule (primary use case) |
| `repo-overview` | Repository structure and technology overview |
| `explain-file` | Explain a file's role, imports, and relationships |
| `related` | Find files related to a given file |
| `tests` | Find tests relevant to a source file or change |
| `impact` | Blast radius analysis for a set of changed files |
| `failure-context` | Extract context from a failure log |
| `verify-plan` | Generate a verification plan for a set of changes |

### Server mode

```sh
suitcode . serve --port 7878
```

All features are available via HTTP at `http://localhost:7878/api/v1/<feature>`.

### Metrics

Every feature call appends a structured record to `.suitcode/calls.jsonl` in the
analyzed repository. No code content — only relative paths and numeric metrics.

```sh
suitcode . metrics show          # tabular summary of recent calls
suitcode . metrics export        # zip the log for sharing
```

### Analytics

Correlate SuitCode capsule files with Claude Code file operations to measure
capsule quality (did we include the right files?).

```sh
suitcode . analytics show        # list Claude Code sessions for this project
suitcode . analytics correlate   # compute per-call hit/miss rates
```

### Output

Feature results are written to `.suitcode/<feature>/<timestamp>.json` in the
analyzed repository. The CLI prints a brief summary to stdout; use
`--format json` or `--format markdown` for full output.

### Examples

```sh
# Compile a context capsule for files you're about to edit
suitcode . context --files internal/server/main.go,internal/auth/auth.go --budget 8000

# Understand what breaks if you change a file
suitcode . impact --files internal/auth/auth.go

# Find tests for a changed file
suitcode . tests --path internal/auth/auth.go

# Explain an unfamiliar file
suitcode . explain-file --path internal/server/main.go --format markdown

# Run the smoke eval suite to verify correctness
suitcode . eval run --suite smoke
```

## Architecture

```
cmd/suitcode/       — CLI binary entry point
core/
  config/           — global and per-project configuration
  features/         — typed request/response contracts (shared vocabulary)
  provider/         — provider interfaces and vocabulary types
    filesystem/     — file listing with .gitignore support
    vcs/            — git status and diff
    language/go/    — Go import graph (go/packages) + gopls symbol queries
investigator/
  artifacts/        — SQLite artifact store (.suitcode/store.db)
  eval/             — evaluation framework and suites
  features/         — feature implementations (context capsule, etc.)
  output/           — markdown renderers
calllog/            — JSONL call logger (.suitcode/calls.jsonl)
analytics/          — transcript analysis and capsule quality metrics
```

## State directory

SuitCode creates a `.suitcode/` directory in the root of each analyzed
repository. This directory should be added to `.gitignore`:

```
.suitcode/
```

Contents:
- `store.db` — SQLite artifact store (run metrics, eval results)
- `calls.jsonl` — per-call metrics log
- `<feature>/<timestamp>.json` — full feature result artifacts
- `config.json` — per-project configuration (optional)

## Development

```sh
go test ./... -short    # fast tests (skips go/packages load and gopls)
go test ./...           # full test suite including eval suites
go build ./cmd/suitcode # build the binary
```
