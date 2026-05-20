// Package calllog appends structured call records to <repo>/.suitcode/calls.jsonl.
// All fields use relative paths only — no code content or absolute paths are ever written.
package calllog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GreenFuze/SuitCode/core/config"
)

// FileName is the name of the JSONL log file inside the .suitcode directory.
const FileName = "calls.jsonl"

// Record is one feature-call entry in the call log.
// All path fields are relative to the repository root.
// Privacy invariant: never include file content, absolute paths, or user-identifiable data.
type Record struct {
	TS                 string   `json:"ts"`
	Feature            string   `json:"feature"`
	SeedFiles          []string `json:"seed_files,omitempty"`
	FilesReturned      []string `json:"files_returned,omitempty"`
	CandidatesTotal    int      `json:"candidates_total"`
	FilesIncluded      int      `json:"files_included"`
	CompressionRatio   float64  `json:"compression_ratio"`
	BudgetRequested    int      `json:"budget_requested"`
	BudgetUsed         int      `json:"budget_used"`
	LatencyMs          int64    `json:"latency_ms"`
	ImportEdgesScanned int      `json:"import_edges_scanned"`
	LspEnhanced        bool     `json:"lsp_enhanced"`
}

// Logger appends Records to <repoPath>/.suitcode/calls.jsonl.
// It is safe for concurrent use.
type Logger struct {
	mu   sync.Mutex
	path string
}

// New creates a Logger for the given repository root.
// The .suitcode directory is created if it does not exist.
func New(repoPath string) (*Logger, error) {
	stateDir, err := config.StateDirForRepo(repoPath)
	if err != nil {
		return nil, fmt.Errorf("calllog: %w", err)
	}
	return &Logger{path: filepath.Join(stateDir, FileName)}, nil
}

// Append writes r as a JSON line. TS is set to now (UTC) if empty.
// Callers should treat failures as non-fatal warnings; logging must never block a feature call.
func (l *Logger) Append(r Record) error {
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("calllog: marshal: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("calllog: open %q: %w", l.path, err)
	}
	defer f.Close()

	_, err = fmt.Fprintf(f, "%s\n", data)
	return err
}

// LoadAll reads all records from the JSONL file in chronological order.
// Returns an empty slice if the file does not exist.
// Malformed lines are silently skipped.
func (l *Logger) LoadAll() ([]Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("calllog: open %q: %w", l.path, err)
	}
	defer f.Close()

	var records []Record
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // skip malformed lines
		}
		records = append(records, r)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("calllog: read %q: %w", l.path, err)
	}
	return records, nil
}

// Path returns the absolute path to the JSONL file.
func (l *Logger) Path() string { return l.path }
