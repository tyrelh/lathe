package manager

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workspace"
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
		groups: Local{}, capacity: capacity, uncertain: map[string]bool{}}, db, out
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
// the entire manual procedure: it must name the run, the process, the log
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

// submitSpec records a run carrying a specification of the given version.
func submitSpec(t *testing.T, db *trace.DB, workflow, repo string, version int) string {
	t.Helper()
	spec, err := worker.Spec{Version: version, Workflow: workflow, Request: "x", Base: "abc"}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.Submit(trace.Request{Workflow: workflow, Repo: repo, Request: "x", Branch: "main", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Writing runs execute in worktrees of their own, so several for one
// repository are admitted up to capacity, even beside a run in the checkout.
// Read-only runs still wait for the checkout, and a queued legacy writing run
// is dispatched only to be refused, without holding the checkout.
func TestDispatchByExecutionMode(t *testing.T) {
	l := &fake{alive: true}
	m, db, _ := newManager(t, 5, l)
	reader := submitSpec(t, db, "scout", "/repo", worker.SpecVersion)
	first := submitSpec(t, db, "build", "/repo", worker.SpecVersion)
	second := submitSpec(t, db, "build", "/repo", worker.SpecVersion)
	legacy := submitSpec(t, db, "implement", "/repo", 3)
	waiting := submitSpec(t, db, "plan", "/repo", 3)
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	launched := map[string]bool{}
	for _, j := range l.jobs {
		launched[j.RunID] = true
	}
	for _, id := range []string{first, second, legacy} {
		if !launched[id] {
			t.Fatalf("%s was not launched (%+v)", id, l.jobs)
		}
	}
	// The two checkout runs share one checkout: whichever is older goes.
	if launched[reader] == launched[waiting] {
		t.Fatalf("checkout runs launched: %v and %v; want exactly one", launched[reader], launched[waiting])
	}
	if !launched[reader] {
		reader = waiting
	}
	for id, mode := range map[string]string{reader: trace.ModeCheckout, first: trace.ModeWorktree,
		second: trace.ModeWorktree, legacy: trace.ModeReject} {
		row, _ := db.Get(id)
		if a, _ := db.GetAttempt(row.AttemptID); a.Mode != mode {
			t.Fatalf("%s mode = %q; want %q", id, a.Mode, mode)
		}
	}

	// Capacity still bounds writing runs.
	l2 := &fake{alive: true}
	m2, db2, _ := newManager(t, 2, l2)
	for i := 0; i < 3; i++ {
		submitSpec(t, db2, "build", "/repo", worker.SpecVersion)
	}
	if err := m2.dispatch(); err != nil {
		t.Fatal(err)
	}
	if len(l2.jobs) != 2 {
		t.Fatalf("launched %d builds; want 2", len(l2.jobs))
	}
}

// A lost run's worktree that cannot be exported holds back its repository
// only: the worktree stays on disk and recorded pending, queued runs for
// that repository wait, other repositories dispatch, and the next start
// retries it.
func TestStartupCleanupFailureBlocksItsRepository(t *testing.T) {
	l := &fake{alive: true}
	m, db, _ := newManager(t, 2, l)
	m.groups = &supervisors{live: map[int]bool{}}
	id := submitSpec(t, db, "build", "/nonexistent-repo", worker.SpecVersion)
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	row, _ := db.Get(id)
	if _, err := db.Claim(row.AttemptID, trace.NewClaimToken(), "host", 424242, time.Minute); err != nil {
		t.Fatal(err)
	}
	path := workspace.WorktreePath(m.dataRoot, id, 0)
	if err := db.SetWorktree(id, row.AttemptID, path, "abc", trace.WorktreeActive); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(path, 0o700)
	os.WriteFile(filepath.Join(path, "work.txt"), []byte("unsaved"), 0o600)

	l.alive = false
	if err := m.reconcile(); err != nil {
		t.Fatalf("reconcile = %v; want the failure confined to its repository", err)
	}
	if m.blocked["/nonexistent-repo"] == nil {
		t.Fatalf("blocked = %v; want /nonexistent-repo", m.blocked)
	}
	if _, err := os.Stat(filepath.Join(path, "work.txt")); err != nil {
		t.Fatal("the unexported worktree was removed")
	}
	its, _ := db.Iterations(id)
	if its[0].WorktreeState != trace.WorktreePending || its[0].CleanupError == "" {
		t.Fatalf("iteration = %+v", its[0])
	}

	before := len(l.jobs)
	waiting := submitSpec(t, db, "build", "/nonexistent-repo", worker.SpecVersion)
	other := submitSpec(t, db, "build", "/other-repo", worker.SpecVersion)
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	if got := l.jobs[before:]; len(got) != 1 || got[0].RunID != other {
		t.Fatalf("dispatched %+v; want only %s", got, other)
	}
	if row, _ := db.Get(waiting); row.Status != trace.StatusQueued {
		t.Fatalf("the blocked repository's run is %s; want queued", row.Status)
	}

	if err := m.reconcile(); err != nil || m.blocked["/nonexistent-repo"] == nil {
		t.Fatalf("retry: %v, blocked %v; want the same failure", err, m.blocked)
	}
}

// supervisors is a fake group probe: groups in live stay alive until a stop
// request names them.
type supervisors struct {
	live     map[int]bool
	stopped  []string
	stubborn bool
}

func (s *supervisors) GroupLive(pgid int) (bool, error) { return s.live[pgid], nil }

func (s *supervisors) Stop(endpoint, token string) error {
	s.stopped = append(s.stopped, endpoint+" "+token)
	if !s.stubborn {
		for pgid := range s.live {
			delete(s.live, pgid)
		}
	}
	return nil
}

// A dead worker's run is lost automatically: its unregistered intent is
// cancelled without a PID, its registered supervisor is asked to stop and
// waited for, and only then is its checkout released. A group that will not
// stop keeps the run's resources and says how to stop it.
func TestLostRunStopsLaunches(t *testing.T) {
	for _, stubborn := range []bool{false, true} {
		t.Run(strconv.FormatBool(stubborn), func(t *testing.T) {
			stopWait = 100 * time.Millisecond
			t.Cleanup(func() { stopWait = 10 * time.Second })
			l := &fake{alive: true}
			m, db, out := newManager(t, 1, l)
			sup := &supervisors{live: map[int]bool{777: true}, stubborn: stubborn}
			m.groups = sup
			id := submitSpec(t, db, "build", "/repo", worker.SpecVersion)
			if err := m.dispatch(); err != nil {
				t.Fatal(err)
			}
			row, _ := db.Get(id)
			host, _ := os.Hostname()
			if _, err := db.Claim(row.AttemptID, trace.NewClaimToken(), host, 424242, time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := db.RecordLaunchIntent(id, row.AttemptID, "never", []string{"git"}); err != nil {
				t.Fatal(err)
			}
			if err := db.RecordLaunchIntent(id, row.AttemptID, "admitted", []string{"pi"}); err != nil {
				t.Fatal(err)
			}
			if err := db.RegisterLaunch("admitted", host, 777, 777, "127.0.0.1:1", "tok"); err != nil {
				t.Fatal(err)
			}
			if err := db.ReserveCheckout("/wt", host, id); err != nil {
				t.Fatal(err)
			}

			l.alive = false
			if err := m.reconcile(); err != nil {
				t.Fatal(err)
			}
			if row, _ := db.Get(id); row.Status != trace.StatusLost {
				t.Fatalf("status = %q", row.Status)
			}
			if len(sup.stopped) != 1 || sup.stopped[0] != "127.0.0.1:1 tok" {
				t.Fatalf("stop requests = %v", sup.stopped)
			}
			open, _ := db.OpenLaunches(id)
			held, _ := db.HeldCheckouts()
			if stubborn {
				if len(open) != 1 || open[0].ID != "admitted" || held["/wt"] != id ||
					!strings.Contains(out.String(), "kill -TERM -777") {
					t.Fatalf("open %+v held %v\n%s", open, held, out)
				}
				return
			}
			if len(open) != 0 || len(held) != 0 {
				t.Fatalf("open %+v held %v", open, held)
			}
			// A late registration of the cancelled intent is refused.
			if err := db.RegisterLaunch("never", host, 1, 1, "x", "y"); err == nil {
				t.Fatal("a revoked intent registered")
			}
		})
	}
}

// Settling a run's launches leaves those of the attempt named to keep alone.
func TestStopLaunchesKeepsTheLiveAttempt(t *testing.T) {
	l := &fake{alive: true}
	m, db, _ := newManager(t, 1, l)
	sup := &supervisors{live: map[int]bool{777: true}}
	m.groups = sup
	id := submitSpec(t, db, "build", "/repo", worker.SpecVersion)
	if err := m.dispatch(); err != nil {
		t.Fatal(err)
	}
	row, _ := db.Get(id)
	host, _ := os.Hostname()
	if _, err := db.Claim(row.AttemptID, trace.NewClaimToken(), host, 424242, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordLaunchIntent(id, row.AttemptID, "live", []string{"pi"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterLaunch("live", host, 777, 777, "127.0.0.1:1", "tok"); err != nil {
		t.Fatal(err)
	}

	if err := m.stopLaunches(id, row.AttemptID, host); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenLaunches(id); len(open) != 1 || len(sup.stopped) != 0 {
		t.Fatalf("open %+v, stop requests %v; want the live attempt untouched", open, sup.stopped)
	}
	if err := m.stopLaunches(id, "", host); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenLaunches(id); len(open) != 0 || len(sup.stopped) != 1 {
		t.Fatalf("open %+v, stop requests %v; want it stopped", open, sup.stopped)
	}
}
