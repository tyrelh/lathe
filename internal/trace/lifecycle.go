package trace

// The run lifecycle lives here: submission, dispatch, claim, heartbeat,
// cancellation, completion and reconciliation. Every one of them is a
// conditional update inside a transaction, because the whole point of the
// manager/worker split is that two processes can be wrong about who owns a run
// and only the database gets to settle it. Scheduling and workflow code call
// these; neither writes SQL.

import (
	"database/sql"
	"encoding/json"
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

// executing matches a run that is dispatched but not settled.
const executing = "status IN ('" + StatusStarting + "', '" + StatusRunning + "')"

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
}

// Submit records a queued run and returns its ID.
func (d *DB) Submit(r Request) (string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	id, at := NewRunID(r.Workflow), nowUTC()
	if _, err := tx.Exec(
		`INSERT INTO runs (run_id, workflow, repo, request, status, spec, branch, submitted_at, activity_at)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?, ?)`,
		id, r.Workflow, r.Repo, r.Request, string(r.Spec), r.Branch, at, at); err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO iterations (run_id, iteration, request, spec, status, submitted_at, kind)
		 VALUES (?, 0, ?, ?, 'queued', ?, ?)`, id, r.Request, string(r.Spec), at, KindSubmission); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// Revise accepts another request against a finished run: it appends an
// iteration and puts the same run back in the queue. expect is the iteration
// the caller checked the pull request against; if another was submitted
// since, those checks describe the wrong state and this refuses.
// Eligibility, the append and the requeue share one transaction: of concurrent
// calls with the same expect, at most one succeeds. The run keeps its ID, its totals,
// its creation and first-start times; the new submission time is what the
// queue orders it by.
func (d *DB) Revise(runID string, expect int, request string, spec []byte) (int, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var status string
	var n int
	if err := tx.QueryRow(`SELECT status, iteration FROM runs WHERE run_id = ?`, runID).
		Scan(&status, &n); err != nil {
		return 0, err
	}
	if n != expect {
		return 0, fmt.Errorf("%w: iteration %d was submitted after this request was checked; inspect the run before retrying", ErrNotRevisable, n)
	}
	if err := Revisable(Row{Status: status, Iteration: n}); err != nil {
		return 0, err
	}

	at, next := nowUTC(), n+1
	if _, err := tx.Exec(
		`INSERT INTO iterations (run_id, iteration, request, spec, status, submitted_at, kind)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?)`, runID, next, request, string(spec), at, KindRevision); err != nil {
		return 0, err
	}
	// ended_at, reason and cancel_at describe the iteration that just ended;
	// that is kept on its own row, and the run now follows the new one.
	res, err := tx.Exec(
		`UPDATE runs SET status = 'queued', iteration = ?, spec = ?, activity_at = ?,
		   reason = NULL, ended_at = NULL, cancel_at = NULL
		 WHERE run_id = ? AND iteration = ? AND status = '`+StatusOK+`'`, next, string(spec), at, runID, n)
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
	rows, err := d.sql.Query(runColumns + ` FROM runs WHERE ` + executing + ` ORDER BY submitted_at`)
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
	// Mode is how the attempt executes: ModeCheckout, ModeWorktree or
	// ModeReject. Attempts from before worktrees are ModeCheckout.
	Mode string
}

// Execution modes an attempt is dispatched with.
const (
	// ModeCheckout runs in the source checkout, which it coordinates for.
	ModeCheckout = "checkout"
	// ModeWorktree runs in a managed worktree of its own.
	ModeWorktree = "worktree"
	// ModeReject only records a refusal; it touches no checkout.
	ModeReject = "reject"
)

// NewAttemptID derives a dispatch identity from the run it belongs to, so a
// log directory sorts beside its run and a stray file names its owner.
func NewAttemptID(runID string) string { return runID + "_a" + randomHex(3) }

// NewClaimToken is what binds a worker's writes to its own claim.
func NewClaimToken() string { return randomHex(16) }

// Reserve is ReserveMode with ModeCheckout.
func (d *DB) Reserve(runID, attemptID, logPath string) error {
	return d.ReserveMode(runID, attemptID, logPath, ModeCheckout)
}

// ReserveMode records the intent to dispatch before anything is launched,
// with the attempt's execution mode. A manager that dies between this and the
// launch leaves a starting run with an unclaimed attempt, which is exactly what
// reconciliation looks for.
func (d *DB) ReserveMode(runID, attemptID, logPath, mode string) error {
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
	if err := claimed(res); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO attempts (attempt_id, run_id, dispatched_at, log_path, iteration, mode)
		 VALUES (?, ?, ?, ?, (SELECT iteration FROM runs WHERE run_id = ?), ?)`,
		attemptID, runID, nowUTC(), logPath, runID, mode); err != nil {
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
	if err := claimed(res); err != nil {
		return Row{}, err
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
	if err := claimed(res); err != nil {
		return Row{}, err
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

	row, err := getRow(tx, runID)
	if err != nil {
		return Row{}, err
	}
	return row, tx.Commit()
}

// Beat extends the lease and reports whether a cancellation is waiting. It
// returns ErrClaimLost if the attempt no longer holds its claim.
func (d *DB) Beat(attemptID, token string, lease time.Duration) (cancel bool, err error) {
	// Cancellation is polled here rather than in a second query because the
	// heartbeat already runs on its own clock, independent of model output.
	res, err := d.sql.Exec(
		`UPDATE attempts SET heartbeat_at = ?, lease_until = ?
		 WHERE attempt_id = ? AND claim_token = ? AND outcome IS NULL`,
		nowUTC(), until(lease), attemptID, token)
	if err != nil {
		return false, err
	}
	if err := claimed(res); err != nil {
		return false, err
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
		 WHERE run_id = ? AND attempt_id = ? AND `+executing+`
		   AND EXISTS (SELECT 1 FROM attempts WHERE attempt_id = ? AND claim_token = ?)`,
		status, reason, nowUTC(), runID, attemptID, attemptID, token)
	if err != nil {
		return err
	}
	if err := claimed(res); err != nil {
		return err
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
		 WHERE run_id = ? AND `+executing, reason, nowUTC(), runID); err != nil {
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
		`UPDATE runs SET commit_sha = ?`, []any{sha},
		`UPDATE iterations SET base_commit = ?`, []any{sha})
}

// SetShipped records the run's shipped branch and commit, and the current
// iteration's commit. It writes nothing and returns ErrClaimLost if attemptID
// no longer owns the run.
func (d *DB) SetShipped(runID, attemptID, branch, sha string) error {
	return d.guarded(runID, attemptID,
		`UPDATE runs SET branch = ?, commit_sha = ?`, []any{branch, sha},
		`UPDATE iterations SET commit_sha = ?`, []any{sha})
}

// SetEvidence records what publication established for the current
// iteration: the commit it made, and the branch's remote head when last
// verified. Neither moves the run's shipped commit.
func (d *DB) SetEvidence(runID, attemptID, commit, remote string) error {
	return d.guarded(runID, attemptID, "", nil,
		`UPDATE iterations SET commit_sha = ?, remote_sha = ?`, []any{commit, nullIfEmpty(remote)})
}

// guarded runs an optional run update and an iteration update in one
// transaction. runSet and iterSet are UPDATE ... SET clauses; guarded scopes
// the first to runID and the second to the attempt's iteration, both only
// while attemptID owns the run.
func (d *DB) guarded(runID, attemptID, runSet string, runArgs []any, iterSet string, iterArgs []any) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if runSet != "" {
		res, err := tx.Exec(runSet+" WHERE run_id = ? AND "+owned,
			append(append([]any{}, runArgs...), runID, runID, attemptID)...)
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
	// Worktree is where a writing iteration executed, WorktreeState its
	// cleanup state and CleanupError why cleanup is pending. Patch and
	// RecoveryRef are where its result was preserved.
	Worktree      string `json:"worktree"`
	WorktreeState string `json:"worktree_state"`
	CleanupError  string `json:"cleanup_error"`
	Patch         string `json:"patch"`
	RecoveryRef   string `json:"recovery_ref"`
	Spec          []byte `json:"-"`
	// Kind is why the iteration exists (KindSubmission, KindRevision,
	// KindRecovery or KindRestart); Source is the iteration a recovery
	// continues, or -1. Resolved is the task an issue resolved to.
	Kind     string `json:"kind"`
	Source   int    `json:"source_iteration"`
	Guidance string `json:"guidance"`
	Resolved string `json:"resolved_request"`
	// Restored is whether a recovery finished restoring the work its
	// specification carries.
	Restored bool `json:"restored"`
	// Preservation is how cleanup preserved the worktree's work: one of the
	// Preserved constants, or "" when nothing recorded it.
	Preservation string `json:"preservation"`
}

// Iterations is every iteration of a run, oldest first.
func (d *DB) Iterations(runID string) ([]Iteration, error) {
	rows, err := d.sql.Query(
		`SELECT iteration, COALESCE(request, ''), COALESCE(status, ''), COALESCE(reason, ''),
		        COALESCE(submitted_at, ''), COALESCE(started_at, ''), COALESCE(ended_at, ''),
		        COALESCE(base_commit, ''), COALESCE(commit_sha, ''), COALESCE(remote_sha, ''),
		        COALESCE(worktree_path, ''), COALESCE(worktree_state, ''), COALESCE(worktree_error, ''),
		        COALESCE(patch_path, ''), COALESCE(recovery_ref, ''), COALESCE(spec, ''),
		        COALESCE(kind, CASE WHEN iteration = 0 THEN ? ELSE ? END),
		        COALESCE(source_iteration, -1), COALESCE(guidance, ''), COALESCE(resolved_request, ''), restored,
		        COALESCE(preservation, '')
		 FROM iterations WHERE run_id = ? ORDER BY iteration`, KindSubmission, KindRevision, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Iteration{}
	for rows.Next() {
		var it Iteration
		var spec string
		if err := rows.Scan(&it.N, &it.Request, &it.Status, &it.Reason, &it.Submitted, &it.Started,
			&it.Ended, &it.Base, &it.Commit, &it.Remote, &it.Worktree, &it.WorktreeState,
			&it.CleanupError, &it.Patch, &it.RecoveryRef, &spec,
			&it.Kind, &it.Source, &it.Guidance, &it.Resolved, &it.Restored, &it.Preservation); err != nil {
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
		        worker_host, handle, log_path, heartbeat_at, lease_until, outcome, mode
		 FROM attempts WHERE attempt_id = ?`, attemptID).
		Scan(&a.ID, &a.RunID, &a.Dispatched, &claimed, &token, &a.PID,
			&host, &handle, &logPath, &beat, &lease, &outcome, &a.Mode)
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

// Worktree cleanup states. creating and active belong to a worker; cleaning
// and pending are cleanup started or failed, which the manager retries;
// removed is done.
const (
	WorktreeCreating = "creating"
	WorktreeActive   = "active"
	WorktreeCleaning = "cleaning"
	WorktreePending  = "pending"
	WorktreeRemoved  = "removed"
)

// Worktree is one iteration's managed worktree, with what recovery needs to
// preserve and remove it.
type Worktree struct {
	RunID     string
	Iteration int
	Repo      string // the source checkout
	Path      string
	Base      string
	RunStatus string
	// Current is whether this is the run's latest iteration.
	Current bool
	Spec    []byte
	// State is the cleanup state recorded when it was read.
	State string
}

// SetWorktree records the attempt's iteration worktree, the base commit it
// starts from and its state, in one write. It lands only while attemptID owns
// the run.
func (d *DB) SetWorktree(runID, attemptID, path, base, state string) error {
	return d.guarded(runID, attemptID, "", nil,
		`UPDATE iterations SET worktree_path = ?, base_commit = ?, worktree_state = ?, worktree_error = NULL`,
		[]any{path, nullIfEmpty(base), state})
}

// SetWorktreeState records an iteration's worktree cleanup state and, for
// pending, why. It is not bound to a claim.
func (d *DB) SetWorktreeState(runID string, iteration int, state, errMsg string) error {
	_, err := d.sql.Exec(
		`UPDATE iterations SET worktree_state = ?, worktree_error = ? WHERE run_id = ? AND iteration = ?`,
		state, nullIfEmpty(errMsg), runID, iteration)
	return err
}

// SetPatch records the iteration's latest patch export; empty records that
// the export found nothing, superseding any earlier one.
func (d *DB) SetPatch(runID string, iteration int, patch string) error {
	_, err := d.sql.Exec(`UPDATE iterations SET patch_path = ? WHERE run_id = ? AND iteration = ?`,
		nullIfEmpty(patch), runID, iteration)
	return err
}

// SetRecoveryRef records the ref an iteration's commits are kept under.
func (d *DB) SetRecoveryRef(runID string, iteration int, ref string) error {
	_, err := d.sql.Exec(`UPDATE iterations SET recovery_ref = ? WHERE run_id = ? AND iteration = ?`,
		nullIfEmpty(ref), runID, iteration)
	return err
}

// Worktrees returns every recorded worktree not yet removed, oldest first.
func (d *DB) Worktrees() ([]Worktree, error) {
	rows, err := d.sql.Query(
		`SELECT i.run_id, i.iteration, COALESCE(r.repo, ''), i.worktree_path, COALESCE(i.base_commit, ''),
		        COALESCE(r.status, ''), i.iteration = r.iteration, COALESCE(i.spec, ''), COALESCE(i.worktree_state, '')
		 FROM iterations i JOIN runs r ON r.run_id = i.run_id
		 WHERE COALESCE(i.worktree_path, '') != '' AND COALESCE(i.worktree_state, '') != 'removed'
		 ORDER BY i.submitted_at, i.run_id, i.iteration`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Worktree
	for rows.Next() {
		var w Worktree
		var spec string
		if err := rows.Scan(&w.RunID, &w.Iteration, &w.Repo, &w.Path, &w.Base,
			&w.RunStatus, &w.Current, &spec, &w.State); err != nil {
			return nil, err
		}
		w.Spec = []byte(spec)
		out = append(out, w)
	}
	return out, rows.Err()
}

// Launch states.
const (
	LaunchIntent     = "intent"
	LaunchRegistered = "registered"
	LaunchDrained    = "drained"
	LaunchCancelled  = "cancelled"
)

// openLaunch matches a launch not yet drained or cancelled.
const openLaunch = "state IN ('" + LaunchIntent + "', '" + LaunchRegistered + "')"

// ErrLaunchRevoked is a registration the run no longer admits.
var ErrLaunchRevoked = errors.New("the launch is no longer admitted")

// Launch is one supervised subprocess as recovery reads it.
type Launch struct {
	ID        string
	RunID     string
	AttemptID string
	State     string
	Host      string
	PID       int
	PGID      int
	Endpoint  string
	Token     string
}

// RecordLaunchIntent records that attemptID is about to start a supervised
// subprocess. It lands only while the attempt owns the run.
func (d *DB) RecordLaunchIntent(runID, attemptID, launchID string, argv []string) error {
	b, _ := json.Marshal(argv)
	res, err := d.sql.Exec(
		`INSERT INTO launches (launch_id, run_id, attempt_id, state, argv, intent_at)
		 SELECT ?, ?, ?, 'intent', ?, ? WHERE `+owned,
		launchID, runID, attemptID, string(b), nowUTC(), runID, attemptID)
	if err != nil {
		return err
	}
	return claimed(res)
}

// RegisterLaunch records a started, not yet admitted, supervisor. It lands
// only for an intent whose attempt still owns its run, so a run settled or
// an intent cancelled meanwhile is never admitted: that is ErrLaunchRevoked.
func (d *DB) RegisterLaunch(launchID, host string, pid, pgid int, endpoint, token string) error {
	res, err := d.sql.Exec(
		`UPDATE launches SET state = 'registered', host = ?, pid = ?, pgid = ?, endpoint = ?, token = ?, registered_at = ?
		 WHERE launch_id = ? AND state = 'intent'
		   AND EXISTS (SELECT 1 FROM runs r WHERE r.run_id = launches.run_id
		     AND r.attempt_id = launches.attempt_id AND r.`+executing+`)`,
		host, pid, pgid, endpoint, token, nowUTC(), launchID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrLaunchRevoked
	}
	return nil
}

// DrainLaunch records that a launch's process group has stopped.
func (d *DB) DrainLaunch(launchID string) error {
	_, err := d.sql.Exec(
		`UPDATE launches SET state = 'drained', drained_at = ? WHERE launch_id = ? AND `+openLaunch,
		nowUTC(), launchID)
	return err
}

// CancelLaunch revokes an intent that was never registered, and reports
// whether it did. A registered launch is left for its group to be stopped.
func (d *DB) CancelLaunch(launchID string) (bool, error) {
	res, err := d.sql.Exec(
		`UPDATE launches SET state = 'cancelled', drained_at = ? WHERE launch_id = ? AND state = 'intent'`,
		nowUTC(), launchID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// OpenLaunches is every launch of runID not yet drained or cancelled.
func (d *DB) OpenLaunches(runID string) ([]Launch, error) {
	rows, err := d.sql.Query(
		`SELECT launch_id, run_id, attempt_id, state, COALESCE(host, ''), pid, pgid,
		        COALESCE(endpoint, ''), COALESCE(token, '')
		 FROM launches WHERE run_id = ? AND `+openLaunch+` ORDER BY intent_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Launch
	for rows.Next() {
		var l Launch
		if err := rows.Scan(&l.ID, &l.RunID, &l.AttemptID, &l.State, &l.Host, &l.PID, &l.PGID,
			&l.Endpoint, &l.Token); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func until(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339) }
