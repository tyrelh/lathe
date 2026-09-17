// Package trace records runs, phases, and events into one global SQLite
// database shared by every repo lathe is invoked from.
package trace

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go: cross-compiles without a C toolchain
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  run_id     TEXT PRIMARY KEY,
  workflow   TEXT,
  repo       TEXT,
  request    TEXT,
  status     TEXT,
  started_at TEXT,
  ended_at   TEXT,
  tokens     INTEGER DEFAULT 0,
  cost       REAL    DEFAULT 0
);

CREATE TABLE IF NOT EXISTS phases (
  phase_id   TEXT PRIMARY KEY,
  run_id     TEXT REFERENCES runs,
  seq        INTEGER,
  name       TEXT,
  kind       TEXT,
  owner      TEXT,
  status     TEXT DEFAULT 'fail',
  error      TEXT,
  started_at TEXT,
  ended_at   TEXT
);

CREATE TABLE IF NOT EXISTS events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id   TEXT,
  phase_id TEXT,
  type     TEXT,
  name     TEXT,
  payload  TEXT,
  at       TEXT
);
`

// DataRoot is where the database and per-run artifacts live: $XDG_DATA_HOME/lathe,
// falling back to ~/.local/share/lathe.
func DataRoot() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "lathe"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "lathe"), nil
}

// DB is the single writer. Every lathe process holds one.
type DB struct{ sql *sql.DB }

// Open creates dataRoot if needed and opens runs.db inside it, applying the
// schema. Close it when the run ends.
func Open(dataRoot string) (*DB, error) {
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataRoot, "runs.db"))
	if err != nil {
		return nil, err
	}
	// One connection makes the writer serial by construction; WAL still lets
	// readers (the dashboard) in, and busy_timeout makes a second lathe process
	// wait its turn instead of failing.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &DB{sql: db}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// Phase is one row of the phases table. The caller holds it for the phase's
// lifetime and writes it twice: once at start, once at end.
type Phase struct {
	ID     string
	RunID  string
	Seq    int
	Name   string
	Kind   string // engineer | agent | code
	Owner  string // agent name, or 'engineer'
	Status string // fail until success is earned
	Error  string
	Start  string
	End    string
}

// RunStart records a run as running. Every timestamp in the database comes
// from nowUTC so a Mac and a VPS interleave correctly.
func (d *DB) RunStart(runID, workflow, repo, request string) error {
	_, err := d.sql.Exec(
		`INSERT INTO runs (run_id, workflow, repo, request, status, started_at)
		 VALUES (?, ?, ?, ?, 'running', ?)`,
		runID, workflow, repo, request, nowUTC())
	return err
}

// RunFinish settles a run's status and its accumulated spend.
func (d *DB) RunFinish(runID, status string, tokens int, cost float64) error {
	_, err := d.sql.Exec(
		`UPDATE runs SET status = ?, ended_at = ?, tokens = ?, cost = ? WHERE run_id = ?`,
		status, nowUTC(), tokens, cost, runID)
	return err
}

// PhaseUpsert writes the whole phase row, creating or replacing it. Leaving
// p.Start or p.End empty stamps them now, so a caller never formats a time.
func (d *DB) PhaseUpsert(p *Phase) error {
	if p.Start == "" {
		p.Start = nowUTC()
	}
	if p.Status == "" {
		p.Status = "fail"
	}
	_, err := d.sql.Exec(
		`INSERT INTO phases (phase_id, run_id, seq, name, kind, owner, status, error, started_at, ended_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(phase_id) DO UPDATE SET
		   seq=excluded.seq, name=excluded.name, kind=excluded.kind, owner=excluded.owner,
		   status=excluded.status, error=excluded.error,
		   started_at=excluded.started_at, ended_at=excluded.ended_at`,
		p.ID, p.RunID, p.Seq, p.Name, p.Kind, p.Owner, p.Status, p.Error, p.Start, nullIfEmpty(p.End))
	return err
}

// Event appends to the one append-only table. payload is marshalled to JSON;
// pass nil for none. A payload that will not marshal is stored as its error
// rather than losing the event.
func (d *DB) Event(runID, phaseID, typ, name string, payload any) error {
	var blob any
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			b, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
		}
		blob = string(b)
	}
	_, err := d.sql.Exec(
		`INSERT INTO events (run_id, phase_id, type, name, payload, at) VALUES (?, ?, ?, ?, ?, ?)`,
		runID, nullIfEmpty(phaseID), typ, name, blob, nowUTC())
	return err
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
