package worker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workspace"
)

// Settle preserves what an iteration's worktree holds and removes it. With
// keepPatch, everything beyond the base is saved as result.patch in the
// iteration's report directory; commits nothing else reaches are kept under
// the iteration's recovery ref either way. It records the cleanup state:
// removed on success, pending with the error otherwise, in which case the
// worktree is left in place. The worktree's writers must have stopped.
func Settle(db *trace.DB, dataRoot string, w trace.Worktree, keepPatch bool) error {
	if err := db.SetWorktreeState(w.RunID, w.Iteration, trace.WorktreeCleaning, ""); err != nil {
		return err
	}
	if err := settle(db, dataRoot, w, protectedPaths(w.Spec), keepPatch); err != nil {
		if serr := db.SetWorktreeState(w.RunID, w.Iteration, trace.WorktreePending, err.Error()); serr != nil {
			return errors.Join(err, serr)
		}
		return err
	}
	return db.SetWorktreeState(w.RunID, w.Iteration, trace.WorktreeRemoved, "")
}

func settle(db *trace.DB, dataRoot string, w trace.Worktree, protected []string, keepPatch bool) error {
	if want := workspace.WorktreePath(dataRoot, w.RunID, w.Iteration); w.Path != want {
		return fmt.Errorf("the recorded worktree %s is not where lathe creates %s's iteration %d; it was left alone", w.Path, w.RunID, w.Iteration)
	}
	fi, err := os.Lstat(w.Path)
	if err == nil && w.Base == "" {
		return fmt.Errorf("%s exists but its base commit was never recorded, so its work cannot be exported; it was left alone", w.Path)
	}
	switch {
	case err == nil && fi.IsDir():
		outcome := trace.PreservedSkipped
		if keepPatch {
			dest := filepath.Join(ReportDir(dataRoot, w.RunID, w.Iteration), run.PatchFile)
			wrote, err := workspace.SavePatch(w.Path, w.Base, dest, protected)
			if err != nil {
				return fmt.Errorf("saving the result patch: %w; the worktree is kept at %s", err, w.Path)
			}
			outcome = trace.PreservedPatch
			if !wrote {
				dest, outcome = "", trace.PreservedEmpty
			}
			if err := db.SetPatch(w.RunID, w.Iteration, dest); err != nil {
				return err
			}
		}
		// Recorded before the worktree is removed, so removal never stands
		// in for evidence of what it held.
		if err := db.SetPreservation(w.RunID, w.Iteration, outcome); err != nil {
			return err
		}
		ref, err := workspace.PreserveHead(w.Path, w.Base, workspace.RecoveryRef(w.RunID, w.Iteration))
		if err != nil {
			return fmt.Errorf("keeping the worktree's commits: %w; the worktree is kept at %s", err, w.Path)
		}
		if ref != "" {
			if err := db.SetRecoveryRef(w.RunID, w.Iteration, ref); err != nil {
				return err
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		// Nothing to export. A worktree still being created held no work; one
		// that existed and is gone lost whatever it held.
		outcome := trace.PreservedMissing
		if w.State == trace.WorktreeCreating {
			outcome = trace.PreservedUnstarted
		}
		if err := db.SetPreservation(w.RunID, w.Iteration, outcome); err != nil {
			return err
		}
	}
	return workspace.RemoveWorktree(dataRoot, w.Repo, w.RunID, w.Iteration)
}
