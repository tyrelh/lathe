// Package trace records runs, phases, and events into one global SQLite
// database shared by every repo lathe is invoked from.
package trace

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // pure Go: cross-compiles without a C toolchain
)

// dbFile is the database this release writes. The v0.2 runs.db is left where
// it is and never opened: a one-shot destructive upgrade path costs more than
// a new filename.
const dbFile = "lathe.db"

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  run_id       TEXT PRIMARY KEY,
  workflow     TEXT,
  repo         TEXT,
  request      TEXT,
  status       TEXT,
  spec         TEXT,
  branch       TEXT,
  commit_sha   TEXT,
  attempt_id   TEXT,
  reason       TEXT,
  submitted_at TEXT,
  started_at   TEXT,
  ended_at     TEXT,
  cancel_at    TEXT,
  tokens       INTEGER DEFAULT 0,
  cost         REAL    DEFAULT 0
);

-- One row per dispatch. The manager writes it before it launches anything, so
-- a crash between the two is reconcilable from the database alone.
CREATE TABLE IF NOT EXISTS attempts (
  attempt_id   TEXT PRIMARY KEY,
  run_id       TEXT REFERENCES runs,
  dispatched_at TEXT,
  claimed_at   TEXT,
  claim_token  TEXT,
  worker_pid   INTEGER DEFAULT 0,
  worker_host  TEXT,
  handle       TEXT,
  log_path     TEXT,
  heartbeat_at TEXT,
  lease_until  TEXT,
  outcome      TEXT
);
CREATE INDEX IF NOT EXISTS attempts_run ON attempts (run_id);

-- A checkout reservation outlives its run's terminal state: a lost run may
-- still have a process sitting in that directory.
CREATE TABLE IF NOT EXISTS reservations (
  path   TEXT PRIMARY KEY,
  host   TEXT,
  run_id TEXT,
  at     TEXT
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

// busyTimeout makes a second connection wait its turn rather than fail. Both
// openers set it, so it is one literal.
const busyTimeout = "PRAGMA busy_timeout=5000"

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

// Open creates dataRoot if needed and opens lathe.db inside it, applying the
// schema. Close it when the run ends.
func Open(dataRoot string) (*DB, error) {
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataRoot, dbFile))
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
		busyTimeout,
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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: db}, nil
}

// Init creates and migrates the database, then closes it. The dashboard calls
// it before opening its read-only connection, so a manager started on a fresh
// machine serves an empty dashboard rather than an error.
func Init(dataRoot string) error {
	db, err := Open(dataRoot)
	if err != nil {
		return err
	}
	return db.Close()
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

// NewRunID builds the canonical run ID <UTC timestamp>_<workflow>_<random>.
// The suffix is random rather than the process ID because after the
// submitter/worker split the submitting process executes nothing, so its PID
// identifies nothing. The format stays sortable, greppable and usable as a
// directory name.
func NewRunID(workflow string) string {
	return fmt.Sprintf("%s_%s_%s", time.Now().UTC().Format("20060102T150405Z"), workflow, randomHex(4))
}

// randomHex is the suffix source for run IDs, attempt IDs and claim tokens.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not a condition worth a second code path;
		// the clock still separates these and the caller would only panic.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
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
	ID        string  `json:"run_id"`
	Workflow  string  `json:"workflow"`
	Repo      string  `json:"repo"`
	Request   string  `json:"request"`
	Status    string  `json:"status"`
	Branch    string  `json:"branch"`
	Commit    string  `json:"commit"`
	AttemptID string  `json:"attempt_id"`
	Reason    string  `json:"reason"`
	Submitted string  `json:"submitted_at"`
	Started   string  `json:"started_at"`
	Ended     string  `json:"ended_at"`
	Cancelled string  `json:"cancel_requested_at"`
	Tokens    int     `json:"tokens"`
	Cost      float64 `json:"cost"`
	// Spec is the execution specification captured at submission. It is only
	// loaded by the worker, so it stays out of the JSON the dashboard reads.
	Spec []byte `json:"-"`
}

// Recent returns the n most recent runs across every repo — the whole point of
// one global database.
func (d *DB) Recent(n int) ([]Row, error) {
	rows, err := d.sql.Query(
		runColumns+` FROM runs ORDER BY submitted_at DESC, run_id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// OpenRO opens lathe.db read-only, which is what the dashboard uses: a UI bug
// cannot write, and mode=ro fails loudly if the file is not there rather than
// creating an empty database that looks like "no runs yet".
func OpenRO(dataRoot string) (*DB, error) {
	path := filepath.Join(dataRoot, dbFile)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no trace database at %s — run something first", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(busyTimeout); err != nil {
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
	rows, err := d.sql.Query(runColumns+` FROM runs WHERE run_id = ?`, runID)
	if err != nil {
		return Row{}, err
	}
	out, err := scanRows(rows)
	if err != nil {
		return Row{}, err
	}
	if len(out) == 0 {
		return Row{}, sql.ErrNoRows
	}
	return out[0], nil
}

// Phases returns a run's phases in the order they ran.
func (d *DB) Phases(runID string) ([]PhaseRow, error) {
	rows, err := d.sql.Query(
		`SELECT phase_id, seq, name, owner, status, error, started_at, ended_at
		 FROM phases WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PhaseRow{}
	for rows.Next() {
		var p PhaseRow
		var errMsg, end sql.NullString
		if err := rows.Scan(&p.ID, &p.Seq, &p.Name, &p.Owner, &p.Status, &errMsg, &p.Start, &end); err != nil {
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
