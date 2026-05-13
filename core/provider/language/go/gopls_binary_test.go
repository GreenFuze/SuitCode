package goprovider_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	goprovider "github.com/GreenFuze/SuitCode/core/provider/language/go"
)

// ──────────────────────────────────────────────────────────────────────────────
// pathToURI
// ──────────────────────────────────────────────────────────────────────────────

func TestPathToURI_POSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path test skipped on Windows")
	}

	got := goprovider.PathToURIForTest("/foo/bar.go")
	want := "file:///foo/bar.go"
	if got != want {
		t.Errorf("PathToURI(%q) = %q; want %q", "/foo/bar.go", got, want)
	}
}

func TestPathToURI_POSIXWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path test skipped on Windows")
	}

	got := goprovider.PathToURIForTest("/path/with spaces/bar.go")
	// Spaces should be percent-encoded.
	if !strings.HasPrefix(got, "file:///path/with%20spaces/bar.go") {
		t.Errorf("PathToURI with spaces = %q; want percent-encoded URI", got)
	}
}

func TestPathToURI_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows URI test skipped on non-Windows")
	}

	// Use the current file's path as input — guaranteed to be a real Windows path.
	absPath := `C:\foo\bar.go`
	got := goprovider.PathToURIForTest(absPath)
	want := "file:///C:/foo/bar.go"
	if got != want {
		t.Errorf("PathToURI(%q) = %q; want %q", absPath, got, want)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// managedGoplsBinDir
// ──────────────────────────────────────────────────────────────────────────────

func TestManagedGoplsBinDir_EnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SUITCODE_TOOL_CACHE_DIR", tmp)

	got := goprovider.ManagedGoplsBinDirForTest()
	want := filepath.Join(tmp, "gopls", "managed", "bin")
	if got != want {
		t.Errorf("managedGoplsBinDir = %q; want %q", got, want)
	}
}

func TestManagedGoplsBinDir_DefaultContainsSuitCode(t *testing.T) {
	// Clear overrides so we hit the default path logic.
	t.Setenv("SUITCODE_TOOL_CACHE_DIR", "")
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("XDG_CACHE_HOME", "")

	got := goprovider.ManagedGoplsBinDirForTest()

	// The path must contain "SuitCode" and end with "managed/bin" (or "managed\bin").
	if !strings.Contains(got, "SuitCode") {
		t.Errorf("managedGoplsBinDir = %q; want path containing 'SuitCode'", got)
	}
	if !strings.Contains(got, "managed") {
		t.Errorf("managedGoplsBinDir = %q; want path containing 'managed'", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// resolveBinary — tier 1: explicit env var
// ──────────────────────────────────────────────────────────────────────────────

func TestResolveBinary_EnvVar(t *testing.T) {
	// Create a fake executable in a temp dir.
	tmp := t.TempDir()
	fakeExe := filepath.Join(tmp, "fakegopls")
	if runtime.GOOS == "windows" {
		fakeExe += ".exe"
	}
	if err := os.WriteFile(fakeExe, []byte("#!/bin/sh\necho gopls"), 0o755); err != nil {
		t.Fatalf("creating fake binary: %v", err)
	}

	t.Setenv("SUITCODE_GOPLS_PATH", fakeExe)
	// Point managed cache and PATH away so tier 2/3 cannot interfere.
	t.Setenv("SUITCODE_TOOL_CACHE_DIR", t.TempDir())

	path, lim := goprovider.ResolveBinaryForTest()
	if lim != nil {
		t.Fatalf("expected no limitation, got: %+v", lim)
	}
	if path != fakeExe {
		t.Errorf("got path %q; want %q", path, fakeExe)
	}
}

func TestResolveBinary_EnvVar_MissingFile(t *testing.T) {
	// Set env var to a path that doesn't exist — should fall through to the next tier.
	t.Setenv("SUITCODE_GOPLS_PATH", "/nonexistent/gopls")

	// Also isolate tiers 2, 3 so we see the fall-through.
	t.Setenv("SUITCODE_TOOL_CACHE_DIR", t.TempDir())

	// We don't assert the exact outcome here — just that the function doesn't panic.
	_, _ = goprovider.ResolveBinaryForTest()
}

// ──────────────────────────────────────────────────────────────────────────────
// resolveBinary — tier 2: managed cache
// ──────────────────────────────────────────────────────────────────────────────

func TestResolveBinary_ManagedCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SUITCODE_TOOL_CACHE_DIR", tmp)
	// Disable tier 1.
	t.Setenv("SUITCODE_GOPLS_PATH", "")

	// Create the managed binary + .ready marker.
	binDir := filepath.Join(tmp, "gopls", "managed", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	binName := "gopls"
	if runtime.GOOS == "windows" {
		binName = "gopls.exe"
	}
	fakeExe := filepath.Join(binDir, binName)
	if err := os.WriteFile(fakeExe, []byte("#!/bin/sh\necho gopls"), 0o755); err != nil {
		t.Fatalf("creating fake binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, ".ready"), []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("creating .ready: %v", err)
	}

	path, lim := goprovider.ResolveBinaryForTest()
	if lim != nil {
		t.Fatalf("expected no limitation, got: %+v", lim)
	}
	if path != fakeExe {
		t.Errorf("got path %q; want %q", path, fakeExe)
	}
}

func TestResolveBinary_ManagedCache_MissingReady(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SUITCODE_TOOL_CACHE_DIR", tmp)
	t.Setenv("SUITCODE_GOPLS_PATH", "")

	// Binary exists but .ready is missing — tier 2 must not return this path.
	binDir := filepath.Join(tmp, "gopls", "managed", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binName := "gopls"
	if runtime.GOOS == "windows" {
		binName = "gopls.exe"
	}
	fakeExe := filepath.Join(binDir, binName)
	if err := os.WriteFile(fakeExe, []byte("#!/bin/sh\necho gopls"), 0o755); err != nil {
		t.Fatalf("creating fake binary: %v", err)
	}
	// Intentionally do NOT write .ready.

	// This will fall through to tier 3 / 4. Just ensure it doesn't return
	// the managed binary (since .ready is absent).
	path, _ := goprovider.ResolveBinaryForTest()
	if path == fakeExe {
		t.Errorf("managed binary without .ready should not be returned, but got %q", path)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// FlattenSymbolNames
// ──────────────────────────────────────────────────────────────────────────────

func TestFlattenSymbolNames_Flat(t *testing.T) {
	syms := []goprovider.LspDocumentSymbolForTest{
		{Name: "Foo"},
		{Name: "Bar"},
	}
	got := goprovider.FlattenSymbolNamesForTest(syms)
	want := []string{"Foo", "Bar"}
	if !equalStringSlice(got, want) {
		t.Errorf("FlattenSymbolNames = %v; want %v", got, want)
	}
}

func TestFlattenSymbolNames_Hierarchical(t *testing.T) {
	syms := []goprovider.LspDocumentSymbolForTest{
		{
			Name: "MyStruct",
			Children: []goprovider.LspDocumentSymbolForTest{
				{Name: "Method1"},
				{Name: "Method2"},
			},
		},
		{Name: "TopLevel"},
	}
	got := goprovider.FlattenSymbolNamesForTest(syms)
	// Expected: MyStruct, MyStruct.Method1, MyStruct.Method2, TopLevel
	want := []string{"MyStruct", "MyStruct.Method1", "MyStruct.Method2", "TopLevel"}
	if !equalStringSlice(got, want) {
		t.Errorf("FlattenSymbolNames = %v; want %v", got, want)
	}
}

func TestFlattenSymbolNames_DeepNesting(t *testing.T) {
	syms := []goprovider.LspDocumentSymbolForTest{
		{
			Name: "A",
			Children: []goprovider.LspDocumentSymbolForTest{
				{
					Name: "B",
					Children: []goprovider.LspDocumentSymbolForTest{
						{Name: "C"},
					},
				},
			},
		},
	}
	got := goprovider.FlattenSymbolNamesForTest(syms)
	want := []string{"A", "A.B", "A.B.C"}
	if !equalStringSlice(got, want) {
		t.Errorf("FlattenSymbolNames deep = %v; want %v", got, want)
	}
}

func TestFlattenSymbolNames_Empty(t *testing.T) {
	got := goprovider.FlattenSymbolNamesForTest(nil)
	if len(got) != 0 {
		t.Errorf("FlattenSymbolNames(nil) = %v; want empty", got)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// symbolPresent reports whether any symbol in names matches want, either as
// an exact match or as a ".want" suffix (handles gopls "(*Receiver).Method"
// and "Parent.Child" flattened formats).
func symbolPresent(names []string, want string) bool {
	suffix := "." + want
	for _, n := range names {
		if n == want {
			return true
		}
		if len(n) > len(suffix) && n[len(n)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
