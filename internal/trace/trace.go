// Package trace records runs, phases, and events into one global SQLite
// database shared by every repo lathe is invoked from.
package trace

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modernc.org/sqlite" // pure Go: cross-compiles without a C toolchain
	sqlite3 "modernc.org/sqlite/lib"
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
  kind       TEXT, -- unwritten since the phase graph; left in place rather than migrated away
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

// busyMillis is how long a connection waits for a lock rather than fail.
const busyMillis = "5000"

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
	if err := Private(dataRoot); err != nil {
		return nil, err
	}
	// Created private before SQLite opens it, and tightened if it exists:
	// the database holds supervisor stop tokens and run history. SQLite
	// gives the WAL sidecars it creates the database's mode.
	path := filepath.Join(dataRoot, dbFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	// Two connections switching a fresh database to WAL at once can fail
	// with SQLITE_IOERR rather than busy, so initialization is serialized.
	unlock, err := lockInit(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// Transactions take the write lock when they begin. A deferred one that
	// reads and then writes fails at once with SQLITE_BUSY if another process
	// wrote in between, and busy_timeout cannot wait that out.
	//
	// The busy timeout is in the DSN so it holds on every connection before
	// its first statement: switching to WAL takes a lock, and a second lathe
	// opening the same database meanwhile must wait rather than fail.
	db, err := sql.Open("sqlite", databaseURI(path)+"?_txlock=immediate&_pragma=busy_timeout("+busyMillis+")")
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
		"PRAGMA foreign_keys=ON",
	} {
		if err := execRetrying(db, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if err := execRetrying(db, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: db}, nil
}

// lockInit takes an exclusive lock on a file beside the database at path,
// shared by every process, and returns its release. It blocks until the lock
// is free.
func lockInit(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	// Closing the file releases the flock.
	return func() { f.Close() }, nil
}

// initRetries bounds how often a pragma that SQLite refused for contention
// is tried again. Some lock waits, such as WAL recovery by another
// connection (SQLITE_BUSY_RECOVERY), return busy without the busy handler.
const initRetries = 20

// retryPause is the first pause between contended tries; each later one is
// longer by the same step.
const retryPause = 25 * time.Millisecond

// retryContended runs op, retrying contention with growing pauses up to
// initRetries times. Any other error, or the last try's result, is returned.
func retryContended[T any](op func() (T, error)) (T, error) {
	var v T
	var err error
	for try := 0; try <= initRetries; try++ {
		if v, err = op(); err == nil || !contended(err) {
			return v, err
		}
		time.Sleep(time.Duration(try+1) * retryPause)
	}
	return v, err
}

// execRetrying runs stmt with retryContended.
func execRetrying(db *sql.DB, stmt string) error {
	_, err := retryContended(func() (sql.Result, error) { return db.Exec(stmt) })
	return err
}

// contended reports whether err is SQLite reporting a lock held by another
// connection rather than a fault.
func contended(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	// The primary code; extended ones such as SQLITE_BUSY_RECOVERY carry it
	// in their low byte.
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	}
	return false
}

// Private creates dir with owner-only access, or tightens an existing one to
// it. A path that is a symlink or not owned by this user is refused rather
// than tightened.
func Private(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user; lathe will not keep its data there", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
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
	Owner  string // agent name, or 'engineer'
	Status string // fail until success is earned
	Error  string
	Start  string
	End    string
	// Iteration is the submitted request this phase works on. Attempt, when
	// set, binds the write to the worker that owns the run: a worker that lost
	// its claim, or one from an earlier iteration, writes nothing.
	Iteration int
	Attempt   string
}

// NewRunID returns <UTC timestamp>_<workflow>_<8 hex chars>: sortable,
// greppable and safe as a directory name.
func NewRunID(workflow string) string {
	// Random, not the PID: the submitting process executes nothing, so its PID
	// identifies nothing.
	return fmt.Sprintf("%s_%s_%s", time.Now().UTC().Format("20060102T150405Z"), workflow, randomHex(4))
}

// RunIDSuffix returns the random part of an ID built by NewRunID.
func RunIDSuffix(id string) string { return id[strings.LastIndex(id, "_")+1:] }

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
func NewPhase(runID string, seq int, name, owner string) *Phase {
	return &Phase{
		ID:    fmt.Sprintf("%s_%02d_%s", runID, seq, name),
		RunID: runID,
		Seq:   seq,
		Name:  name,
		Owner: owner,
	}
}

// Finish stamps the phase's end now and settles its status, so no caller ever
// formats a time. Write it back with PhaseUpsert.
func (p *Phase) Finish(status, errMsg string) {
	p.Status, p.Error, p.End = status, errMsg, nowUTC()
}

// Phase states. Phases say success where runs say ok, on purpose: stored
// rows and the dashboard read both vocabularies.
const (
	PhaseSuccess = "success"
	PhaseFail    = "fail"
)

// ExitCancelled is the exit code for a cancelled run: the conventional
// "stopped by a signal".
const ExitCancelled = 130

// PhaseUpsert writes the whole phase row, creating or replacing it. Leaving
// p.Start empty stamps it now (and sticks, so the second write keeps it);
// p.End stays empty until Finish sets it. With p.Attempt set, the write lands
// only while that attempt owns the run, and ErrClaimLost says it did not.
func (d *DB) PhaseUpsert(p *Phase) error {
	if p.Start == "" {
		p.Start = nowUTC()
	}
	if p.Status == "" {
		p.Status = PhaseFail
	}
	res, err := d.sql.Exec(
		`INSERT INTO phases (phase_id, run_id, seq, name, owner, status, error, started_at, ended_at, iteration)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 WHERE ? = '' OR `+owned+`
		 ON CONFLICT(phase_id) DO UPDATE SET
		   seq=excluded.seq, name=excluded.name, owner=excluded.owner,
		   status=excluded.status, error=excluded.error,
		   started_at=excluded.started_at, ended_at=excluded.ended_at`,
		p.ID, p.RunID, p.Seq, p.Name, p.Owner, p.Status, p.Error, p.Start, nullIfEmpty(p.End), p.Iteration,
		p.Attempt, p.RunID, p.Attempt)
	if err != nil {
		return err
	}
	return claimed(res)
}

// owned is the ownership condition a worker's write carries: its attempt is
// the run's current one and the run is still executing. It takes the run ID
// and the attempt ID, in that order.
const owned = `EXISTS (SELECT 1 FROM runs WHERE run_id = ? AND attempt_id = ? AND ` + executing + `)`

// claimed turns a guarded write that changed nothing into ErrClaimLost.
func claimed(res sql.Result) error {
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrClaimLost
	}
	return nil
}

// MaxSeq is the highest phase number a run has used, so a later iteration's
// phases carry on from it: phase IDs embed the number, and none is reused.
func (d *DB) MaxSeq(runID string) (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM phases WHERE run_id = ?`, runID).Scan(&n)
	return n, err
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
	TaskID    *string `json:"task_id"`
	PRState   *string `json:"pr_state"`
	PRStateAt *string `json:"pr_state_at"`
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
	// Iteration is the latest submitted request, counted from zero; Status,
	// Reason and Ended follow it. Activity is when it was submitted, which is
	// what orders the queue and the lists. Submitted and Started stay the
	// run's original creation and first start.
	Iteration int    `json:"iteration"`
	Activity  string `json:"activity_at"`
	// Spec is the execution specification captured at submission. It is only
	// loaded by the worker, so it stays out of the JSON the dashboard reads.
	Spec []byte `json:"-"`
}

// runColumns is the one column list every run query selects, so Row and the
// scanner cannot drift apart across every place runs are read.
const runColumns = `SELECT run_id, workflow, repo, request, status,
	COALESCE(branch, ''), COALESCE(commit_sha, ''), COALESCE(attempt_id, ''),
	COALESCE(reason, ''), COALESCE(submitted_at, ''), COALESCE(started_at, ''),
	COALESCE(ended_at, ''), COALESCE(cancel_at, ''), tokens, cost, COALESCE(spec, ''),
	iteration, COALESCE(activity_at, ''), pr_state, pr_state_at, task_id`

// queryer is a *sql.DB or *sql.Tx.
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// getRow reads one run through q. A missing run is sql.ErrNoRows.
func getRow(q queryer, runID string) (Row, error) {
	rows, err := q.Query(runColumns+` FROM runs WHERE run_id = ?`, runID)
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

func scanRows(rows *sql.Rows) ([]Row, error) {
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		var spec string
		if err := rows.Scan(&r.ID, &r.Workflow, &r.Repo, &r.Request, &r.Status,
			&r.Branch, &r.Commit, &r.AttemptID, &r.Reason, &r.Submitted, &r.Started,
			&r.Ended, &r.Cancelled, &r.Tokens, &r.Cost, &spec, &r.Iteration, &r.Activity, &r.PRState, &r.PRStateAt, &r.TaskID); err != nil {
			return nil, err
		}
		r.Spec = []byte(spec)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Recent returns the n most recently active runs across every repo — the
// whole point of one global database. A revised run moves to the top.
func (d *DB) Recent(n int) ([]Row, error) {
	rows, err := d.sql.Query(
		runColumns+` FROM runs ORDER BY COALESCE(activity_at, '') DESC, run_id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func until(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339) }

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// databaseURI encodes a filesystem path as a SQLite file URI.
func databaseURI(path string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath()
}

// OpenRO opens lathe.db read-only, which is what the dashboard uses: a UI bug
// cannot write, and mode=ro fails loudly if the file is not there rather than
// creating an empty database that looks like "no runs yet".
func OpenRO(dataRoot string) (*DB, error) {
	path := filepath.Join(dataRoot, dbFile)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no trace database at %s — run something first", path)
	}
	db, err := sql.Open("sqlite", databaseURI(path)+"?mode=ro&_pragma=busy_timeout("+busyMillis+")")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
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
	// Iteration is the submitted request the phase worked on.
	Iteration int `json:"iteration"`
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
	return getRow(d.sql, runID)
}

// Phases returns a run's phases in the order they ran.
func (d *DB) Phases(runID string) ([]PhaseRow, error) {
	rows, err := d.sql.Query(
		`SELECT phase_id, seq, name, owner, status, error, started_at, ended_at, iteration
		 FROM phases WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PhaseRow{}
	for rows.Next() {
		var p PhaseRow
		var errMsg, end sql.NullString
		if err := rows.Scan(&p.ID, &p.Seq, &p.Name, &p.Owner, &p.Status, &errMsg, &p.Start, &end, &p.Iteration); err != nil {
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
