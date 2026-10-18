package trace

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotRecoverable is a recovery the run's recorded state refuses. The
// wrapped message says why.
var ErrNotRecoverable = errors.New("this run cannot be resumed")

// recoverableStatus matches the states Recoverable accepts; the two must agree.
const recoverableStatus = "status IN ('" + StatusFail + "', '" + StatusCancelled + "', '" + StatusLost + "')"

// Recoverable is the run-state half of recovery eligibility: the latest
// iteration ended failed, cancelled or lost. It must agree with
// recoverableStatus.
func Recoverable(row Row) error {
	switch row.Status {
	case StatusFail, StatusCancelled, StatusLost:
		return nil
	case StatusQueued, StatusStarting, StatusRunning:
		return fmt.Errorf("%w: iteration %d is still %s; wait for it or cancel it", ErrNotRecoverable, row.Iteration, row.Status)
	case StatusOK:
		return fmt.Errorf("%w: iteration %d succeeded; ask for further changes with lathe revise", ErrNotRecoverable, row.Iteration)
	}
	return fmt.Errorf("%w: its latest iteration is %s", ErrNotRecoverable, row.Status)
}

// Recovery is what a recovery submission appends.
type Recovery struct {
	Source   int    // the iteration inspected and continued
	Kind     string // KindRecovery or KindRestart
	Resolved string // the run's resolved task
	Guidance string
	Spec     []byte
}

// Recover appends a recovery iteration and requeues the run. Eligibility,
// the stopped-writer evidence and the append share one transaction, so two
// submitters cannot both be accepted and a refusal appends nothing. The run
// keeps its ID, totals and earlier iterations.
func (d *DB) Recover(runID string, rec Recovery) (int, error) {
	if rec.Kind != KindRecovery && rec.Kind != KindRestart {
		return 0, fmt.Errorf("unknown recovery kind %q", rec.Kind)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	status, n, err := runState(tx, runID, ErrNotRecoverable)
	if err != nil {
		return 0, err
	}
	var superseded bool
	if err := tx.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM runs r WHERE r.run_id = ? AND r.task_id IS NOT NULL
		AND r.run_id != (SELECT run_id FROM runs WHERE task_id = r.task_id
		ORDER BY submitted_at DESC, run_id DESC LIMIT 1)
	)`, runID).Scan(&superseded); err != nil {
		return 0, err
	}
	if superseded {
		return 0, fmt.Errorf("%w: its task has a newer run; inspect the task before retrying", ErrNotRecoverable)
	}
	if n != rec.Source {
		return 0, fmt.Errorf("%w: iteration %d was submitted after iteration %d was inspected; inspect the run before retrying", ErrNotRecoverable, n, rec.Source)
	}
	if err := Recoverable(Row{Status: status, Iteration: n}); err != nil {
		return 0, err
	}
	if err := stopped(tx, runID, n); err != nil {
		return 0, err
	}

	at, next := nowUTC(), n+1
	if _, err := tx.Exec(
		`INSERT INTO iterations (run_id, iteration, request, spec, status, submitted_at, kind, source_iteration, guidance, resolved_request)
		 VALUES (?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?)`,
		runID, next, rec.Guidance, string(rec.Spec), at, rec.Kind, rec.Source, rec.Guidance, nullIfEmpty(rec.Resolved)); err != nil {
		return 0, err
	}
	if err := requeue(tx, runID, n, next, rec.Spec, at, recoverableStatus,
		fmt.Errorf("%w: its state changed while the request was being recorded", ErrNotRecoverable)); err != nil {
		return 0, err
	}
	return next, tx.Commit()
}

// Stopped reports why runID's iteration n may still have a writer or
// unsettled cleanup, or nil when the record shows none.
func (d *DB) Stopped(runID string, n int) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return stopped(tx, runID, n)
}

// writerEvidence is what the record shows of an iteration's writers and
// cleanup.
type writerEvidence struct {
	open, unsettled, held int
	path, state           sql.NullString
}

// worktreeUnsettled reports whether the iteration's worktree exists and is
// not yet preserved and removed.
func (e writerEvidence) worktreeUnsettled() bool {
	return e.path.String != "" && e.state.String != WorktreeRemoved
}

// collectEvidence reads runID's open launches, unsettled attempts and
// reservations, and iteration n's worktree. A nonempty exceptAttempt ignores
// that attempt's launches and attempt row; a nonempty exceptPath ignores
// that reservation. A missing iteration is sql.ErrNoRows.
func collectEvidence(q queryer, runID string, n int, exceptAttempt, exceptPath string) (writerEvidence, error) {
	var e writerEvidence
	launches, attempts, args := `SELECT count(*) FROM launches WHERE run_id = ?`, `SELECT count(*) FROM attempts WHERE run_id = ?`, []any{runID}
	if exceptAttempt != "" {
		launches += ` AND attempt_id != ?`
		attempts += ` AND attempt_id != ?`
		args = append(args, exceptAttempt)
	}
	if err := q.QueryRow(launches+` AND `+openLaunch, args...).Scan(&e.open); err != nil {
		return e, err
	}
	if err := q.QueryRow(attempts+` AND outcome IS NULL`, args...).Scan(&e.unsettled); err != nil {
		return e, err
	}
	reservations, rargs := `SELECT count(*) FROM reservations WHERE run_id = ?`, []any{runID}
	if exceptPath != "" {
		reservations += ` AND path != ?`
		rargs = append(rargs, exceptPath)
	}
	if err := q.QueryRow(reservations, rargs...).Scan(&e.held); err != nil {
		return e, err
	}
	err := q.QueryRow(`SELECT worktree_path, worktree_state FROM iterations WHERE run_id = ? AND iteration = ?`, runID, n).
		Scan(&e.path, &e.state)
	return e, err
}

// stopped is Stopped inside tx. Terminal status and expired leases are not
// evidence: every supervised launch must be drained or revoked, every
// attempt settled, the run's reservation released and the iteration's
// worktree preserved and removed.
func stopped(tx *sql.Tx, runID string, n int) error {
	e, err := collectEvidence(tx, runID, n, "", "")
	if err != nil {
		return err
	}
	switch {
	case e.open > 0:
		return fmt.Errorf("%w: %d supervised process(es) of the run are not confirmed stopped; let the manager settle them first", ErrNotRecoverable, e.open)
	case e.unsettled > 0:
		return fmt.Errorf("%w: an attempt of the run has no recorded outcome, so its worker is not known to have stopped", ErrNotRecoverable)
	case e.held > 0:
		return fmt.Errorf("%w: the run still holds a checkout reservation; its worker is not confirmed stopped", ErrNotRecoverable)
	case e.worktreeUnsettled():
		return fmt.Errorf("%w: iteration %d's worktree is %s, not yet preserved and removed; let the manager finish its cleanup first", ErrNotRecoverable, n, cmp.Or(e.state.String, "in an unknown state"))
	}
	return nil
}

// SourceStopped repeats the stopped-writer checks for a recovery's source
// iteration under the recovering attempt's claim, ignoring that attempt's
// own launches and its reservation of path.
func (d *DB) SourceStopped(runID string, source int, attemptID, path string) error {
	e, err := collectEvidence(d.sql, runID, source, attemptID, path)
	if err != nil {
		return err
	}
	if e.open > 0 || e.unsettled > 0 || e.held > 0 || e.worktreeUnsettled() {
		return fmt.Errorf("iteration %d's writers or cleanup are no longer confirmed settled; nothing was changed", source)
	}
	return nil
}

// SetRestored records that the attempt's recovery finished restoring the
// work its specification carries. It lands only while attemptID owns the run.
func (d *DB) SetRestored(runID, attemptID string) error {
	return d.guarded(runID, attemptID, "", nil, `UPDATE iterations SET restored = 1`, nil)
}

// SetResolved records the task the attempt's iteration resolved to, such as
// an issue body, the first time it is available. It lands only while
// attemptID owns the run.
func (d *DB) SetResolved(runID, attemptID, request string) error {
	return d.guarded(runID, attemptID, "", nil,
		`UPDATE iterations SET resolved_request = ?`, []any{request})
}
