package trace

import "fmt"

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
