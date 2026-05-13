package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// Shared investigator fixture
//
// Creating a ProjectInvestigator is expensive (~1 s due to go/packages load).
// All investigator tests share a single warmed instance created via sync.Once.
// ──────────────────────────────────────────────────────────────────────────────

var (
	sharedInvestigatorOnce sync.Once
	sharedInvInstance      *ProjectInvestigator
	sharedInvErr           error
)

// sharedInv returns a warmed *ProjectInvestigator for the repository root.
// It is created once and reused across all tests in this package.
// The test is marked fatal if setup fails.
// findRepoRoot walks up from the current directory until it finds a go.mod file,
// returning that directory. Falls back to "." if go.mod is not found.
func findRepoRoot() string {
	dir, err := filepath.Abs(".")
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root reached
		}
		dir = parent
	}
	return "."
}

func sharedInv(t *testing.T) *ProjectInvestigator {
	t.Helper()

	sharedInvestigatorOnce.Do(func() {
		ctx := context.Background()
		sharedInvInstance, sharedInvErr = NewProjectInvestigator(ctx, findRepoRoot())
		if sharedInvErr != nil {
			return
		}
		sharedInvErr = sharedInvInstance.Warm(ctx)
	})

	if sharedInvErr != nil {
		t.Fatalf("shared investigator setup failed: %v", sharedInvErr)
	}
	return sharedInvInstance
}

// skipIfShort skips the test when running with -short, printing a reason.
func skipIfShort(t *testing.T, reason string) {
	t.Helper()
	if testing.Short() {
		t.Skipf("skipping in short mode: %s", reason)
	}
}

// fileExistsInList reports whether any path in paths matches relPath, accepting
// either the exact value or its slash-normalised form.
func fileExistsInList(paths []string, relPath string) bool {
	for _, p := range paths {
		if p == relPath || osPathToSlash(p) == relPath {
			return true
		}
	}
	return false
}

func osPathToSlash(p string) string {
	out := make([]byte, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' {
			out[i] = '/'
		} else {
			out[i] = p[i]
		}
	}
	return string(out)
}

// sliceContains reports whether s contains the target string.
func sliceContains(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

// TestMain sets up any global state needed for investigator tests and
// suppresses the gopls startup banner to keep test output clean.
func TestMain(m *testing.M) {
	// Suppress all stdout from the investigator during tests — the status
	// messages ("SuitCode: warming…") are useful interactively but noisy in
	// `go test -v` output. Tests use t.Logf for their own reporting.
	// NOTE: we cannot capture os.Stdout without forking, so we skip that and
	// just run normally. The test binary's output is filtered by `go test`.
	os.Exit(m.Run())
}
