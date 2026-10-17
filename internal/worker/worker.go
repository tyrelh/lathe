// Package worker executes one run. It is a separate process from the
// submitter and from the manager, which is where durability comes from: the
// submitter can be interrupted, the manager can be restarted, and the run
// continues because the process doing the work is neither of them.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/process"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workflow"
	"github.com/tyrelh/lathe/internal/workspace"
)

// Timings are chosen together: the lease must outlast a heartbeat outage long
// enough for a worker to notice, stop its subprocesses and exit before the
// manager could consider its lease expired. They are variables only so a test
// can compress them; nothing in production writes to them.
var (
	// Heartbeat is how often a worker renews its lease.
	Heartbeat = 5 * time.Second
	// Lease is how long a renewal is good for.
	Lease = 45 * time.Second
	// dbGrace is how long a worker keeps retrying a database it cannot reach
	// before it gives up and shuts execution down. It is well inside Lease so
	// the shutdown finishes before the lease it was granted runs out.
	dbGrace = 15 * time.Second
)

// Execute claims the attempt, runs the workflow and records the outcome. It
// returns the process exit code. Every early return is a recorded outcome:
// a worker that gives up without writing one leaves a run that only the
// manager's reconciliation can settle.
func Execute(dataRoot, runID, attemptID string, out io.Writer) int {
	db, err := trace.Open(dataRoot)
	if err != nil {
		fmt.Fprintln(out, "lathe worker:", err)
		return 1
	}
	defer db.Close()

	host, _ := os.Hostname()
	token := trace.NewClaimToken()
	row, err := db.Claim(attemptID, token, host, os.Getpid(), Lease)
	if err != nil {
		// Losing the claim is the duplicate-launch case, and it must happen
		// before any work: whoever holds the claim owns the run.
		fmt.Fprintln(out, "lathe worker:", err)
		return 1
	}

	code, err := execute(db, dataRoot, row, attemptID, token, out)
	if err != nil {
		fmt.Fprintln(out, "lathe worker:", err)
		if cerr := db.Complete(row.ID, attemptID, token, trace.StatusFail, err.Error()); cerr != nil {
			fmt.Fprintln(out, "lathe worker: recording the failure:", cerr)
		}
		return 1
	}
	return code
}

func execute(db *trace.DB, dataRoot string, row trace.Row, attemptID, token string, out io.Writer) (int, error) {
	var spec Spec
	if err := unmarshalSpec(row.Spec, &spec); err != nil {
		return 0, err
	}
	// Decided before any reservation, Git or agent: a refused specification
	// changes nothing.
	mode, err := spec.mode(row.ID)
	if err != nil {
		return 0, err
	}
	if spec.Roster.Version != config.SnapshotVersion {
		return 0, fmt.Errorf("run %s was recorded by an incompatible lathe (roster v%d)", row.ID, spec.Roster.Version)
	}
	if err := spec.Workspace.Check(); err != nil {
		return 0, err
	}
	if _, ok := workflow.Graphs[spec.Workflow]; !ok {
		return 0, fmt.Errorf("unknown workflow %q", spec.Workflow)
	}

	// Cancellation and the heartbeat start before setup, so creating a
	// worktree or fetching a branch is as stoppable as an agent turn.
	ctx, stop := context.WithCancel(context.Background())
	beats := beat(ctx, db, attemptID, token, stop, out)
	defer func() {
		stop()
		<-beats
	}()
	// A signal is a cancellation, not a second shutdown route: the worker
	// stops its children, keeps what it has produced, and records cancelled.
	signals(ctx, stop)

	var code int
	var status, reason string
	if mode == trace.ModeWorktree {
		code, status, reason, err = inWorktree(ctx, db, dataRoot, row, attemptID, spec, out)
	} else {
		code, status, reason, err = inCheckout(ctx, db, dataRoot, row, attemptID, spec, out)
	}
	if err != nil {
		return 0, err
	}
	stop()
	<-beats

	if err := db.Complete(row.ID, attemptID, token, status, reason); err != nil {
		// A terminal state that is already recorded is not an error here: the
		// race with cancellation is settled by whichever committed first.
		if !errors.Is(err, trace.ErrClaimLost) {
			return code, err
		}
	}
	return code, nil
}

// inCheckout runs a read-only workflow in the source checkout, which it
// reserves and locks for the length of the run.
func inCheckout(ctx context.Context, db *trace.DB, dataRoot string, row trace.Row, attemptID string, spec Spec, out io.Writer) (int, string, string, error) {
	// The reservation is the database's record of who owns the checkout; the
	// lock is what actually keeps a second lathe process out of it. Both,
	// because either alone is a lie: the row survives a crash and the lock
	// does not.
	if err := db.ReserveCheckout(spec.Workspace.Path, spec.Workspace.Host, row.ID); err != nil {
		return 0, "", "", err
	}
	defer release(db, row.ID, out)
	lock, err := workspace.Acquire(dataRoot, spec.Workspace.Path)
	if err != nil {
		return 0, "", "", err
	}
	defer lock.Release()

	if err := revalidate(spec, row); err != nil {
		return 0, "", "", err
	}
	// A checkout with no commits yet has none to record, which is not a
	// reason to refuse to read it.
	commit, _ := workspace.Head(spec.Workspace.Path)
	if err := db.SetCommit(row.ID, attemptID, commit); err != nil {
		return 0, "", "", err
	}
	var recovery *run.Recovery
	if spec.Recovery != nil {
		if recovery, err = recoveryOf(db, dataRoot, row, spec.Recovery); err != nil {
			return 0, "", "", err
		}
	}
	r, err := open(ctx, db, dataRoot, row, attemptID, spec, spec.Workspace.Path, "", nil, recovery, nil, out)
	if err != nil {
		return 0, "", "", err
	}
	code := workflow.Graphs[spec.Workflow](r)
	code, status, reason := outcome(db, row.ID, r, code)
	return code, status, reason, nil
}

// inWorktree runs a writing workflow in a worktree of its own at the
// specification's base commit. Every subprocess it starts is supervised. The
// worktree's result is preserved and the worktree removed before it returns,
// after every supervised writer has stopped.
func inWorktree(ctx context.Context, db *trace.DB, dataRoot string, row trace.Row, attemptID string, spec Spec, out io.Writer) (int, string, string, error) {
	path := workspace.WorktreePath(dataRoot, row.ID, row.Iteration)
	if err := db.ReserveCheckout(path, spec.Workspace.Host, row.ID); err != nil {
		return 0, "", "", err
	}
	defer release(db, row.ID, out)
	lock, err := workspace.Acquire(dataRoot, path)
	if err != nil {
		return 0, "", "", err
	}
	defer lock.Release()

	runner, err := supervisor(db, row.ID, attemptID)
	if err != nil {
		return 0, "", "", err
	}
	restore := process.Install(runner)
	defer restore()

	base := spec.Base
	if spec.Recovery != nil {
		if err := db.SourceStopped(row.ID, spec.Recovery.Source, attemptID, path); err != nil {
			return 0, "", "", err
		}
	}
	if spec.Recovery != nil && spec.Recovery.Publication != nil {
		// The queued recovery's view of origin and the pull request, again.
		pub := spec.Recovery.Publication
		if err := recheck(spec.Workspace.Path, pub); err != nil {
			return 0, "", "", err
		}
		// A revision delivered despite a lost acknowledgement: the run's
		// shipped commit catches up with what origin verifiably holds.
		if spec.Workflow == workflow.NameRevise && row.Commit != pub.Head {
			if err := db.SetShipped(row.ID, attemptID, pub.Branch, pub.Head); err != nil {
				return 0, "", "", err
			}
			row.Commit = pub.Head
		}
	}
	var revision *run.Revision
	if spec.Workflow == workflow.NameRevise {
		// Rechecked under the claim, against origin: the pull request had
		// people around it the whole time the request waited.
		if revision, err = revisionOf(db, dataRoot, row, spec.Workspace.Path); err != nil {
			return 0, "", "", err
		}
		// Saved work applies only at the head it was made on; a moved head
		// is refused rather than rebased onto.
		if spec.Recovery != nil && spec.Base != "" && spec.Base != revision.Head {
			return 0, "", "", fmt.Errorf("the recovery was submitted at %s, but the pull request's head is now %s; nothing was changed",
				workspace.Short(spec.Base), workspace.Short(revision.Head))
		}
		base = revision.Head
	}
	var recovery *run.Recovery
	if spec.Recovery != nil {
		if recovery, err = recoveryOf(db, dataRoot, row, spec.Recovery); err != nil {
			return 0, "", "", err
		}
	}
	if base == "" {
		return 0, "", "", fmt.Errorf("run %s records no commit to start from; nothing was changed", row.ID)
	}

	// Ownership and base in one write, before the worktree exists: a crash
	// anywhere after this leaves cleanup enough to export what is there.
	if err := db.SetWorktree(row.ID, attemptID, path, base, trace.WorktreeCreating); err != nil {
		return 0, "", "", err
	}
	wt := trace.Worktree{RunID: row.ID, Iteration: row.Iteration, Repo: spec.Workspace.Path, Path: path, Base: base, Spec: row.Spec,
		State: trace.WorktreeCreating}
	if _, err := workspace.CreateWorktree(dataRoot, spec.Workspace.Path, row.ID, row.Iteration, base); err != nil {
		return 0, "", "", errors.Join(err, Settle(db, dataRoot, wt, true))
	}
	if err := db.SetWorktree(row.ID, attemptID, path, base, trace.WorktreeActive); err != nil {
		return 0, "", "", errors.Join(err, Settle(db, dataRoot, wt, true))
	}
	wt.State = trace.WorktreeActive
	if err := db.SetCommit(row.ID, attemptID, base); err != nil {
		return 0, "", "", errors.Join(err, Settle(db, dataRoot, wt, true))
	}
	// Restored into the fresh worktree, which Restore verifies clean and at
	// base, before any agent runs.
	if recovery != nil && spec.Recovery.Patch != "" {
		restored, err := workspace.Restore(path, base, spec.Recovery.Patch, spec.Recovery.Digest, spec.Roster.Protected)
		if err != nil {
			return 0, "", "", errors.Join(fmt.Errorf("restoring iteration %d's work: %w%s",
				spec.Recovery.Source, err, restartHint(row.ID)), Settle(db, dataRoot, wt, false))
		}
		recovery.Restored = restored
		// Until this lands, a later recovery restores the same evidence again.
		if err := db.SetRestored(row.ID, attemptID); err != nil {
			return 0, "", "", errors.Join(err, Settle(db, dataRoot, wt, true))
		}
	}
	r, err := open(ctx, db, dataRoot, row, attemptID, spec, path, base, revision, recovery, runner, out)
	if err != nil {
		return 0, "", "", errors.Join(err, Settle(db, dataRoot, wt, true))
	}
	if recovery != nil {
		r.Retain(recovery.Restored)
	}
	// Every supervised writer has drained by the time the graph returns.
	code := workflow.Graphs[spec.Workflow](r)
	code, status, reason := outcome(db, row.ID, r, code)

	keep := spec.Workflow == workflow.NameImplement || status != trace.StatusOK
	if err := Settle(db, dataRoot, wt, keep); err != nil {
		// Not a success: the result is not preserved or the worktree is not
		// gone. The worktree stays, recorded pending, and the manager retries.
		fmt.Fprintln(out, "lathe worker: cleaning up the worktree:", err)
		if status == trace.StatusOK {
			status, code = trace.StatusFail, 1
		}
		reason = join(reason, "worktree cleanup pending ("+path+"): "+err.Error())
	}
	if its, err := db.Iterations(row.ID); err == nil && row.Iteration < len(its) {
		PrintResult(out, its[row.Iteration], status == trace.StatusOK)
	}
	return code, status, reason, nil
}

func open(ctx context.Context, db *trace.DB, dataRoot string, row trace.Row, attemptID string, spec Spec, repo, base string, revision *run.Revision, recovery *run.Recovery, runner *process.Runner, out io.Writer) (*run.Run, error) {
	return run.Open(run.Options{
		ID:       row.ID,
		Workflow: spec.Workflow,
		Request:  spec.Request,
		Issue:    spec.Issue,
		Repo:     repo,
		Dir:      ReportDir(dataRoot, row.ID, row.Iteration),
		Work:     AttemptDir(dataRoot, row.ID, attemptID),
		Snapshot: spec.Roster,
		DB:       db,
		Ctx:      ctx,
		Out:      out,

		Iteration: row.Iteration,
		Attempt:   attemptID,
		Revision:  revision,
		Recovery:  recovery,
		Runner:    runner,
		Base:      base,
	})
}

// outcome is the run's status and reason, with a recorded cancellation
// taking precedence over the workflow's own result.
func outcome(db *trace.DB, runID string, r *run.Run, code int) (int, string, string) {
	status, reason := r.Status(), r.Reason()
	if cancelled, err := db.Cancelled(runID); err == nil && cancelled {
		status, code = trace.StatusCancelled, trace.ExitCancelled
		if reason == "" {
			reason = "cancelled"
		}
	}
	return code, status, reason
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func release(db *trace.DB, runID string, out io.Writer) {
	// Released by the worker itself, which is the one process that knows its
	// own execution has stopped.
	if err := db.ReleaseCheckout(runID); err != nil {
		fmt.Fprintln(out, "lathe worker: releasing the reservation:", err)
	}
}

// PrintResult writes where its preserved result is: the patch, how to apply
// it at its base, and the ref keeping its commits. ok labels the patch
// accepted.
func PrintResult(w io.Writer, it trace.Iteration, ok bool) {
	if it.Patch != "" {
		label := "result patch"
		if !ok {
			label = "partial, unaccepted patch"
		}
		fmt.Fprintf(w, "  %s %s\n    apply from a checkout at %s: git apply --index %s\n",
			label, it.Patch, workspace.Short(it.Base), it.Patch)
	}
	if it.RecoveryRef != "" {
		fmt.Fprintf(w, "  commits kept under %s\n", it.RecoveryRef)
	}
}

// supervisor is the runner every subprocess of a writing run starts through.
func supervisor(db *trace.DB, runID, attemptID string) (*process.Runner, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving the supervisor helper: %w", err)
	}
	return &process.Runner{Bin: bin, Registrar: launches{db: db, runID: runID, attemptID: attemptID}}, nil
}

// launches records a worker's supervised launches, bound to its attempt.
type launches struct {
	db               *trace.DB
	runID, attemptID string
}

func (l launches) Intent(id string, argv []string) error {
	return l.db.RecordLaunchIntent(l.runID, l.attemptID, id, argv)
}

func (l launches) Register(id string, reg process.Registration) error {
	return l.db.RegisterLaunch(id, reg.Host, reg.PID, reg.PGID, reg.Endpoint, reg.Token)
}

func (l launches) Drained(id string) error { return l.db.DrainLaunch(id) }

// revalidate refuses a run whose source checkout is no longer on the branch
// row was submitted against.
func revalidate(spec Spec, row trace.Row) error {
	branch, err := workspace.Branch(spec.Workspace.Path)
	if err != nil {
		return err
	}
	if row.Branch != "" && branch != row.Branch {
		return fmt.Errorf("%s is on branch %s, but this run was submitted against %s; "+
			"nothing was changed", spec.Workspace.Path, branch, row.Branch)
	}
	return nil
}

// beat renews the lease and polls for cancellation on its own clock, so
// neither depends on a model or a test command producing output. It returns a
// channel closed when it has stopped.
func beat(ctx context.Context, db *trace.DB, attemptID, token string, stop context.CancelFunc, out io.Writer) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(Heartbeat)
		defer tick.Stop()
		var firstFailure time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			cancel, err := db.Beat(attemptID, token, Lease)
			switch {
			case errors.Is(err, trace.ErrClaimLost):
				fmt.Fprintln(out, "lathe worker: this attempt is no longer ours; stopping")
				stop()
				return
			case err != nil:
				if firstFailure.IsZero() {
					firstFailure = time.Now()
				}
				if time.Since(firstFailure) > dbGrace {
					fmt.Fprintln(out, "lathe worker: the database has been unreachable for",
						dbGrace, "— stopping execution and keeping partial results:", err)
					stop()
					return
				}
			default:
				firstFailure = time.Time{}
				if cancel {
					fmt.Fprintln(out, "lathe worker: cancellation requested; stopping")
					stop()
					return
				}
			}
		}
	}()
	return done
}

// signals turns SIGINT and SIGTERM into a cancellation. It reuses the
// cancellation path the design already needs rather than adding a second
// shutdown route, and it keeps a killed worker from leaving a lost run that
// occupies capacity.
func signals(ctx context.Context, stop context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer signal.Stop(ch)
		select {
		case <-ch:
			stop()
		case <-ctx.Done():
		}
	}()
}

// priors returns the run's iterations before row.Iteration as run.Prior.
func priors(db *trace.DB, dataRoot string, row trace.Row) ([]run.Prior, error) {
	its, err := db.Iterations(row.ID)
	if err != nil {
		return nil, err
	}
	var out []run.Prior
	for _, it := range its {
		if it.N < row.Iteration {
			out = append(out, run.Prior{Iteration: it.N, Request: it.Request, Dir: ReportDir(dataRoot, row.ID, it.N)})
		}
	}
	return out, nil
}
