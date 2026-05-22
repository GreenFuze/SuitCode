// Package calllog appends structured call records to <repo>/.suitcode/calls.jsonl.
// All fields use relative paths only — no code content or absolute paths are ever written.
package calllog

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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

// PrintSummary writes a human-readable tabular summary of the most recent
// records to w. Pass last = 0 to show all records.
func (l *Logger) PrintSummary(w io.Writer, last int) error {
	records, err := l.LoadAll()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		fmt.Fprintln(w, "No call records found in", l.path)
		return nil
	}

	// Trim to last N if requested.
	if last > 0 && len(records) > last {
		records = records[len(records)-last:]
	}

	// Header row.
	fmt.Fprintf(w, "%-20s  %-18s  %-8s  %-12s  %-8s  %-10s\n",
		"Feature", "Time", "Files", "Budget", "Latency", "Compression")
	fmt.Fprintln(w, strings.Repeat("-", 85))

	// Data rows.
	for _, r := range records {
		ts := r.TS
		if t, parseErr := time.Parse(time.RFC3339, r.TS); parseErr == nil {
			ts = t.Local().Format("2006-01-02 15:04")
		}

		filesCol := "-"
		if r.CandidatesTotal > 0 {
			filesCol = fmt.Sprintf("%d/%d", r.FilesIncluded, r.CandidatesTotal)
		}

		budgetCol := fmt.Sprintf("%d", r.BudgetUsed)
		if r.BudgetRequested > 0 {
			budgetCol = fmt.Sprintf("%d/%d", r.BudgetUsed, r.BudgetRequested)
		}

		latencyCol := fmt.Sprintf("%dms", r.LatencyMs)

		compressionCol := "-"
		if r.CandidatesTotal > 0 {
			saved := int((1 - r.CompressionRatio) * 100)
			compressionCol = fmt.Sprintf("%d%%", saved)
		}

		fmt.Fprintf(w, "%-20s  %-18s  %-8s  %-12s  %-8s  %-10s\n",
			truncate(r.Feature, 20),
			ts,
			filesCol,
			budgetCol,
			latencyCol,
			compressionCol,
		)
	}

	fmt.Fprintf(w, "\n%d records  ·  %s\n", len(records), l.path)
	return nil
}

// Export packages the call log into a zip archive at outputPath.
// The zip contains only the JSONL file — no code content or absolute paths.
// outputPath is created or truncated; its parent directory must already exist.
func (l *Logger) Export(outputPath string) error {
	src := l.path
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("calllog: no call log found at %s", src)
		}
		return fmt.Errorf("calllog: stat %q: %w", src, err)
	}

	// Create the output zip.
	zf, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("calllog: create zip %q: %w", outputPath, err)
	}
	defer zf.Close()

	zw := zip.NewWriter(zf)
	defer zw.Close()

	// Open the source JSONL file.
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("calllog: open source %q: %w", src, err)
	}
	defer srcFile.Close()

	// Add the JSONL file to the archive under its base name.
	entry, err := zw.Create(filepath.Base(src))
	if err != nil {
		return fmt.Errorf("calllog: create zip entry: %w", err)
	}

	if _, err := io.Copy(entry, srcFile); err != nil {
		return fmt.Errorf("calllog: write zip entry: %w", err)
	}

	return nil
}

// truncate shortens s to at most n runes, appending "…" if truncated.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}
