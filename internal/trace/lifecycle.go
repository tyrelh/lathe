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
	// ErrDuplicateBuild is a second outstanding writing run for one checkout.
	ErrDuplicateBuild = errors.New("a run that owns this checkout is already outstanding")
	// ErrClaimLost means another process owns this attempt now, or the run is
	// already terminal. A worker that sees it stops without writing anything.
	ErrClaimLost = errors.New("this attempt is no longer ours")
	// ErrAlreadyDone is a cancellation that lost the race with completion.
	ErrAlreadyDone = errors.New("already completed")
	// ErrNotRevisable is a revision the run's recorded state refuses. The
	// wrapped message says what that state is.
	ErrNotRevisable = errors.New("this run cannot take a revision")
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
	// Exclusive is a run that owns its checkout: at most one of them may be
	// outstanding per repository. The submitter sets it from the workflow list,
	// which is what keeps workflow names out of this package.
	Exclusive bool
}

// Submit records a queued run and returns its ID. The exclusivity check and
// the insert share one transaction, so two submitters cannot both see an empty
// queue.
func (d *DB) Submit(r Request) (string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if r.Exclusive {
		var other string
		err := tx.QueryRow(
			`SELECT run_id FROM runs WHERE repo = ? AND exclusive = 1
			   AND status IN ('queued', 'starting', 'running') LIMIT 1`, r.Repo).Scan(&other)
		switch {
		case err == nil:
			return "", fmt.Errorf("%w: %s", ErrDuplicateBuild, other)
		case !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	}

	id, at := NewRunID(r.Workflow), nowUTC()
	if _, err := tx.Exec(
		`INSERT INTO runs (run_id, workflow, repo, request, status, spec, branch, exclusive, submitted_at, activity_at)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?)`,
		id, r.Workflow, r.Repo, r.Request, string(r.Spec), r.Branch, r.Exclusive, at, at); err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO iterations (run_id, iteration, request, spec, status, submitted_at)
		 VALUES (?, 0, ?, ?, 'queued', ?)`, id, r.Request, string(r.Spec), at); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// Revise accepts another request against a finished run: it appends an
// iteration and puts the same run back in the queue. expect is the iteration
// the caller checked the checkout and pull request against; if another was
// submitted since, those checks describe the wrong state and this refuses.
// Eligibility, exclusivity, the append and the requeue share one transaction,
// so two submitters cannot both be accepted. The run keeps its ID, its totals,
// its creation and first-start times; the new submission time is what the
// queue orders it by.
func (d *DB) Revise(runID string, expect int, request string, spec []byte) (int, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var status, repo string
	var n int
	if err := tx.QueryRow(`SELECT status, COALESCE(repo, ''), iteration FROM runs WHERE run_id = ?`, runID).
		Scan(&status, &repo, &n); err != nil {
		return 0, err
	}
	if n != expect {
		return 0, fmt.Errorf("%w: iteration %d was submitted after this request was checked; inspect the run before retrying", ErrNotRevisable, n)
	}
	if err := Revisable(Row{Status: status, Iteration: n}); err != nil {
		return 0, err
	}
	var other string
	err = tx.QueryRow(
		`SELECT run_id FROM runs WHERE repo = ? AND exclusive = 1 AND run_id != ?
		   AND status IN ('queued', 'starting', 'running') LIMIT 1`, repo, runID).Scan(&other)
	switch {
	case err == nil:
		return 0, fmt.Errorf("%w: %s", ErrDuplicateBuild, other)
	case !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}

	at, next := nowUTC(), n+1
	if _, err := tx.Exec(
		`INSERT INTO iterations (run_id, iteration, request, spec, status, submitted_at)
		 VALUES (?, ?, ?, ?, 'queued', ?)`, runID, next, request, string(spec), at); err != nil {
		return 0, err
	}
	// ended_at, reason and cancel_at describe the iteration that just ended;
	// that is kept on its own row, and the run now follows the new one.
	res, err := tx.Exec(
		`UPDATE runs SET status = 'queued', iteration = ?, spec = ?, activity_at = ?,
		   reason = NULL, ended_at = NULL, cancel_at = NULL
		 WHERE run_id = ? AND iteration = ? AND status = 'ok'`, next, string(spec), at, runID, n)
	if err != nil {
		return 0, err
	}
	if err := claimed(res); err != nil {
		return 0, fmt.Errorf("%w: its state changed while the request was being recorded", ErrNotRevisable)
	}
	return next, tx.Commit()
}

// Revisable is the run-state half of revision eligibility: the latest
// iteration finished, and succeeded. A failed, cancelled or lost iteration
// ends the run's revisions, even when an earlier one succeeded. Submitters
// call it for an early answer; Revise calls it again inside its transaction.
func Revisable(row Row) error {
	switch row.Status {
	case StatusOK:
		return nil
	case StatusQueued, StatusStarting, StatusRunning:
		return fmt.Errorf("%w: iteration %d is still %s; wait for it or cancel it", ErrNotRevisable, row.Iteration, row.Status)
	}
	return fmt.Errorf("%w: its latest iteration ended %s, and only a run whose latest iteration succeeded takes another", ErrNotRevisable, row.Status)
}

// mirror copies the run's lifecycle onto its current iteration inside the
// caller's transaction. Every lifecycle write ends with it, so the run and the
// iteration it follows cannot disagree about how that iteration ended.
func mirror(tx *sql.Tx, runID string) error {
	_, err := tx.Exec(
		`UPDATE iterations SET status = r.status, reason = r.reason, ended_at = r.ended_at
		 FROM runs r WHERE r.run_id = ? AND iterations.run_id = r.run_id AND iterations.iteration = r.iteration`, runID)
	return err
}

// Queued returns waiting runs, oldest submission first: the manager dispatches
// in that order, and a revision queues by when it was submitted.
func (d *DB) Queued() ([]Row, error) {
	rows, err := d.sql.Query(runColumns + ` FROM runs WHERE status = 'queued' ORDER BY activity_at, run_id`)
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
		`INSERT INTO attempts (attempt_id, run_id, dispatched_at, log_path, iteration)
		 VALUES (?, ?, ?, ?, (SELECT iteration FROM runs WHERE run_id = ?))`,
		attemptID, runID, nowUTC(), logPath, runID); err != nil {
		return err
	}
	if err := mirror(tx, runID); err != nil {
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
	// The run keeps its first start; each iteration records its own.
	res, err = tx.Exec(
		`UPDATE runs SET status = 'running', started_at = COALESCE(started_at, ?)
		 WHERE run_id = ? AND attempt_id = ? AND status = 'starting'`,
		at, runID, attemptID)
	if err != nil {
		return Row{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Row{}, err
	} else if n == 0 {
		return Row{}, ErrClaimLost
	}
	if _, err := tx.Exec(
		`UPDATE iterations SET started_at = ?
		 WHERE run_id = ? AND iteration = (SELECT iteration FROM attempts WHERE attempt_id = ?)`,
		at, runID, attemptID); err != nil {
		return Row{}, err
	}
	if err := mirror(tx, runID); err != nil {
		return Row{}, err
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
		if err := mirror(tx, runID); err != nil {
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
	if err := mirror(tx, runID); err != nil {
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
	if err := mirror(tx, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetCommit records the commit execution actually read, which is not
// necessarily submission-time HEAD, on the run and as the iteration's base.
// Like every write below it, it lands only while attemptID owns the run.
func (d *DB) SetCommit(runID, attemptID, sha string) error {
	return d.guarded(runID, attemptID,
		`UPDATE runs SET commit_sha = ? WHERE run_id = ?`, []any{sha, runID},
		`UPDATE iterations SET base_commit = ?`, []any{sha})
}

// SetShipped moves a run's branch and commit to where its work now lives. A
// build starts from the branch it was submitted against and ends on one it
// created, so without this the run would name its base as its result. The base
// is not lost: it is the shipped commit's parent, and the pull request's base.
// A revision calls it only once its push is verified, so the run's commit is
// always the last one lathe knows reached the pull request.
func (d *DB) SetShipped(runID, attemptID, branch, sha string) error {
	return d.guarded(runID, attemptID,
		`UPDATE runs SET branch = ?, commit_sha = ? WHERE run_id = ?`, []any{branch, sha, runID},
		`UPDATE iterations SET commit_sha = ?`, []any{sha})
}

// SetEvidence records what publication established for the current
// iteration: the commit it made, and the branch's remote head when last
// verified. Neither moves the run's shipped commit.
func (d *DB) SetEvidence(runID, attemptID, commit, remote string) error {
	return d.guarded(runID, attemptID, "", nil,
		`UPDATE iterations SET commit_sha = ?, remote_sha = ?`, []any{commit, nullIfEmpty(remote)})
}

// guarded runs an optional run update and an iteration update, each only while
// attemptID owns the run, in one transaction. iterSet is an UPDATE ... SET
// clause; the iteration it applies to is the attempt's own.
func (d *DB) guarded(runID, attemptID, runSQL string, runArgs []any, iterSet string, iterArgs []any) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if runSQL != "" {
		res, err := tx.Exec(runSQL+" AND "+owned, append(runArgs, runID, attemptID)...)
		if err != nil {
			return err
		}
		if err := claimed(res); err != nil {
			return err
		}
	}
	res, err := tx.Exec(iterSet+` WHERE run_id = ? AND iteration = (SELECT iteration FROM attempts WHERE attempt_id = ?) AND `+owned,
		append(iterArgs, runID, attemptID, runID, attemptID)...)
	if err != nil {
		return err
	}
	if err := claimed(res); err != nil {
		return err
	}
	return tx.Commit()
}

// Iteration is one submitted request against a run and what became of it.
// Spec is the configuration it was executed with, kept per iteration so the
// history says which configuration produced which result.
type Iteration struct {
	N         int    `json:"iteration"`
	Request   string `json:"request"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Submitted string `json:"submitted_at"`
	Started   string `json:"started_at"`
	Ended     string `json:"ended_at"`
	// Base is HEAD when it started. Commit is the commit it made, published
	// or not; Remote is the branch's remote head when publication was last
	// verified. Together they explain where its work is.
	Base   string `json:"base_commit"`
	Commit string `json:"commit"`
	Remote string `json:"remote"`
	Spec   []byte `json:"-"`
}

// Iterations is every iteration of a run, oldest first.
func (d *DB) Iterations(runID string) ([]Iteration, error) {
	rows, err := d.sql.Query(
		`SELECT iteration, COALESCE(request, ''), COALESCE(status, ''), COALESCE(reason, ''),
		        COALESCE(submitted_at, ''), COALESCE(started_at, ''), COALESCE(ended_at, ''),
		        COALESCE(base_commit, ''), COALESCE(commit_sha, ''), COALESCE(remote_sha, ''), COALESCE(spec, '')
		 FROM iterations WHERE run_id = ? ORDER BY iteration`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Iteration{}
	for rows.Next() {
		var it Iteration
		var spec string
		if err := rows.Scan(&it.N, &it.Request, &it.Status, &it.Reason, &it.Submitted, &it.Started,
			&it.Ended, &it.Base, &it.Commit, &it.Remote, &spec); err != nil {
			return nil, err
		}
		it.Spec = []byte(spec)
		out = append(out, it)
	}
	return out, rows.Err()
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
