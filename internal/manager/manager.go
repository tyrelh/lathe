// Package manager is the long-lived local process: it drains the queue, one
// worker process per run, and serves the dashboard. It owns no run state —
// the database does — so restarting it loses nothing except the wait between
// polls.
package manager

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tyrelh/lathe/dashboard"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workspace"
)

// poll is how often the queue is checked. Dispatch latency of a second is
// invisible next to an agent turn, and polling costs one indexed query.
const poll = time.Second

// LogFile is the manager's own log, beside the per-attempt worker logs,
// because "why is nothing running" has to be answerable.
const LogFile = "manager.log"

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
// Handles are opaque strings the manager persists and hands back.
type Launcher interface {
	// Launch returns the handle for the started worker.
	Launch(Job) (handle string, err error)
	// Alive reports whether that worker is still running. An error means the
	// launcher cannot tell, which is not the same as "no": capacity stays
	// occupied until execution is confirmed stopped.
	Alive(handle string) (bool, error)
}

// Run drains the queue and serves the dashboard until ctx is cancelled. Only
// one manager per data directory: the lock is what enforces it, and the same
// lock is what a submitter checks before auto-starting one.
func Run(ctx context.Context, dataRoot, version string, out io.Writer) error {
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

	m := &manager{db: db, dataRoot: dataRoot, out: out, launcher: Local{},
		capacity: Capacity()}

	// Reservations and attempts are reconciled before anything new is
	// admitted, so a surviving worker keeps its capacity and its checkout.
	if err := m.reconcile(); err != nil {
		return err
	}

	srv := &http.Server{Addr: dashboard.Addr, Handler: dashboard.Handler(dataRoot, version)}
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
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		return nil, err
	}
	return workspace.Acquire(dataRoot, filepath.Join(dataRoot, "manager"))
}

type manager struct {
	db       *trace.DB
	dataRoot string
	out      io.Writer
	launcher Launcher
	capacity int
	// uncertain is the attempt whose worker the launcher could not vouch for.
	// It holds capacity until a person confirms the process is gone, and the
	// refusal message is the whole recovery procedure.
	uncertain map[string]bool
	warned    bool
}

// dispatch admits as much queued work as capacity and checkout availability
// allow, oldest first.
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
	// A dispatched run intends to hold its checkout before its worker has
	// reserved it, so the reservations alone are not enough: without this a
	// second poll would dispatch into a directory the first run is about to
	// take, and that run would fail instead of waiting.
	for _, row := range active {
		held[row.Repo] = row.ID
	}
	queued, err := m.db.Queued()
	if err != nil {
		return err
	}
	for _, row := range queued {
		if len(active) >= m.capacity {
			return nil
		}
		if _, busy := held[row.Repo]; busy {
			// Another run is in that checkout. Waiting is the whole strategy;
			// concurrency against one checkout needs worktrees.
			continue
		}
		if err := m.launch(row); err != nil {
			fmt.Fprintf(m.out, "lathe manager: dispatching %s: %v\n", row.ID, err)
			continue
		}
		held[row.Repo] = row.ID
		active = append(active, row)
	}
	return nil
}

// launch records the dispatch before spawning anything. If the manager dies
// between the two, reconciliation has an attempt row to ask about rather than
// a run nobody can account for.
func (m *manager) launch(row trace.Row) error {
	attemptID := trace.NewAttemptID(row.ID)
	logPath := worker.LogPath(m.dataRoot, row.ID, attemptID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	if err := m.db.Reserve(row.ID, attemptID, logPath); err != nil {
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
	return m.db.SetHandle(attemptID, handle, pid(handle))
}

// reconcile settles what the last manager left behind. A run whose worker is
// gone is lost; a run whose worker is still there keeps its capacity and its
// checkout and will record its own outcome.
func (m *manager) reconcile() error {
	m.uncertain = map[string]bool{}
	active, err := m.db.Active()
	if err != nil {
		return err
	}
	for _, row := range active {
		attempt, err := m.db.GetAttempt(row.AttemptID)
		if err != nil {
			return err
		}
		if attempt.Handle == "" {
			// The previous manager died between recording the dispatch and
			// spawning: no process was ever started, so nothing has to stop.
			if err := m.db.MarkLost(row.ID, "the manager stopped before the worker was launched"); err != nil {
				return err
			}
			if err := m.db.ReleaseCheckout(row.ID); err != nil {
				return err
			}
			continue
		}
		alive, err := m.launcher.Alive(attempt.Handle)
		switch {
		case err != nil:
			m.uncertain[row.ID] = true
		case alive:
			fmt.Fprintf(m.out, "lathe manager: %s is still running (pid %d)\n", row.ID, attempt.PID)
		default:
			if err := m.db.MarkLost(row.ID, "the worker is gone; its outcome is unknown"); err != nil {
				return err
			}
			if err := m.db.ReleaseCheckout(row.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// refuse prints why nothing is being admitted. There is no recovery command
// in this release, so this message is the recovery documentation: everything
// a person needs to unblock the queue by hand is in it.
func (m *manager) refuse(active []trace.Row) {
	if m.warned {
		return
	}
	m.warned = true
	for _, row := range active {
		attempt, err := m.db.GetAttempt(row.AttemptID)
		if err != nil {
			continue
		}
		if !m.uncertain[row.ID] {
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

func pid(handle string) int {
	n, _ := strconv.Atoi(handle)
	return n
}
