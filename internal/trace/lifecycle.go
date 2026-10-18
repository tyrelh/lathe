package trace

// The run lifecycle lives here: submission, revision, dispatch, claim,
// heartbeat, cancellation, completion and reconciliation. Every one of them is a
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
// TaskID attaches a build to an existing task, and a build without one gets a
// new task; other workflows ignore it.
type Request struct {
	TaskID   string
	Workflow string
	Repo     string
	Request  string
	Branch   string
	Spec     []byte
}

// Submit records a queued run and returns its ID.
func (d *DB) Submit(r Request) (string, error) {
	return d.submit(r, func(tx *sql.Tx) (Task, error) {
		task, err := getTask(tx, r.TaskID)
		if err != nil {
			return Task{}, fmt.Errorf("attach task: %w", err)
		}
		return task, nil
	})
}

// StartTask atomically submits a prepared build for an unchanged Backlog task.
func (d *DB) StartTask(r Request) (string, error) {
	if r.Workflow != "build" || r.TaskID == "" {
		return "", ErrTaskConflict
	}
	return d.submit(r, func(tx *sql.Tx) (Task, error) {
		task, err := backlogTask(tx, r.TaskID)
		if err != nil {
			return Task{}, err
		}
		issue := ""
		if task.IssueRef != nil {
			issue = *task.IssueRef
		}
		if task.Repo != r.Repo || task.Text != r.Request || issue != specIssue(r.Spec) {
			return Task{}, ErrTaskConflict
		}
		return task, nil
	})
}

// FreshTaskRun submits a build only for an unarchived Blocked task.
func (d *DB) FreshTaskRun(r Request, hasPR func(Row) bool) (string, error) {
	if r.Workflow != "build" || r.TaskID == "" {
		return "", ErrTaskConflict
	}
	return d.submit(r, func(tx *sql.Tx) (Task, error) {
		if _, err := tx.Exec(`UPDATE tasks SET id = id WHERE id = ?`, r.TaskID); err != nil {
			return Task{}, err
		}
		task, err := getTask(tx, r.TaskID)
		if err != nil {
			return Task{}, err
		}
		if task.ArchivedAt != nil {
			return Task{}, ErrTaskConflict
		}
		rows, err := tx.Query(runColumns+` FROM runs WHERE task_id = ? ORDER BY submitted_at DESC, run_id DESC LIMIT 1`, r.TaskID)
		if err != nil {
			return Task{}, err
		}
		runs, err := scanRows(rows)
		if err != nil {
			return Task{}, err
		}
		if len(runs) == 0 {
			return Task{}, ErrTaskConflict
		}
		run := runs[0]
		if TaskLane(run.Status, run.PRState, hasPR != nil && hasPR(run)) != LaneBlocked {
			return Task{}, ErrTaskConflict
		}
		return task, nil
	})
}

// submit records a queued run, loading an attached build task through load.
func (d *DB) submit(r Request, load func(*sql.Tx) (Task, error)) (string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	id, at := NewRunID(r.Workflow), nowUTC()
	var taskID any
	if r.Workflow == "build" {
		if r.TaskID != "" {
			task, err := load(tx)
			if err != nil {
				return "", err
			}
			if task.ArchivedAt != nil {
				return "", fmt.Errorf("task %s is archived", r.TaskID)
			}
			if task.Repo != r.Repo {
				return "", fmt.Errorf("task %s belongs to a different repository", r.TaskID)
			}
			if issue := specIssue(r.Spec); issue != "" && (task.IssueRef == nil || !issueRefsMatch(*task.IssueRef, issue)) {
				return "", fmt.Errorf("issue conflicts with task %s", r.TaskID)
			}
			// Keep attached submissions strictly ordered at whole-second precision.
			var previous sql.NullString
			if err := tx.QueryRow(`SELECT MAX(submitted_at) FROM runs WHERE task_id = ?`, r.TaskID).Scan(&previous); err != nil {
				return "", err
			}
			if previous.Valid && previous.String >= at {
				last, err := time.Parse(time.RFC3339, previous.String)
				if err != nil {
					return "", err
				}
				at = last.Add(time.Second).UTC().Format(time.RFC3339)
			}
			taskID = r.TaskID
		} else {
			created, err := insertTask(tx, r.Repo, r.Request, specIssue(r.Spec), at)
			if err != nil {
				return "", err
			}
			taskID = created
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO runs (run_id, workflow, repo, request, status, spec, branch, submitted_at, activity_at, task_id)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?)`,
		id, r.Workflow, r.Repo, r.Request, string(r.Spec), r.Branch, at, at, taskID); err != nil {
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

	status, n, err := runState(tx, runID, ErrNotRevisable)
	if err != nil {
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
	if err := requeue(tx, runID, n, next, spec, at, `status = '`+StatusOK+`'`,
		fmt.Errorf("%w: its state changed while the request was being recorded", ErrNotRevisable)); err != nil {
		return 0, err
	}
	return next, tx.Commit()
}

// runState returns a run's status and iteration, or refused when the run's task is archived.
func runState(tx *sql.Tx, runID string, refused error) (status string, n int, err error) {
	var archived bool
	if err := tx.QueryRow(`SELECT status, iteration,
		EXISTS (SELECT 1 FROM tasks WHERE tasks.id = runs.task_id AND archived_at IS NOT NULL)
		FROM runs WHERE run_id = ?`, runID).
		Scan(&status, &n, &archived); err != nil {
		return "", 0, err
	}
	if archived {
		return "", 0, fmt.Errorf("%w: its task is archived", refused)
	}
	return status, n, nil
}

// requeue points runID at its new iteration next and queues it, provided the
// run is still at iteration from and matches statusPred. It returns changed
// when the run no longer matches.
func requeue(tx *sql.Tx, runID string, from, next int, spec []byte, at, statusPred string, changed error) error {
	// ended_at, reason and cancel_at describe the iteration that just ended;
	// that is kept on its own row, and the run now follows the new one.
	res, err := tx.Exec(
		`UPDATE runs SET status = 'queued', iteration = ?, spec = ?, activity_at = ?,
		   reason = NULL, ended_at = NULL, cancel_at = NULL
		 WHERE run_id = ? AND iteration = ? AND `+statusPred, next, string(spec), at, runID, from)
	if err != nil {
		return err
	}
	if err := claimed(res); err != nil {
		return changed
	}
	return nil
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
