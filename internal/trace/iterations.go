package trace

// Iteration kinds. Rows written before kind was recorded have NULL kind and
// are read as a submission at iteration 0 and a revision otherwise.
const (
	KindSubmission = "submission"
	KindRevision   = "revision"
	KindRecovery   = "recovery"
	KindRestart    = "restart"
)

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
