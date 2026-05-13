package goprovider

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GreenFuze/SuitCode/core/provider"
)

// managedGoplsVersion is the version passed to `go install` when auto-installing.
// Using "latest" means: install the latest release and cache it. The cached binary
// persists until the .ready marker is deleted (e.g., to force an upgrade).
const managedGoplsVersion = "latest"

// resolveBinary attempts to locate the gopls binary via 4-tier resolution:
//
//  1. $SUITCODE_GOPLS_PATH environment variable (explicit override)
//  2. Managed cache binary + .ready marker (previously auto-installed)
//  3. System PATH lookup (exec.LookPath)
//  4. Auto-install via: GOBIN=<managed-dir> go install golang.org/x/tools/gopls@latest
//
// Returns (binaryPath, nil) on success or ("", *Limitation) on failure.
// Never returns a hard error — callers treat unavailable gopls as a Limitation.
func resolveBinary() (string, *provider.Limitation) {
	// Tier 1: explicit env var override.
	if path := os.Getenv("SUITCODE_GOPLS_PATH"); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// Tier 2: managed cache (previously installed by us).
	binDir := managedGoplsBinDir()
	binName := goplsBinaryName()
	managedBin := filepath.Join(binDir, binName)
	readyMarker := filepath.Join(binDir, ".ready")

	if _, err := os.Stat(managedBin); err == nil {
		if _, err := os.Stat(readyMarker); err == nil {
			return managedBin, nil
		}
	}

	// Tier 3: system PATH.
	if path, err := exec.LookPath("gopls"); err == nil {
		return path, nil
	}

	// Tier 4: auto-install.
	installed, err := installGopls(binDir)
	if err != nil {
		return "", &provider.Limitation{
			Kind:    "gopls_not_found",
			Message: fmt.Sprintf("gopls binary not found and auto-install failed: %v", err),
		}
	}
	return installed, nil
}

// managedGoplsBinDir returns the OS-appropriate directory for the managed gopls binary.
//
// Priority:
//  1. $SUITCODE_TOOL_CACHE_DIR/gopls/managed/bin   (user override)
//  2. %LocalAppData%\SuitCode\tools\gopls\managed\bin  (Windows)
//  3. $XDG_CACHE_HOME/SuitCode/tools/gopls/managed/bin  (Linux/macOS with XDG)
//  4. ~/.cache/SuitCode/tools/gopls/managed/bin    (fallback)
func managedGoplsBinDir() string {
	// User-level override.
	if override := os.Getenv("SUITCODE_TOOL_CACHE_DIR"); override != "" {
		return filepath.Join(override, "gopls", "managed", "bin")
	}

	var cacheRoot string

	switch runtime.GOOS {
	case "windows":
		// Prefer %LocalAppData% (C:\Users\<user>\AppData\Local).
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			cacheRoot = filepath.Join(local, "SuitCode")
		}
	default:
		// XDG Base Directory specification.
		if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
			cacheRoot = filepath.Join(xdg, "SuitCode")
		}
	}

	if cacheRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		cacheRoot = filepath.Join(home, ".cache", "SuitCode")
	}

	return filepath.Join(cacheRoot, "tools", "gopls", "managed", "bin")
}

// installGopls runs `go install golang.org/x/tools/gopls@latest` with GOBIN
// pointing at binDir. Creates binDir if it does not exist. Writes a .ready
// marker file on success. Returns the absolute path to the installed binary.
func installGopls(binDir string) (string, error) {
	goExe, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf(
			"go binary not found on PATH — cannot auto-install gopls (install manually and set $SUITCODE_GOPLS_PATH): %w",
			err,
		)
	}

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("creating gopls bin dir %q: %w", binDir, err)
	}

	// Build the environment with GOBIN pointing at our managed directory.
	// Replace any existing GOBIN entry to avoid duplication.
	rawEnv := os.Environ()
	newEnv := make([]string, 0, len(rawEnv)+1)
	goBinSet := false
	for _, e := range rawEnv {
		if strings.HasPrefix(e, "GOBIN=") {
			if !goBinSet {
				newEnv = append(newEnv, "GOBIN="+binDir)
				goBinSet = true
			}
			// Skip duplicate GOBIN entries.
		} else {
			newEnv = append(newEnv, e)
		}
	}
	if !goBinSet {
		newEnv = append(newEnv, "GOBIN="+binDir)
	}

	// Run: go install golang.org/x/tools/gopls@<version>
	cmd := exec.Command(goExe, "install", "golang.org/x/tools/gopls@"+managedGoplsVersion)
	cmd.Env = newEnv
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return "", fmt.Errorf("go install gopls@%s: %w\noutput:\n%s",
			managedGoplsVersion, runErr, strings.TrimSpace(string(out)))
	}

	// Verify the binary was placed correctly.
	binPath := filepath.Join(binDir, goplsBinaryName())
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("gopls binary not found at %q after install", binPath)
	}

	// Write the .ready marker — its presence tells resolveBinary that the
	// managed binary is complete and trustworthy.
	readyMarker := filepath.Join(binDir, ".ready")
	if werr := os.WriteFile(readyMarker, []byte("ok\n"), 0o644); werr != nil {
		// Non-fatal: the binary is usable; we'll just re-install next time.
		_ = werr
	}

	return binPath, nil
}

// goplsBinaryName returns the platform-appropriate name for the gopls executable.
func goplsBinaryName() string {
	if runtime.GOOS == "windows" {
		return "gopls.exe"
	}
	return "gopls"
}
