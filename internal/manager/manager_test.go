package manager

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/trace"
)

// fake is a launcher that records what it was asked to start and answers
// whatever a test wants about whether it is still running. Claim races,
// duplicate dispatch and restart reconciliation are transaction properties,
// so real subprocesses here would add wall clock and flake without testing
// anything the store does not already decide.
type fake struct {
	mu      sync.Mutex
	jobs    []Job
	alive   bool
	unknown error
	fail    error
}

func (f *fake) Launch(j Job) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", f.fail
	}
	f.jobs = append(f.jobs, j)
	return "handle-" + strconv.Itoa(len(f.jobs)), nil
}

func (f *fake) Alive(string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive, f.unknown
}

func newManager(t *testing.T, capacity int, l Launcher) (*manager, *trace.DB, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	out := &bytes.Buffer{}
	return &manager{db: db, dataRoot: root, out: out, launcher: l,
		capacity: capacity, uncertain: map[string]bool{}}, db, out
}

func submit(t *testing.T, db *trace.DB, workflow, repo string) string {
	t.Helper()
	id, err := db.Submit(trace.Request{Workflow: workflow, Repo: repo, Request: "x", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Capacity is the whole scheduling rule: one run executes, the rest wait, and
// the dispatch is recorded before the worker is launched.
func TestCapacityHoldsTheQueue(t *testing.T) {
	l := &fake{alive: true}
	m, db, _ := newManager(t, 1, l)
	submit(t, db, "scout", "/a")
	submit(t, db, "plan", "/b")
	queued, err := db.Queued()
	if err != nil {
		t.Fatal(err)
	}
	first := queued[0].ID // oldest first, and inside one second by run ID

	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	if len(l.jobs) != 1 || l.jobs[0].RunID != first {
		t.Fatalf("launched %+v; want only %s", l.jobs, first)
	}
	row, _ := db.Get(first)
	if row.Status != trace.StatusStarting || row.AttemptID == "" {
		t.Fatalf("dispatch was not recorded before the launch: %+v", row)
	}
	attempt, err := db.GetAttempt(row.AttemptID)
	if err != nil || attempt.Handle != "handle-1" || attempt.LogPath == "" {
		t.Fatalf("attempt = %+v, %v", attempt, err)
	}

	// Still at capacity on the next poll, so the second run stays queued.
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	if len(l.jobs) != 1 {
		t.Fatalf("capacity was exceeded: %+v", l.jobs)
	}

	// Raising capacity admits the rest.
	m.capacity = 2
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	if len(l.jobs) != 2 {
		t.Fatalf("launched %d; want 2", len(l.jobs))
	}
}

// One checkout, one run: a queued run whose repository is reserved waits even
// when capacity is free.
func TestOneRunPerCheckout(t *testing.T) {
	l := &fake{alive: true}
	m, db, _ := newManager(t, 4, l)
	first := submit(t, db, "scout", "/repo")
	submit(t, db, "plan", "/repo")
	if err := db.ReserveCheckout("/repo", "host", first); err != nil {
		t.Fatal(err)
	}
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	if len(l.jobs) != 0 {
		t.Fatalf("dispatched into a reserved checkout: %+v", l.jobs)
	}
	if err := db.ReleaseCheckout(first); err != nil {
		t.Fatal(err)
	}
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	// One of the two, not both: the first to be dispatched reserves nothing
	// itself yet, so the manager holds the checkout for the rest of the pass.
	if len(l.jobs) != 1 {
		t.Fatalf("launched %d into one checkout; want 1", len(l.jobs))
	}
}

// Capacity to spare is not permission to put two runs in one checkout: the
// second waits across polls, rather than being dispatched and failing when it
// cannot reserve the directory.
func TestSecondRunWaitsForTheCheckoutAcrossPolls(t *testing.T) {
	l := &fake{alive: true}
	m, db, _ := newManager(t, 4, l)
	submit(t, db, "scout", "/repo")
	submit(t, db, "plan", "/repo")
	for i := 0; i < 3; i++ {
		if err := m.dispatch(); err != nil {
			t.Fatal(err)
		}
	}
	if len(l.jobs) != 1 {
		t.Fatalf("launched %d runs into one checkout; want 1", len(l.jobs))
	}
}

// A launch that never happened leaves nothing to stop, so the run is lost and
// its checkout is freed.
func TestFailedLaunchIsLost(t *testing.T) {
	l := &fake{fail: errFake}
	m, db, _ := newManager(t, 1, l)
	id := submit(t, db, "scout", "/a")
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	row, _ := db.Get(id)
	if row.Status != trace.StatusLost || !strings.Contains(row.Reason, "launching the worker failed") {
		t.Fatalf("run = %+v", row)
	}
}

var errFake = errFakeType{}

type errFakeType struct{}

func (errFakeType) Error() string { return "no launcher here" }

// A manager restart: a worker that is gone leaves a lost run and a free
// checkout, and a worker that survived keeps both.
func TestReconcileAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alive bool
		want  string
		holds bool
	}{
		{name: "worker is gone", alive: false, want: trace.StatusLost},
		{name: "worker survived", alive: true, want: trace.StatusRunning, holds: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &fake{alive: true}
			m, db, _ := newManager(t, 1, l)
			id := submit(t, db, "scout", "/a")
			if err := m.dispatch(); err != nil {
				t.Fatal(err)
			}
			row, _ := db.Get(id)
			if _, err := db.Claim(row.AttemptID, trace.NewClaimToken(), "host", 424242, time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := db.ReserveCheckout("/a", "host", id); err != nil {
				t.Fatal(err)
			}

			l.alive = tc.alive
			if err := m.reconcile(); err != nil {
				t.Fatal(err)
			}
			row, _ = db.Get(id)
			if row.Status != tc.want {
				t.Fatalf("status = %q; want %q", row.Status, tc.want)
			}
			held, _ := db.HeldCheckouts()
			if _, still := held["/a"]; still != tc.holds {
				t.Fatalf("checkout held = %v; want %v", still, tc.holds)
			}
		})
	}
}

// A dispatch recorded with no launcher handle is a manager that died between
// the two writes. No process was ever started, so nothing has to be confirmed
// stopped.
func TestReconcileUnlaunchedDispatch(t *testing.T) {
	m, db, _ := newManager(t, 1, &fake{})
	id := submit(t, db, "scout", "/a")
	if err := db.Reserve(id, trace.NewAttemptID(id), "/tmp/log"); err != nil {
		t.Fatal(err)
	}
	if err := m.reconcile(); err != nil {
		t.Fatal(err)
	}
	if row, _ := db.Get(id); row.Status != trace.StatusLost {
		t.Fatalf("status = %q; want lost", row.Status)
	}
}

// A worker the launcher cannot vouch for holds capacity, and the refusal is
// the entire recovery procedure: it must name the run, the process, the log
// and the command that stops it.
func TestUncertainWorkerBlocksAndExplains(t *testing.T) {
	l := &fake{alive: true}
	m, db, out := newManager(t, 1, l)
	id := submit(t, db, "scout", "/a")
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	row, _ := db.Get(id)
	if _, err := db.Claim(row.AttemptID, trace.NewClaimToken(), "host", 424242, time.Minute); err != nil {
		t.Fatal(err)
	}

	l.unknown = errFake
	if err := m.reconcile(); err != nil {
		t.Fatal(err)
	}
	if row, _ := db.Get(id); row.Status != trace.StatusRunning {
		t.Fatalf("an uncertain worker's run was settled as %q", row.Status)
	}
	submit(t, db, "plan", "/b")
	out.Reset()
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	if len(l.jobs) != 1 {
		t.Fatalf("work was admitted past an uncertain worker: %+v", l.jobs)
	}
	message := out.String()
	for _, want := range []string{id, "scout", "/a", "424242", "kill -TERM -424242",
		"worker.log", "restart the manager"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal never mentions %q:\n%s", want, message)
		}
	}
}

// Shutdown stops dispatch and hands back the runs that keep going, with the
// command that stops them.
func TestShutdownListsContinuingRuns(t *testing.T) {
	l := &fake{alive: true}
	m, db, out := newManager(t, 1, l)
	id := submit(t, db, "scout", "/a")
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := m.shutdown(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "lathe cancel "+id) {
		t.Fatalf("shutdown did not say how to stop %s:\n%s", id, out)
	}
}

func TestCapacityDefaultsToOne(t *testing.T) {
	t.Setenv("LATHE_CAPACITY", "")
	if n := Capacity(); n != 1 {
		t.Fatalf("capacity = %d; want 1", n)
	}
	t.Setenv("LATHE_CAPACITY", "3")
	if n := Capacity(); n != 3 {
		t.Fatalf("capacity = %d; want 3", n)
	}
	t.Setenv("LATHE_CAPACITY", "nonsense")
	if n := Capacity(); n != 1 {
		t.Fatalf("a bad capacity should fall back to 1, got %d", n)
	}
}

// One manager per data directory, which is also the check a submitter makes
// before auto-starting one.
func TestDataDirectoryLockIsExclusive(t *testing.T) {
	root := t.TempDir()
	first, err := Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(root); err == nil {
		t.Fatal("two managers took the same data directory")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Lock(root)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	second.Release()
}
