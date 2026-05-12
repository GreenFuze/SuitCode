// Package config holds global and per-project configuration types and their
// loaders. Config files are JSON and located at:
//
//	~/.suitcode/config.json          — GlobalConfig
//	<repo>/.suitcode/config.json     — ProjectConfig
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultCoordinatorPort is the TCP port the coordinator listens on.
	DefaultCoordinatorPort = 7878
	// DefaultMaxBudget is the default token budget when none is specified.
	DefaultMaxBudget = 10_000
	// DefaultCacheTTL is how long cached investigator state is considered fresh.
	DefaultCacheTTL = 24 * time.Hour
	// SuitCodeDir is the repository-local state directory name.
	SuitCodeDir = ".suitcode"
)

// GlobalConfig holds settings that apply across all projects and are stored
// in the user's home directory.
type GlobalConfig struct {
	CoordinatorPort int    `json:"coordinator_port"`
	CoordinatorHost string `json:"coordinator_host"`
	LogLevel        string `json:"log_level"`
}

// Defaults fills any zero-value fields with sensible defaults.
func (g *GlobalConfig) Defaults() {
	if g.CoordinatorPort == 0 {
		g.CoordinatorPort = DefaultCoordinatorPort
	}
	if g.CoordinatorHost == "" {
		g.CoordinatorHost = "127.0.0.1"
	}
	if g.LogLevel == "" {
		g.LogLevel = "info"
	}
}

// ProjectConfig holds settings specific to one repository.
type ProjectConfig struct {
	MaxBudget int           `json:"max_budget"`
	CacheTTL  time.Duration `json:"cache_ttl_ns"`
}

// Defaults fills any zero-value fields with sensible defaults.
func (p *ProjectConfig) Defaults() {
	if p.MaxBudget == 0 {
		p.MaxBudget = DefaultMaxBudget
	}
	if p.CacheTTL == 0 {
		p.CacheTTL = DefaultCacheTTL
	}
}

// LoadGlobal reads the global config from ~/.suitcode/config.json.
// Missing or unreadable files are silently replaced with defaults.
func LoadGlobal() GlobalConfig {
	var cfg GlobalConfig

	home, err := os.UserHomeDir()
	if err != nil {
		cfg.Defaults()
		return cfg
	}

	path := filepath.Join(home, SuitCodeDir, "config.json")
	if err := loadJSON(path, &cfg); err != nil {
		// File missing or malformed — fall through to defaults.
	}

	cfg.Defaults()
	return cfg
}

// LoadProject reads the project config from <repoPath>/.suitcode/config.json.
// Missing or unreadable files are silently replaced with defaults.
func LoadProject(repoPath string) ProjectConfig {
	var cfg ProjectConfig

	path := filepath.Join(repoPath, SuitCodeDir, "config.json")
	if err := loadJSON(path, &cfg); err != nil {
		// File missing or malformed — fall through to defaults.
	}

	cfg.Defaults()
	return cfg
}

// StateDirForRepo returns the absolute path to the repository-local state
// directory, creating it if it does not exist.
func StateDirForRepo(repoPath string) (string, error) {
	dir := filepath.Join(repoPath, SuitCodeDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating state dir %q: %w", dir, err)
	}
	return dir, nil
}

// GlobalStateDir returns the path to the user-global state directory,
// creating it if needed.
func GlobalStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	dir := filepath.Join(home, SuitCodeDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating global state dir %q: %w", dir, err)
	}
	return dir, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func loadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parsing %q: %w", path, err)
	}
	return nil
}
