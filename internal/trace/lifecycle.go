package trace

// The run lifecycle lives here: submission, dispatch, claim, heartbeat,
// cancellation, completion and reconciliation. Every one of them is a
// conditional update inside a transaction, because the whole point of the
// manager/worker split is that two processes can be wrong about who owns a run
// and only the database gets to settle it. Scheduling and workflow code call
// these; neither writes SQL.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Run states. queued -> starting -> running -> ok | fail | cancelled, with
// lost reachable from starting or running when a worker disappears.
const (
	StatusQueued    = "queued"
	StatusStarting  = "starting"
	StatusRunning   = "running"
	StatusOK        = "ok"
	StatusFail      = "fail"
	StatusCancelled = "cancelled"
	StatusLost      = "lost"
)

// Terminal reports whether a state can still change. Terminal states are
// immutable: a late worker write against one is dropped, not applied.
func Terminal(status string) bool {
	switch status {
	case StatusOK, StatusFail, StatusCancelled, StatusLost:
		return true
	}
	return false
}

var (
	// ErrDuplicateBuild is a second outstanding build for one checkout.
	ErrDuplicateBuild = errors.New("a build is already outstanding for this checkout")
	// ErrClaimLost means another process owns this attempt now, or the run is
	// already terminal. A worker that sees it stops without writing anything.
	ErrClaimLost = errors.New("this attempt is no longer ours")
	// ErrAlreadyDone is a cancellation that lost the race with completion.
	ErrAlreadyDone = errors.New("already completed")
)

// Request is what the submitter records. Spec is the versioned execution
// specification: everything configuration could otherwise change under a
// queued run. Repository contents and worker secrets are not in it.
type Request struct {
	Workflow string
	Repo     string
	Request  string
	Branch   string
	Spec     []byte
}

// Submit records a queued run and returns its ID. The duplicate-build check
// and the insert share one transaction, so two submitters cannot both see an
// empty queue.
func (d *DB) Submit(r Request) (string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if r.Workflow == "build" {
		var other string
		err := tx.QueryRow(
			`SELECT run_id FROM runs WHERE repo = ? AND workflow = 'build'
			   AND status IN ('queued', 'starting', 'running') LIMIT 1`, r.Repo).Scan(&other)
		switch {
		case err == nil:
			return "", fmt.Errorf("%w: %s", ErrDuplicateBuild, other)
		case !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	}

	id := NewRunID(r.Workflow)
	if _, err := tx.Exec(
		`INSERT INTO runs (run_id, workflow, repo, request, status, spec, branch, submitted_at)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?)`,
		id, r.Workflow, r.Repo, r.Request, string(r.Spec), r.Branch, nowUTC()); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// Queued returns waiting runs, oldest first: the manager dispatches in
// submission order.
func (d *DB) Queued() ([]Row, error) {
	rows, err := d.sql.Query(runColumns + ` FROM runs WHERE status = 'queued' ORDER BY submitted_at, run_id`)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// Active returns the runs that hold capacity: dispatched but not settled.
func (d *DB) Active() ([]Row, error) {
	rows, err := d.sql.Query(runColumns + ` FROM runs WHERE status IN ('starting', 'running') ORDER BY submitted_at`)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// Attempt is one dispatch of one run.
type Attempt struct {
	ID         string
	RunID      string
	Dispatched string
	Claimed    string
	PID        int
	Host       string
	Handle     string
	LogPath    string
	Heartbeat  string
	LeaseUntil string
	Outcome    string
}

// NewAttemptID derives a dispatch identity from the run it belongs to, so a
// log directory sorts beside its run and a stray file names its owner.
func NewAttemptID(runID string) string { return runID + "_a" + randomHex(3) }

// NewClaimToken is what binds a worker's writes to its own claim.
func NewClaimToken() string { return randomHex(16) }

// Reserve records the intent to dispatch before anything is launched. A
// manager that dies between this and the launch leaves a starting run with an
// unclaimed attempt, which is exactly what reconciliation looks for.
func (d *DB) Reserve(runID, attemptID, logPath string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE runs SET status = 'starting', attempt_id = ? WHERE run_id = ? AND status = 'queued'`,
		attemptID, runID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrClaimLost
	}
	if _, err := tx.Exec(
		`INSERT INTO attempts (attempt_id, run_id, dispatched_at, log_path) VALUES (?, ?, ?, ?)`,
		attemptID, runID, nowUTC(), logPath); err != nil {
		return err
	}
	return tx.Commit()
}

// SetHandle persists whatever the launcher returned — a PID locally, an
// instance ID later — so a restarted manager can ask about a worker it never
// spawned itself.
func (d *DB) SetHandle(attemptID, handle string, pid int) error {
	_, err := d.sql.Exec(
		`UPDATE attempts SET handle = ?, worker_pid = ? WHERE attempt_id = ?`, handle, pid, attemptID)
	return err
}

// Claim is the worker taking ownership. It is conditional on the attempt being
// unclaimed and its run still starting, so a duplicate launch loses here,
// before it does any work.
func (d *DB) Claim(attemptID, token, host string, pid int, lease time.Duration) (Row, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return Row{}, err
	}
	defer tx.Rollback()

	at := nowUTC()
	res, err := tx.Exec(
		`UPDATE attempts SET claimed_at = ?, claim_token = ?, worker_pid = ?, worker_host = ?,
		   heartbeat_at = ?, lease_until = ?
		 WHERE attempt_id = ? AND claimed_at IS NULL`,
		at, token, pid, host, at, until(lease), attemptID)
	if err != nil {
		return Row{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Row{}, err
	} else if n == 0 {
		return Row{}, ErrClaimLost
	}

	var runID string
	if err := tx.QueryRow(`SELECT run_id FROM attempts WHERE attempt_id = ?`, attemptID).Scan(&runID); err != nil {
		return Row{}, err
	}
	res, err = tx.Exec(
		`UPDATE runs SET status = 'running', started_at = ? WHERE run_id = ? AND attempt_id = ? AND status = 'starting'`,
		at, runID, attemptID)
	if err != nil {
		return Row{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Row{}, err
	} else if n == 0 {
		return Row{}, ErrClaimLost
	}

	rows, err := tx.Query(runColumns+` FROM runs WHERE run_id = ?`, runID)
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
	return out[0], tx.Commit()
}

// Beat extends the lease and reports whether a cancellation is waiting. The
// worker polls cancellation here rather than in a second query because the
// heartbeat already has to run on its own clock, independent of model output.
func (d *DB) Beat(attemptID, token string, lease time.Duration) (cancel bool, err error) {
	res, err := d.sql.Exec(
		`UPDATE attempts SET heartbeat_at = ?, lease_until = ?
		 WHERE attempt_id = ? AND claim_token = ? AND outcome IS NULL`,
		nowUTC(), until(lease), attemptID, token)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, err
	} else if n == 0 {
		return false, ErrClaimLost
	}
	var cancelAt sql.NullString
	err = d.sql.QueryRow(
		`SELECT cancel_at FROM runs WHERE attempt_id = ?`, attemptID).Scan(&cancelAt)
	return cancelAt.Valid, err
}

// RequestCancel records a cancellation. A queued run is cancelled outright; an
// active one keeps running until its worker notices. Whichever of cancellation
// and completion commits first decides the race.
func (d *DB) RequestCancel(runID string) (status string, err error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if err := tx.QueryRow(`SELECT status FROM runs WHERE run_id = ?`, runID).Scan(&status); err != nil {
		return "", err
	}
	if Terminal(status) {
		return status, ErrAlreadyDone
	}
	at := nowUTC()
	if status == StatusQueued {
		if _, err := tx.Exec(
			`UPDATE runs SET status = 'cancelled', cancel_at = ?, ended_at = ?, reason = 'cancelled while queued'
			 WHERE run_id = ? AND status = 'queued'`, at, at, runID); err != nil {
			return "", err
		}
		return StatusCancelled, tx.Commit()
	}
	if _, err := tx.Exec(
		`UPDATE runs SET cancel_at = COALESCE(cancel_at, ?) WHERE run_id = ?`, at, runID); err != nil {
		return "", err
	}
	return status, tx.Commit()
}

// Cancelled reports whether a cancellation request is recorded, which is how a
// worker finishing normally learns it should record cancelled instead.
func (d *DB) Cancelled(runID string) (bool, error) {
	var at sql.NullString
	err := d.sql.QueryRow(`SELECT cancel_at FROM runs WHERE run_id = ?`, runID).Scan(&at)
	return at.Valid, err
}

// Complete settles a run from its worker. It is bound to the current attempt
// and claim token, so a stale worker cannot overwrite a terminal outcome.
func (d *DB) Complete(runID, attemptID, token, status, reason string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`UPDATE runs SET status = ?, reason = ?, ended_at = ?
		 WHERE run_id = ? AND attempt_id = ? AND status IN ('starting', 'running')
		   AND EXISTS (SELECT 1 FROM attempts WHERE attempt_id = ? AND claim_token = ?)`,
		status, reason, nowUTC(), runID, attemptID, attemptID, token)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrClaimLost
	}
	if _, err := tx.Exec(`UPDATE attempts SET outcome = ? WHERE attempt_id = ?`, status, attemptID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkLost settles a run whose worker is gone with its outcome unknown. It is
// the manager's write, so it is not bound to a claim token.
func (d *DB) MarkLost(runID, reason string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE runs SET status = 'lost', reason = ?, ended_at = ?
		 WHERE run_id = ? AND status IN ('starting', 'running')`, reason, nowUTC(), runID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE attempts SET outcome = 'lost' WHERE run_id = ? AND outcome IS NULL`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetCommit records the commit execution actually read, which is not
// necessarily submission-time HEAD.
func (d *DB) SetCommit(runID, sha string) error {
	_, err := d.sql.Exec(`UPDATE runs SET commit_sha = ? WHERE run_id = ?`, sha, runID)
	return err
}

// GetAttempt returns one attempt. A missing one is sql.ErrNoRows.
func (d *DB) GetAttempt(attemptID string) (Attempt, error) {
	var a Attempt
	var claimed, host, handle, logPath, beat, lease, outcome, token sql.NullString
	err := d.sql.QueryRow(
		`SELECT attempt_id, run_id, dispatched_at, claimed_at, claim_token, worker_pid,
		        worker_host, handle, log_path, heartbeat_at, lease_until, outcome
		 FROM attempts WHERE attempt_id = ?`, attemptID).
		Scan(&a.ID, &a.RunID, &a.Dispatched, &claimed, &token, &a.PID,
			&host, &handle, &logPath, &beat, &lease, &outcome)
	a.Claimed, a.Host, a.Handle = claimed.String, host.String, handle.String
	a.LogPath, a.Heartbeat, a.LeaseUntil, a.Outcome = logPath.String, beat.String, lease.String, outcome.String
	return a, err
}

// ReserveCheckout records that a run owns a working directory. It is stored
// separately from run state because a lost run may still have a live process
// in that directory; releasing it is a separate decision.
func (d *DB) ReserveCheckout(path, host, runID string) error {
	res, err := d.sql.Exec(
		`INSERT INTO reservations (path, host, run_id, at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(path) DO NOTHING`, path, host, runID, nowUTC())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		var holder string
		if err := d.sql.QueryRow(`SELECT run_id FROM reservations WHERE path = ?`, path).Scan(&holder); err != nil {
			return err
		}
		if holder == runID {
			return nil
		}
		return fmt.Errorf("%s is reserved by run %s", path, holder)
	}
	return nil
}

// ReleaseCheckout drops a run's reservation. Only a caller that knows the
// worker has stopped may call it: clearing the row does not stop a process.
func (d *DB) ReleaseCheckout(runID string) error {
	_, err := d.sql.Exec(`DELETE FROM reservations WHERE run_id = ?`, runID)
	return err
}

// HeldCheckouts is every reserved path, keyed by the run holding it.
func (d *DB) HeldCheckouts() (map[string]string, error) {
	rows, err := d.sql.Query(`SELECT path, run_id FROM reservations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := map[string]string{}
	for rows.Next() {
		var path, runID string
		if err := rows.Scan(&path, &runID); err != nil {
			return nil, err
		}
		held[path] = runID
	}
	return held, rows.Err()
}

func until(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339) }
