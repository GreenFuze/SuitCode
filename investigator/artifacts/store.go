// Package artifacts provides a SQLite-backed store for persisting feature run
// metrics, traces, responses, and eval results under <repo>/.suitcode/.
package artifacts

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/GreenFuze/SuitCode/core/config"
	"github.com/GreenFuze/SuitCode/core/features"
	_ "modernc.org/sqlite" // register "sqlite" driver
)

const dbName = "store.db"

// Store persists SuitCode artefacts in a SQLite database located at
// <repoPath>/.suitcode/store.db.
type Store struct {
	db       *sql.DB
	repoPath string
}

// Open opens (or creates) the SQLite database for the given repository and
// applies any pending schema migrations.
func Open(repoPath string) (*Store, error) {
	stateDir, err := config.StateDirForRepo(repoPath)
	if err != nil {
		return nil, fmt.Errorf("artifacts store: %w", err)
	}

	dbPath := filepath.Join(stateDir, dbName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("artifacts store: opening %q: %w", dbPath, err)
	}

	// SQLite pragmas for reliability and performance.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=3000",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("artifacts store: pragma %q: %w", p, err)
		}
	}

	s := &Store{db: db, repoPath: repoPath}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("artifacts store: migration: %w", err)
	}

	return s, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// ──────────────────────────────────────────────────────────────────────────────
// Feature run persistence
// ──────────────────────────────────────────────────────────────────────────────

// SaveRun persists the metrics and trace from a feature run. The response JSON
// is optional and may be nil.
func (s *Store) SaveRun(runID features.RunID, feature string, metrics features.FeatureMetrics, trace features.FeatureTrace, responseJSON []byte) error {
	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		return fmt.Errorf("artifacts store: marshalling metrics: %w", err)
	}

	traceJSON, err := json.Marshal(trace)
	if err != nil {
		return fmt.Errorf("artifacts store: marshalling trace: %w", err)
	}

	if responseJSON == nil {
		responseJSON = []byte("{}")
	}

	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO runs
		   (id, feature, repo_path, started_at, finished_at, metrics_json, trace_json, response_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(runID),
		feature,
		s.repoPath,
		metrics.Timing.StartedAt.UTC().Format(time.RFC3339),
		metrics.Timing.FinishedAt.UTC().Format(time.RFC3339),
		string(metricsJSON),
		string(traceJSON),
		string(responseJSON),
	)
	if err != nil {
		return fmt.Errorf("artifacts store: inserting run %s: %w", runID, err)
	}

	return nil
}

// LoadRunMetrics retrieves the FeatureMetrics for a given run ID.
func (s *Store) LoadRunMetrics(runID features.RunID) (*features.FeatureMetrics, error) {
	row := s.db.QueryRow(
		`SELECT metrics_json FROM runs WHERE id = ?`,
		string(runID),
	)

	var metricsJSON string
	if err := row.Scan(&metricsJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("artifacts store: run %s not found", runID)
		}
		return nil, fmt.Errorf("artifacts store: loading run %s: %w", runID, err)
	}

	var m features.FeatureMetrics
	if err := json.Unmarshal([]byte(metricsJSON), &m); err != nil {
		return nil, fmt.Errorf("artifacts store: unmarshalling metrics for run %s: %w", runID, err)
	}

	return &m, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Eval run persistence
// ──────────────────────────────────────────────────────────────────────────────

// EvalRunRecord is the stored form of an eval run result.
type EvalRunRecord struct {
	ID          string
	Suite       string
	RepoPath    string
	StartedAt   time.Time
	FinishedAt  time.Time
	ResultsJSON string
	SummaryJSON string
}

// SaveEvalRun persists an eval run record.
func (s *Store) SaveEvalRun(record EvalRunRecord) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO eval_runs
		   (id, suite, repo_path, started_at, finished_at, results_json, summary_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.ID,
		record.Suite,
		record.RepoPath,
		record.StartedAt.UTC().Format(time.RFC3339),
		record.FinishedAt.UTC().Format(time.RFC3339),
		record.ResultsJSON,
		record.SummaryJSON,
	)
	if err != nil {
		return fmt.Errorf("artifacts store: saving eval run %s: %w", record.ID, err)
	}
	return nil
}

// LoadEvalRun retrieves an eval run record by ID.
func (s *Store) LoadEvalRun(id string) (*EvalRunRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, suite, repo_path, started_at, finished_at, results_json, summary_json
		 FROM eval_runs WHERE id = ?`,
		id,
	)

	var r EvalRunRecord
	var startedAt, finishedAt string
	if err := row.Scan(&r.ID, &r.Suite, &r.RepoPath, &startedAt, &finishedAt, &r.ResultsJSON, &r.SummaryJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("artifacts store: eval run %s not found", id)
		}
		return nil, fmt.Errorf("artifacts store: loading eval run %s: %w", id, err)
	}

	r.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	r.FinishedAt, _ = time.Parse(time.RFC3339, finishedAt)

	return &r, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Schema migration
// ──────────────────────────────────────────────────────────────────────────────

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS runs (
			id            TEXT PRIMARY KEY NOT NULL,
			feature       TEXT NOT NULL,
			repo_path     TEXT NOT NULL,
			started_at    TEXT NOT NULL,
			finished_at   TEXT,
			metrics_json  TEXT NOT NULL DEFAULT '{}',
			trace_json    TEXT NOT NULL DEFAULT '{}',
			response_json TEXT NOT NULL DEFAULT '{}'
		);

		CREATE TABLE IF NOT EXISTS eval_runs (
			id            TEXT PRIMARY KEY NOT NULL,
			suite         TEXT NOT NULL,
			repo_path     TEXT NOT NULL,
			started_at    TEXT NOT NULL,
			finished_at   TEXT,
			results_json  TEXT NOT NULL DEFAULT '[]',
			summary_json  TEXT NOT NULL DEFAULT '{}'
		);
	`)
	if err != nil {
		return fmt.Errorf("creating tables: %w", err)
	}
	return nil
}
