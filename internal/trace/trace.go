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
  status     TEXT,
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
		"PRAGMA synchronous=NORMAL", // WAL keeps this crash-safe without a per-statement fsync
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

// NewRunID builds the canonical run ID <UTC timestamp>_<workflow>_<pid>. The
// timestamp is second-resolution, so two runs of the same workflow starting in
// the same second would collide on the primary key; the pid settles it,
// because one lathe process runs exactly one run.
func NewRunID(workflow string) string {
	return fmt.Sprintf("%s_%s_%d", time.Now().UTC().Format("20060102T150405Z"), workflow, os.Getpid())
}

// NewPhase builds a phase with the canonical ID <runID>_<seq, two digits>_<name>,
// keeping the ID convention in the package that owns the schema.
func NewPhase(runID string, seq int, name, kind, owner string) *Phase {
	return &Phase{
		ID:    fmt.Sprintf("%s_%02d_%s", runID, seq, name),
		RunID: runID,
		Seq:   seq,
		Name:  name,
		Kind:  kind,
		Owner: owner,
	}
}

// Finish stamps the phase's end now and settles its status, so no caller ever
// formats a time. Write it back with PhaseUpsert.
func (p *Phase) Finish(status, errMsg string) {
	p.Status, p.Error, p.End = status, errMsg, nowUTC()
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

// RunInterrupt settles a run killed by a signal. It leaves tokens and cost
// alone so a signal handler never has to read counters the run is still
// updating; the usage events hold the spend either way.
func (d *DB) RunInterrupt(runID string) error {
	_, err := d.sql.Exec(
		`UPDATE runs SET status = 'fail', ended_at = ? WHERE run_id = ? AND ended_at IS NULL`,
		nowUTC(), runID)
	return err
}

// PhaseUpsert writes the whole phase row, creating or replacing it. Leaving
// p.Start empty stamps it now (and sticks, so the second write keeps it);
// p.End stays empty until Finish sets it.
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

// Row is one run as `lathe runs` lists it and the dashboard renders it.
type Row struct {
	ID       string  `json:"run_id"`
	Workflow string  `json:"workflow"`
	Repo     string  `json:"repo"`
	Request  string  `json:"request"`
	Status   string  `json:"status"`
	Started  string  `json:"started_at"`
	Ended    string  `json:"ended_at"`
	Tokens   int     `json:"tokens"`
	Cost     float64 `json:"cost"`
}

// Recent returns the n most recent runs across every repo — the whole point of
// one global database.
func (d *DB) Recent(n int) ([]Row, error) {
	rows, err := d.sql.Query(
		`SELECT run_id, workflow, repo, request, status, started_at, ended_at, tokens, cost
		 FROM runs ORDER BY started_at DESC, run_id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Row{}
	for rows.Next() {
		var r Row
		var ended sql.NullString
		if err := rows.Scan(&r.ID, &r.Workflow, &r.Repo, &r.Request, &r.Status, &r.Started, &ended, &r.Tokens, &r.Cost); err != nil {
			return nil, err
		}
		r.Ended = ended.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// OpenRO opens runs.db read-only, which is what the dashboard uses: a UI bug
// cannot write, and mode=ro fails loudly if the file is not there rather than
// creating an empty database that looks like "no runs yet".
func OpenRO(dataRoot string) (*DB, error) {
	path := filepath.Join(dataRoot, "runs.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no trace database at %s — run something first", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: db}, nil
}

// PhaseRow and EventRow are phases and events as the dashboard reads them
// back. Payload is passed through unparsed: it went in as JSON and the browser
// is the one that wants it.
type PhaseRow struct {
	ID     string `json:"phase_id"`
	Seq    int    `json:"seq"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Owner  string `json:"owner"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Start  string `json:"started_at"`
	End    string `json:"ended_at"`
}

type EventRow struct {
	ID      int64           `json:"event_id"`
	PhaseID string          `json:"phase_id"`
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
	At      string          `json:"at"`
}

// Get returns one run. A missing run is sql.ErrNoRows.
func (d *DB) Get(runID string) (Row, error) {
	var r Row
	var ended sql.NullString
	err := d.sql.QueryRow(
		`SELECT run_id, workflow, repo, request, status, started_at, ended_at, tokens, cost
		 FROM runs WHERE run_id = ?`, runID).
		Scan(&r.ID, &r.Workflow, &r.Repo, &r.Request, &r.Status, &r.Started, &ended, &r.Tokens, &r.Cost)
	r.Ended = ended.String
	return r, err
}

// Phases returns a run's phases in the order they ran.
func (d *DB) Phases(runID string) ([]PhaseRow, error) {
	rows, err := d.sql.Query(
		`SELECT phase_id, seq, name, kind, owner, status, error, started_at, ended_at
		 FROM phases WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PhaseRow{}
	for rows.Next() {
		var p PhaseRow
		var errMsg, end sql.NullString
		if err := rows.Scan(&p.ID, &p.Seq, &p.Name, &p.Kind, &p.Owner, &p.Status, &errMsg, &p.Start, &end); err != nil {
			return nil, err
		}
		p.Error, p.End = errMsg.String, end.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// Events returns a run's events with event_id greater than after, which is the
// dashboard's poll cursor: the same query serves live tailing and replay.
// limit bounds one poll; the cursor picks the rest up on the next one.
func (d *DB) Events(runID string, after int64, limit int) ([]EventRow, error) {
	rows, err := d.sql.Query(
		`SELECT event_id, phase_id, type, name, payload, at
		 FROM events WHERE run_id = ? AND event_id > ? ORDER BY event_id LIMIT ?`,
		runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []EventRow{}
	for rows.Next() {
		var e EventRow
		var phaseID, payload sql.NullString
		if err := rows.Scan(&e.ID, &phaseID, &e.Type, &e.Name, &payload, &e.At); err != nil {
			return nil, err
		}
		e.PhaseID = phaseID.String
		if payload.Valid {
			e.Payload = json.RawMessage(payload.String)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
