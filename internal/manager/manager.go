// Package manager is the long-lived local process: it drains the queue, one
// worker process per run, and serves the dashboard. It owns no run state —
// the database does — so restarting it loses nothing except the wait between
// polls.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tyrelh/lathe/dashboard"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workspace"
)

// poll is how often the queue is checked. Dispatch latency of a second is
// invisible next to an agent turn, and polling costs one indexed query.
const poll = time.Second

// logFile is the manager's own log, at the top of the data root above the
// per-attempt worker logs, because "why is nothing running" has to be
// answerable.
const logFile = "manager.log"

func (m *manager) backfillTasks() {
	if err := m.db.BackfillTasks(); err != nil {
		fmt.Fprintln(m.out, "lathe manager: backfill tasks:", err)
	}
}

// Capacity is how many runs may execute at once, from LATHE_CAPACITY.
func Capacity() int {
	if n, err := strconv.Atoi(os.Getenv("LATHE_CAPACITY")); err == nil && n > 0 {
		return n
	}
	return 1
}

// Job is one launch request. The attempt identity is what makes a repeated
// request idempotent: a worker that already claimed it wins, and the second
// process exits without doing any work.
type Job struct {
	RunID     string
	AttemptID string
	DataRoot  string
	LogPath   string
}

// Launcher starts, inspects and stops workers. It is the one seam built for
// the future: an EC2 launcher replaces it without the scheduler changing.
// The manager stores a handle as-is and records it as a local PID too. A
// claimed attempt whose handle was never recorded gets a local handle rebuilt
// from the worker's claimed PID, which only makes sense on the worker's host.
type Launcher interface {
	// Launch returns the handle for the started worker.
	Launch(Job) (handle string, err error)
	// Alive reports whether that worker is still running. An error means the
	// launcher cannot tell, which is not the same as "no": capacity stays
	// occupied until execution is confirmed stopped.
	Alive(handle string) (bool, error)
}

// Supervisors probes and stops the supervised launches a worker left
// behind, by the process group and authenticated endpoint each registered.
type Supervisors interface {
	GroupLive(pgid int) (bool, error)
	Stop(endpoint, token string) error
}

// stopWait is how long reconcile waits for a stopped launch's group to end.
var stopWait = 10 * time.Second

// Run drains the queue and serves the dashboard until ctx is cancelled. Only
// one manager per data directory: the lock is what enforces it, and the same
// lock is what a submitter checks before auto-starting one. actions are
// handed to the dashboard for its forms; nil ones leave it read-only. sources
// are handed to it for its reads outside the database.
func Run(ctx context.Context, dataRoot, version string, out io.Writer, actions dashboard.Actions, sources dashboard.Sources) error {
	out = &run.LockedWriter{W: out}
	lock, err := Lock(dataRoot)
	if err != nil {
		return fmt.Errorf("a manager is already running for %s", dataRoot)
	}
	defer lock.Release()

	db, err := trace.Open(dataRoot)
	if err != nil {
		return err
	}
	defer db.Close()

	m := &manager{db: db, dataRoot: dataRoot, out: out, launcher: Local{}, groups: Local{},
		capacity: Capacity(), viewPR: workspace.ViewPR}

	// Reservations and attempts are reconciled before anything new is
	// admitted, so a surviving worker keeps its capacity and its checkout.
	if err := m.reconcile(); err != nil {
		return err
	}

	pollCtx, cancelPolling := context.WithCancel(ctx)
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		m.pollPRs(pollCtx, prPoll, m.backfillTasks)
	}()
	defer func() {
		cancelPolling()
		<-pollDone
	}()

	srv := &http.Server{Addr: dashboard.Addr, Handler: dashboard.Handler(dataRoot, version, actions, sources)}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(out, "lathe manager: dashboard:", err)
		}
	}()
	defer srv.Close()

	fmt.Fprintf(out, "lathe manager: capacity %d · dashboard http://%s\n", m.capacity, dashboard.Addr)
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return m.shutdown()
		case <-tick.C:
			if err := m.dispatch(); err != nil {
				fmt.Fprintln(out, "lathe manager:", err)
			}
		}
	}
}

// Lock takes the data-directory lock. A submitter uses it as a test: nobody
// holding it means no manager is running, which is the branch auto-start
// hangs off.
func Lock(dataRoot string) (*workspace.Lock, error) {
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return nil, err
	}
	return workspace.Acquire(dataRoot, filepath.Join(dataRoot, "manager"))
}

type manager struct {
	viewPR   func(string, string) (workspace.PR, error)
	db       *trace.DB
	dataRoot string
	out      io.Writer
	launcher Launcher
	groups   Supervisors
	capacity int
	// uncertain is the attempt whose worker the launcher could not vouch for.
	// It holds capacity until a person confirms the process is gone, and the
	// refusal message is the whole procedure for freeing it.
	uncertain map[string]bool
	// blocked is the source repositories whose worktree cleanup is unresolved;
	// their queued runs are not dispatched.
	blocked map[string]error
	warned  bool
}

// dispatch launches queued runs, oldest first, while capacity allows. A run
// in a blocked repository is skipped, as is a checkout-mode run whose
// checkout another active run holds.
func (m *manager) dispatch() error {
	active, err := m.db.Active()
	if err != nil {
		return err
	}
	if len(active) >= m.capacity {
		m.refuse(active)
		return nil
	}
	m.warned = false

	held, err := m.db.HeldCheckouts()
	if err != nil {
		return err
	}
	// A dispatched checkout run intends to hold its checkout before its
	// worker has reserved it, so the reservations alone are not enough:
	// without this a second poll would dispatch into a directory the first
	// run is about to take, and that run would fail instead of waiting.
	for _, row := range active {
		if attempt, err := m.db.GetAttempt(row.AttemptID); err != nil || attempt.Mode == trace.ModeCheckout {
			held[row.Repo] = row.ID
		}
	}
	queued, err := m.db.Queued()
	if err != nil {
		return err
	}
	for _, row := range queued {
		if len(active) >= m.capacity {
			return nil
		}
		if m.blocked[row.Repo] != nil {
			continue
		}
		mode := worker.Mode(row.Spec)
		if _, busy := held[row.Repo]; busy && mode == trace.ModeCheckout {
			// Another run is in that checkout. Waiting is the whole strategy.
			continue
		}
		if err := m.launch(row, mode); err != nil {
			fmt.Fprintf(m.out, "lathe manager: dispatching %s: %v\n", row.ID, err)
			continue
		}
		if mode == trace.ModeCheckout {
			held[row.Repo] = row.ID
		}
		active = append(active, row)
	}
	return nil
}

// launch records the dispatch with its execution mode, then starts the
// worker. A worker that fails to start marks the run lost.
func (m *manager) launch(row trace.Row, mode string) error {
	attemptID := trace.NewAttemptID(row.ID)
	logPath := worker.LogPath(m.dataRoot, row.ID, attemptID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	if err := m.db.ReserveMode(row.ID, attemptID, logPath, mode); err != nil {
		return err
	}
	handle, err := m.launcher.Launch(Job{
		RunID: row.ID, AttemptID: attemptID, DataRoot: m.dataRoot,
		LogPath: logPath,
	})
	if err != nil {
		// Nothing was started, so nothing has to be confirmed stopped.
		return m.db.MarkLost(row.ID, "launching the worker failed: "+err.Error())
	}
	fmt.Fprintf(m.out, "lathe manager: %s %s -> %s (%s)\n", row.Workflow, row.ID, handle, logPath)
	return m.db.SetHandle(attemptID, handle, localPID(handle))
}

// reconcile settles what the last manager left behind, before anything new
// is admitted. A run whose worker is still there keeps its capacity and will
// record its own outcome. A run whose worker is confirmed gone is lost: its
// unadmitted launches are cancelled, its admitted ones stopped, and once
// nothing of it can write, its worktree's result is preserved and the
// worktree removed. Cleanup an earlier worker or manager left pending is
// retried too.
func (m *manager) reconcile() error {
	m.uncertain = map[string]bool{}
	active, err := m.db.Active()
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	for _, row := range active {
		attempt, err := m.db.GetAttempt(row.AttemptID)
		if err != nil {
			return err
		}
		handle := attempt.Handle
		if handle == "" && attempt.Claimed != "" && attempt.PID > 0 {
			// Launched, but the handle was never recorded: the claim says
			// which process took the attempt.
			if attempt.Host != host {
				m.uncertain[row.ID] = true
				continue
			}
			handle = localHandle(attempt.PID)
		}
		if handle == "" {
			// No worker claimed it, and marking it lost revokes the attempt:
			// a late worker's claim needs the run still starting.
			if err := m.db.MarkLost(row.ID, "the manager stopped before the worker was launched"); err != nil {
				return err
			}
			if err := m.db.ReleaseCheckout(row.ID); err != nil {
				return err
			}
			continue
		}
		alive, err := m.launcher.Alive(handle)
		switch {
		case err != nil:
			m.uncertain[row.ID] = true
		case alive:
			fmt.Fprintf(m.out, "lathe manager: %s is still running (pid %d)\n", row.ID, attempt.PID)
		default:
			// Lost first: from here no intent of the run can register.
			if err := m.db.MarkLost(row.ID, "the worker is gone; its outcome is unknown"); err != nil {
				return err
			}
			if err := m.stopLaunches(row.ID, "", host); err != nil {
				fmt.Fprintf(m.out, "lathe manager: %s: %v; its worktree is kept until they are confirmed stopped\n", row.ID, err)
				m.uncertain[row.ID] = true
				continue
			}
			if err := m.db.ReleaseCheckout(row.ID); err != nil {
				return err
			}
		}
	}
	unresolved, err := m.cleanup(host)
	if err != nil {
		return err
	}
	m.blocked = unresolved
	for repo, err := range unresolved {
		fmt.Fprintf(m.out, "lathe manager: recovery is unresolved in %s; its queued runs wait until the cause is fixed and the manager restarted, which retries it: %v\n", repo, err)
	}
	return nil
}

// stopLaunches settles every open launch of runID except those of attempt
// keep. An intent that never registered admitted nothing and is cancelled. A
// registered supervisor is asked to stop, and its group is waited for; one
// that will not stop is an error.
func (m *manager) stopLaunches(runID, keep, host string) error {
	launches, err := m.db.OpenLaunches(runID)
	if err != nil {
		return err
	}
	var errs []error
	for _, l := range launches {
		if keep != "" && l.AttemptID == keep {
			continue
		}
		if l.State == trace.LaunchIntent {
			if cancelled, err := m.db.CancelLaunch(l.ID); err != nil {
				errs = append(errs, err)
				continue
			} else if cancelled {
				continue
			}
			// It registered meanwhile: settle it from its registration.
			fresh, open, err := m.openLaunch(runID, l.ID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !open {
				continue
			}
			l = fresh
		}
		if l.Host != "" && l.Host != host {
			errs = append(errs, fmt.Errorf("launch %s runs on %s, not this host", l.ID, l.Host))
			continue
		}
		if l.PGID > 1 {
			live, err := m.groups.GroupLive(l.PGID)
			if err != nil {
				errs = append(errs, fmt.Errorf("probing launch %s (process group %d): %w", l.ID, l.PGID, err))
				continue
			}
			if live {
				if err := m.groups.Stop(l.Endpoint, l.Token); err != nil {
					fmt.Fprintf(m.out, "lathe manager: asking launch %s to stop: %v\n", l.ID, err)
				}
				if !m.waitStopped(l.PGID) {
					errs = append(errs, fmt.Errorf("process group %d of launch %s did not stop; stop it with kill -TERM -%d", l.PGID, l.ID, l.PGID))
					continue
				}
			}
		}
		if err := m.db.DrainLaunch(l.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// openLaunch is runID's launch id, and whether it is still open.
func (m *manager) openLaunch(runID, id string) (trace.Launch, bool, error) {
	launches, err := m.db.OpenLaunches(runID)
	if err != nil {
		return trace.Launch{}, false, err
	}
	for _, l := range launches {
		if l.ID == id {
			return l, true, nil
		}
	}
	return trace.Launch{}, false, nil
}

func (m *manager) waitStopped(pgid int) bool {
	deadline := time.Now().Add(stopWait)
	for {
		if live, err := m.groups.GroupLive(pgid); err == nil && !live {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cleanup preserves and removes every recorded worktree whose run no longer
// executes in it. It returns the source repositories it could not finish,
// each with why; their unsettled worktrees stay recorded pending. Worktrees
// of runs with an uncertain worker are skipped.
func (m *manager) cleanup(host string) (map[string]error, error) {
	worktrees, err := m.db.Worktrees()
	if err != nil {
		return nil, err
	}
	unresolved := map[string]error{}
	for _, w := range worktrees {
		if w.Current && !trace.Terminal(w.RunStatus) {
			continue // its worker is alive and owns it
		}
		if m.uncertain[w.RunID] {
			continue
		}
		fail := func(err error) {
			unresolved[w.Repo] = errors.Join(unresolved[w.Repo], err)
		}
		keep := func(err error) {
			if serr := m.db.SetWorktreeState(w.RunID, w.Iteration, trace.WorktreePending, err.Error()); serr != nil {
				err = errors.Join(err, serr)
			}
			fail(err)
		}
		live := ""
		if !trace.Terminal(w.RunStatus) {
			row, err := m.db.Get(w.RunID)
			if err != nil {
				keep(err)
				continue
			}
			live = row.AttemptID
		}
		if err := m.stopLaunches(w.RunID, live, host); err != nil {
			keep(fmt.Errorf("%s: %w; its worktree %s is kept", w.RunID, err, w.Path))
			continue
		}
		lock, err := workspace.Acquire(m.dataRoot, w.Path)
		if err != nil {
			keep(fmt.Errorf("%s: %w; its worktree is kept", w.Path, err))
			continue
		}
		err = worker.Settle(m.db, m.dataRoot, w, true)
		lock.Release()
		if err != nil {
			fail(fmt.Errorf("cleaning up %s: %w", w.Path, err))
			continue
		}
		fmt.Fprintf(m.out, "lathe manager: removed %s\n", w.Path)
	}
	return unresolved, nil
}

// refuse prints, once, the manual steps that free capacity held by a run whose
// worker cannot be confirmed stopped.
func (m *manager) refuse(active []trace.Row) {
	if m.warned {
		return
	}
	m.warned = true
	for _, row := range active {
		if !m.uncertain[row.ID] {
			continue
		}
		attempt, err := m.db.GetAttempt(row.AttemptID)
		if err != nil {
			continue
		}
		fmt.Fprintf(m.out, `lathe manager: capacity is held by a run whose worker cannot be confirmed.

  run        %s (%s)
  repository %s
  worker     pid %d, process group %d on %s
  heartbeat  %s (last seen %s)
  log        %s

Nothing new will start until that process is confirmed stopped. Stop it with:

  kill -TERM -%d

then restart the manager. Clearing database records does not stop a process or
make its checkout available.
`, row.ID, row.Workflow, row.Repo, attempt.PID, attempt.PID, attempt.Host,
			attempt.Heartbeat, age(attempt.Heartbeat), attempt.LogPath, attempt.PID)
	}
}

// shutdown stops dispatch and says what keeps running. A worker is an
// independent process and is deliberately left alone.
func (m *manager) shutdown() error {
	active, err := m.db.Active()
	if err != nil {
		return err
	}
	if len(active) == 0 {
		fmt.Fprintln(m.out, "lathe manager: stopped")
		return nil
	}
	fmt.Fprintln(m.out, "lathe manager: stopped; these runs continue in their own processes:")
	for _, row := range active {
		fmt.Fprintf(m.out, "  %s  %s  %s\n    cancel with: lathe cancel %s\n",
			row.ID, row.Workflow, row.Repo, row.ID)
	}
	return nil
}

func age(stamp string) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return "unknown"
	}
	return time.Since(t).Truncate(time.Second).String() + " ago"
}
