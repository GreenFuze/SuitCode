package features

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	cfeatures "github.com/GreenFuze/SuitCode/core/features"
	"github.com/GreenFuze/SuitCode/core/provider"
)

const defaultFailureBudget = 6_000

// Patterns for extracting signals from failure logs.
var (
	// Go file path with line number: "internal/foo/bar.go:42"
	goFileLineRE = regexp.MustCompile(`([a-zA-Z0-9_/\-\.]+\.go):(\d+)`)
	// Go test name: "--- FAIL: TestFoo"
	goTestFailRE = regexp.MustCompile(`--- FAIL:\s+(\S+)`)
	// Panic: "panic: something [signal]"
	goPanicRE = regexp.MustCompile(`^panic:`)
	// Python traceback file: "File \"foo/bar.py\", line 42"
	pyFileRE = regexp.MustCompile(`File "([^"]+\.py)", line (\d+)`)
)

// RunFailureContext parses a failure log and returns structured signals and
// a bounded context capsule for the suspected files.
// langProv may be nil — when provided it enriches the inner RunContext call
// with import-graph scoring signals.
func RunFailureContext(
	ctx context.Context,
	req cfeatures.FailureContextRequest,
	listing *provider.ProviderResult[provider.FilesystemListing],
	estimator provider.TokenEstimator,
	langProv provider.ImportGraphProvider,
) (*cfeatures.FailureContextResponse, error) {
	if req.LogPath == "" && req.LogText == "" {
		return nil, fmt.Errorf("failure-context: --log or inline log text is required")
	}

	budget := budgetOrDefault(req.Budget, defaultFailureBudget)
	runID := newRunID("failure-context")
	metrics, start := startMetrics(runID, "failure-context", req.RepoPath, budget)

	// Read log text.
	logText := req.LogText
	if req.LogPath != "" && logText == "" {
		data, err := os.ReadFile(req.LogPath)
		if err != nil {
			return nil, fmt.Errorf("failure-context: reading log file %q: %w", req.LogPath, err)
		}
		logText = string(data)
	}

	resp := &cfeatures.FailureContextResponse{
		BaseFeatureResponse: cfeatures.BaseFeatureResponse{RunID: runID},
	}

	// ── Signal extraction ──────────────────────────────────────────────────────

	filesSeen := make(map[string]bool)
	testsSeen := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(logText))
	for scanner.Scan() {
		line := scanner.Text()

		// Go file + line references.
		for _, m := range goFileLineRE.FindAllStringSubmatch(line, -1) {
			path := m[1]
			if !filesSeen[path] {
				filesSeen[path] = true
				resp.ParsedSignals = append(resp.ParsedSignals, cfeatures.FailureSignal{
					Kind:       "file_path",
					Value:      path,
					Confidence: 0.90,
					Provenance: heuristicProv("log_parser", fmt.Sprintf("file reference in log: %s", path)),
				})
			}
		}

		// Go test failures.
		if m := goTestFailRE.FindStringSubmatch(line); len(m) == 2 {
			name := m[1]
			if !testsSeen[name] {
				testsSeen[name] = true
				resp.ParsedSignals = append(resp.ParsedSignals, cfeatures.FailureSignal{
					Kind:       "test_name",
					Value:      name,
					Confidence: 0.95,
					Provenance: heuristicProv("log_parser", fmt.Sprintf("failing test: %s", name)),
				})
			}
		}

		// Panic lines.
		if goPanicRE.MatchString(strings.TrimSpace(line)) {
			resp.ParsedSignals = append(resp.ParsedSignals, cfeatures.FailureSignal{
				Kind:       "error_message",
				Value:      strings.TrimSpace(line),
				Confidence: 0.80,
				Provenance: heuristicProv("log_parser", "panic detected"),
			})
		}

		// Python file references.
		for _, m := range pyFileRE.FindAllStringSubmatch(line, -1) {
			path := m[1]
			if !filesSeen[path] {
				filesSeen[path] = true
				resp.ParsedSignals = append(resp.ParsedSignals, cfeatures.FailureSignal{
					Kind:       "file_path",
					Value:      path,
					Confidence: 0.85,
					Provenance: heuristicProv("log_parser", fmt.Sprintf("python file reference: %s", path)),
				})
			}
		}
	}

	// ── Resolve signals to repository files ───────────────────────────────────

	var seedFiles []string
	for _, sig := range resp.ParsedSignals {
		if sig.Kind != "file_path" {
			continue
		}
		// Try to match against the file index by full path.
		clean := filepath.ToSlash(filepath.Clean(sig.Value))
		fsFile, err := findFile(listing, clean, req.RepoPath)
		if err != nil {
			// Full-path match failed. Attempt basename-only match so that
			// stack-trace paths like "server/foo/bar.go" can still resolve even
			// when the working directory prefix differs. Explicitly record a
			// Limitation so callers know the match is weaker and may be ambiguous
			// when multiple files share the same basename.
			base := filepath.Base(clean)
			var matches []provider.FilesystemFile
			for _, f := range listing.Data.Files {
				if filepath.Base(f.RelPath) == base {
					matches = append(matches, f)
				}
			}
			if len(matches) == 1 {
				fsFile = &matches[0]
				resp.Limitations = append(resp.Limitations, provider.Limitation{
					Kind:    "basename_match_only",
					Message: fmt.Sprintf("log path %q matched by basename only; full-path lookup failed", sig.Value),
					Scope:   sig.Value,
				})
			} else if len(matches) > 1 {
				// Ambiguous — multiple files with the same basename. Record the
				// ambiguity and skip rather than guessing.
				resp.Limitations = append(resp.Limitations, provider.Limitation{
					Kind:    "ambiguous_basename_match",
					Message: fmt.Sprintf("log path %q matched %d files by basename — skipping to avoid false positives", sig.Value, len(matches)),
					Scope:   sig.Value,
				})
			}
		}
		if fsFile != nil {
			prov := heuristicProv("log_parser",
				fmt.Sprintf("file referenced in failure log"), fsFile.Path)
			resp.SuspectedFiles = append(resp.SuspectedFiles, fileToRef(*fsFile, prov))
			seedFiles = append(seedFiles, fsFile.RelPath)
		}
	}

	// Resolve test names to test files.
	for _, sig := range resp.ParsedSignals {
		if sig.Kind != "test_name" {
			continue
		}
		for _, f := range listing.Data.Files {
			if f.Role != "test" {
				continue
			}
			prov := heuristicProv("log_parser",
				fmt.Sprintf("test %q found in file %s", sig.Value, f.RelPath), f.Path)
			resp.SuspectedTests = append(resp.SuspectedTests, cfeatures.TestReference{
				Name:       sig.Value,
				FilePath:   f.Path,
				RelPath:    f.RelPath,
				RunCommand: buildTestCommand(f, listing),
				Provenance: prov,
			})
			break // one match per test name is enough
		}
	}

	// Suggest commands based on detected signals.
	if hasSuffix(seedFiles, "_test.go") || len(resp.SuspectedTests) > 0 {
		resp.SuspectedCommands = append(resp.SuspectedCommands, "go test ./... -v")
	}
	if hasSuffix(seedFiles, ".go") {
		resp.SuspectedCommands = append(resp.SuspectedCommands, "go build ./...")
	}

	// Compile a mini context capsule from the suspected files.
	if len(seedFiles) > 0 {
		ctxResp, err := RunContext(
			ctx,
			cfeatures.ContextRequest{
				BaseFeatureRequest: cfeatures.BaseFeatureRequest{
					RepoPath: req.RepoPath,
					Budget:   budget,
					Format:   req.Format,
				},
				Files: seedFiles,
			},
			listing,
			estimator,
			langProv,
		)
		if err == nil {
			resp.RelatedContext = ctxResp.Capsule
		}
	}

	metrics.Budget.Used = resp.RelatedContext.BudgetUsed
	finishMetrics(&metrics, start, resp)
	resp.Metrics = metrics

	return resp, nil
}

// hasSuffix returns true if any element of paths ends with suffix.
func hasSuffix(paths []string, suffix string) bool {
	for _, p := range paths {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}
