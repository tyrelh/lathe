package trace

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
)

// Iteration kinds. Rows written before kind was recorded have NULL kind and
// are read as a submission at iteration 0 and a revision otherwise.
const (
	KindSubmission = "submission"
	KindRevision   = "revision"
	KindRecovery   = "recovery"
	KindRestart    = "restart"
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

	var status string
	var n int
	if err := tx.QueryRow(`SELECT status, iteration FROM runs WHERE run_id = ?`, runID).Scan(&status, &n); err != nil {
		return 0, err
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
	res, err := tx.Exec(
		`UPDATE runs SET status = 'queued', iteration = ?, spec = ?, activity_at = ?,
		   reason = NULL, ended_at = NULL, cancel_at = NULL
		 WHERE run_id = ? AND iteration = ? AND `+recoverableStatus,
		next, string(rec.Spec), at, runID, n)
	if err != nil {
		return 0, err
	}
	if err := claimed(res); err != nil {
		return 0, fmt.Errorf("%w: its state changed while the request was being recorded", ErrNotRecoverable)
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

// stopped is Stopped inside tx. Terminal status and expired leases are not
// evidence: every supervised launch must be drained or revoked, every
// attempt settled, the run's reservation released and the iteration's
// worktree preserved and removed.
func stopped(tx *sql.Tx, runID string, n int) error {
	var open, unsettled, held int
	if err := tx.QueryRow(`SELECT count(*) FROM launches WHERE run_id = ? AND `+openLaunch, runID).Scan(&open); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT count(*) FROM attempts WHERE run_id = ? AND outcome IS NULL`, runID).Scan(&unsettled); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT count(*) FROM reservations WHERE run_id = ?`, runID).Scan(&held); err != nil {
		return err
	}
	var path, state sql.NullString
	if err := tx.QueryRow(`SELECT worktree_path, worktree_state FROM iterations WHERE run_id = ? AND iteration = ?`, runID, n).
		Scan(&path, &state); err != nil {
		return err
	}
	switch {
	case open > 0:
		return fmt.Errorf("%w: %d supervised process(es) of the run are not confirmed stopped; let the manager settle them first", ErrNotRecoverable, open)
	case unsettled > 0:
		return fmt.Errorf("%w: an attempt of the run has no recorded outcome, so its worker is not known to have stopped", ErrNotRecoverable)
	case held > 0:
		return fmt.Errorf("%w: the run still holds a checkout reservation; its worker is not confirmed stopped", ErrNotRecoverable)
	case path.String != "" && state.String != WorktreeRemoved:
		return fmt.Errorf("%w: iteration %d's worktree is %s, not yet preserved and removed; let the manager finish its cleanup first", ErrNotRecoverable, n, cmp.Or(state.String, "in an unknown state"))
	}
	return nil
}

// SourceStopped repeats the stopped-writer checks for a recovery's source
// iteration under the recovering attempt's claim, ignoring that attempt's
// own launches and its reservation of path.
func (d *DB) SourceStopped(runID string, source int, attemptID, path string) error {
	var open, unsettled, held int
	if err := d.sql.QueryRow(`SELECT count(*) FROM launches WHERE run_id = ? AND attempt_id != ? AND `+openLaunch, runID, attemptID).Scan(&open); err != nil {
		return err
	}
	if err := d.sql.QueryRow(`SELECT count(*) FROM attempts WHERE run_id = ? AND attempt_id != ? AND outcome IS NULL`, runID, attemptID).Scan(&unsettled); err != nil {
		return err
	}
	if err := d.sql.QueryRow(`SELECT count(*) FROM reservations WHERE run_id = ? AND path != ?`, runID, path).Scan(&held); err != nil {
		return err
	}
	var wt, state sql.NullString
	if err := d.sql.QueryRow(`SELECT worktree_path, worktree_state FROM iterations WHERE run_id = ? AND iteration = ?`, runID, source).
		Scan(&wt, &state); err != nil {
		return err
	}
	if open > 0 || unsettled > 0 || held > 0 || (wt.String != "" && state.String != WorktreeRemoved) {
		return fmt.Errorf("iteration %d's writers or cleanup are no longer confirmed settled; nothing was changed", source)
	}
	return nil
}

// How cleanup preserved an iteration's worktree. PreservedPatch saved its
// work as a patch; PreservedEmpty exported and found nothing; PreservedUnstarted
// means the worktree was never created, so nothing was written;
// PreservedSkipped means export was not wanted; PreservedMissing means the
// worktree was gone before its work could be exported.
const (
	PreservedPatch     = "patch"
	PreservedEmpty     = "empty"
	PreservedUnstarted = "unstarted"
	PreservedSkipped   = "skipped"
	PreservedMissing   = "missing"
)

// SetPreservation records how iteration's work was preserved. It is not
// bound to a claim: cleanup may run after the worker is gone.
func (d *DB) SetPreservation(runID string, iteration int, outcome string) error {
	_, err := d.sql.Exec(`UPDATE iterations SET preservation = ? WHERE run_id = ? AND iteration = ?`,
		outcome, runID, iteration)
	return err
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

