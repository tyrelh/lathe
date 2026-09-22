// Package worker executes one run. It is a separate process from the
// submitter and from the manager, which is where durability comes from: the
// submitter can be interrupted, the manager can be restarted, and the run
// continues because the process doing the work is neither of them.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workflow"
	"github.com/tyrelh/lathe/internal/workspace"
)

// SpecVersion is stamped into every recorded specification. A worker that
// does not recognise the version fails the run rather than guessing at it.
const SpecVersion = 2

// Spec is everything about a run that configuration could otherwise change
// while it waits in the queue: the workflow, the request, where it executes
// and the effective roster. Repository contents are not in it — a worker reads
// files when it starts — and neither are the worker's secrets.
type Spec struct {
	Version   int              `json:"version"`
	Workflow  string           `json:"workflow"`
	Request   string           `json:"request"`
	Workspace workspace.Desc   `json:"workspace"`
	Roster    config.Snapshot  `json:"roster"`
	Overrides config.Overrides `json:"overrides"`
}

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
	if spec.Version != SpecVersion || spec.Roster.Version != config.SnapshotVersion {
		return 0, fmt.Errorf("run %s was recorded by an incompatible lathe (spec v%d)", row.ID, spec.Version)
	}
	if err := spec.Workspace.Check(); err != nil {
		return 0, err
	}

	// The reservation is the database's record of who owns the checkout; the
	// lock is what actually keeps a second lathe process out of it. Both,
	// because either alone is a lie: the row survives a crash and the lock
	// does not.
	if err := db.ReserveCheckout(spec.Workspace.Path, spec.Workspace.Host, row.ID); err != nil {
		return 0, err
	}
	lock, err := workspace.Acquire(dataRoot, spec.Workspace.Path)
	if err != nil {
		return 0, err
	}
	defer func() {
		lock.Release()
		// Released by the worker itself, which is the one process that knows
		// its own execution has stopped.
		if err := db.ReleaseCheckout(row.ID); err != nil {
			fmt.Fprintln(out, "lathe worker: releasing the checkout reservation:", err)
		}
	}()

	if err := revalidate(spec, row); err != nil {
		return 0, err
	}
	// A checkout with no commits yet has none to record, which is not a
	// reason to refuse to read it.
	commit, _ := workspace.Head(spec.Workspace.Path)
	if err := db.SetCommit(row.ID, commit); err != nil {
		return 0, err
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	beats := beat(ctx, db, attemptID, token, stop, out)
	// A signal is a cancellation, not a second shutdown route: the worker
	// stops its children, keeps what it has produced, and records cancelled.
	signals(ctx, stop)

	graph, ok := workflow.Graphs[spec.Workflow]
	if !ok {
		return 0, fmt.Errorf("unknown workflow %q", spec.Workflow)
	}
	r, err := run.Open(run.Options{
		ID:       row.ID,
		Workflow: spec.Workflow,
		Request:  spec.Request,
		Repo:     spec.Workspace.Path,
		Dir:      filepath.Join(dataRoot, "runs", row.ID),
		Work:     AttemptDir(dataRoot, row.ID, attemptID),
		Snapshot: spec.Roster,
		DB:       db,
		Ctx:      ctx,
		Out:      out,
	})
	if err != nil {
		return 0, err
	}
	code := graph(r)

	// Subprocesses are gone with the context by the time the graph returns,
	// but the cancellation path has to be able to say so: stopping dispatch
	// and releasing the checkout both depend on execution having ended.
	stop()
	<-beats

	status, reason := r.Status(), r.Reason()
	if cancelled, err := db.Cancelled(row.ID); err == nil && cancelled {
		status, code = trace.StatusCancelled, 130
		if reason == "" {
			reason = "cancelled"
		}
	}
	if err := db.Complete(row.ID, attemptID, token, status, reason); err != nil {
		// A terminal state that is already recorded is not an error here: the
		// race with cancellation is settled by whichever committed first.
		if !errors.Is(err, trace.ErrClaimLost) {
			return code, err
		}
	}
	return code, nil
}

// revalidate is the execution-time half of the checkout checks. Submission
// checked the same things for early feedback; between then and now a person
// may have switched branches or started editing.
func revalidate(spec Spec, row trace.Row) error {
	branch, err := workspace.Branch(spec.Workspace.Path)
	if err != nil {
		return err
	}
	if row.Branch != "" && branch != row.Branch {
		return fmt.Errorf("%s is on branch %s, but this run was submitted against %s; "+
			"nothing was changed", spec.Workspace.Path, branch, row.Branch)
	}
	// A writing run queued against a clean checkout that is now dirty fails
	// without touching what is there: those changes are a person's, not an
	// agent's.
	if workflow.Writes[spec.Workflow] {
		if err := permit.Clean(spec.Workspace.Path); err != nil {
			return err
		}
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

// Marshal is how the submitter records a specification.
func (s Spec) Marshal() ([]byte, error) { return json.Marshal(s) }

func unmarshalSpec(b []byte, s *Spec) error {
	if len(b) == 0 {
		return errors.New("this run has no recorded execution specification")
	}
	return json.Unmarshal(b, s)
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

// AttemptDir is where one attempt's raw stream, guard, scope and Pi session
// live. Reports stay in the run directory, which is where readers already
// look for them.
func AttemptDir(dataRoot, runID, attemptID string) string {
	return filepath.Join(dataRoot, "runs", runID, attemptID)
}

// LogPath is the per-attempt worker log the manager redirects output to.
func LogPath(dataRoot, runID, attemptID string) string {
	return filepath.Join(AttemptDir(dataRoot, runID, attemptID), "worker.log")
}
