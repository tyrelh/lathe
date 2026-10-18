package trace

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestFreshTaskRun(t *testing.T) {
	for _, tc := range []struct {
		status, pr     string
		hasPR, allowed bool
	}{
		{StatusFail, "", false, true}, {StatusCancelled, "open", true, true},
		{StatusLost, "", false, true}, {StatusOK, "", false, true},
		{StatusOK, "closed", true, true}, {StatusQueued, "", false, false},
		{StatusStarting, "", false, false}, {StatusRunning, "", false, false},
		{StatusOK, "open", true, false}, {StatusFail, "merged", true, false},
	} {
		t.Run(tc.status+tc.pr, func(t *testing.T) {
			db := open(t)
			old := submit(t, db, "build", "/repo")
			row, _ := db.Get(old)
			setStatus(t, db, old, tc.status)
			if _, err := db.sql.Exec(`UPDATE runs SET pr_state = ?, submitted_at = '2099-01-01T00:00:00Z' WHERE run_id = ?`, nullIfEmpty(tc.pr), old); err != nil {
				t.Fatal(err)
			}
			before, _ := db.GetTask(*row.TaskID)
			req := Request{TaskID: *row.TaskID, Workflow: "build", Repo: "/repo", Request: "new request"}
			id, err := db.FreshTaskRun(req, func(Row) bool { return tc.hasPR })
			if !tc.allowed {
				if !errors.Is(err, ErrTaskConflict) {
					t.Fatalf("refusal: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			runs, err := db.TaskRuns(req.TaskID)
			if err != nil || len(runs) != 2 || runs[0].ID != id || runs[1].ID != old || runs[0].Submitted != "2099-01-01T00:00:01Z" {
				t.Fatalf("runs = %+v, %v", runs, err)
			}
			after, _ := db.GetTask(req.TaskID)
			if before != after {
				t.Fatal("task metadata changed")
			}
			if _, err := db.FreshTaskRun(req, nil); !errors.Is(err, ErrTaskConflict) {
				t.Fatalf("active: %v", err)
			}
		})
	}
}

func TestFreshTaskRunRollbackAndConcurrency(t *testing.T) {
	root := t.TempDir()
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	old := submit(t, db, "build", "/repo")
	row, _ := db.Get(old)
	setStatus(t, db, old, StatusFail)
	req := Request{TaskID: *row.TaskID, Workflow: "build", Repo: "/repo", Request: "fresh"}
	if _, err := db.sql.Exec(`CREATE TRIGGER refuse_fresh BEFORE INSERT ON iterations BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FreshTaskRun(req, nil); err == nil {
		t.Fatal("accepted failed insert")
	}
	runs, _ := db.TaskRuns(req.TaskID)
	if len(runs) != 1 {
		t.Fatal("failed insert left run")
	}
	if _, err := db.sql.Exec(`DROP TRIGGER refuse_fresh`); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, writer := range []*DB{db, other} {
		go func() {
			_, err := writer.FreshTaskRun(req, nil)
			results <- err
		}()
	}
	successes := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrTaskConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d", successes)
	}
}

func TestStartBacklogTask(t *testing.T) {
	root := t.TempDir()
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	id, err := db.CreateTask(TaskInput{Repo: "/repo", Text: "change", IssueRef: "42"})
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Workflow: "build", TaskID: id, Repo: "/repo", Request: "change", Spec: issueSpec("42")}
	for _, change := range []func(*Request){
		func(r *Request) { r.Repo = "/other" },
		func(r *Request) { r.Request = "stale" },
		func(r *Request) { r.Spec = issueSpec("43") },
		func(r *Request) { r.Spec = nil },
		func(r *Request) { r.Workflow = "plan" },
	} {
		stale := req
		change(&stale)
		if _, err := db.StartTask(stale); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("stale %+v: %v", stale, err)
		}
	}
	missing := req
	missing.TaskID = "missing"
	if _, err := db.StartTask(missing); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER refuse_start BEFORE INSERT ON iterations BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.StartTask(req); err == nil {
		t.Fatal("start succeeded despite trigger")
	}
	runs, _ := db.TaskRuns(id)
	if len(runs) != 0 {
		t.Fatal("failed start left a run")
	}
	if _, err := db.sql.Exec(`DROP TRIGGER refuse_start`); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, writer := range []*DB{db, other} {
		go func() {
			_, err := writer.StartTask(req)
			results <- err
		}()
	}
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, ErrTaskConflict) {
			conflicts++
		} else {
			t.Fatalf("concurrent start: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins = %d, conflicts = %d", wins, conflicts)
	}
	runs, _ = db.TaskRuns(id)
	if len(runs) != 1 || runs[0].Workflow != "build" || runs[0].Status != StatusQueued {
		t.Fatalf("runs = %+v", runs)
	}
	setStatus(t, db, runs[0].ID, StatusOK)
	if _, err := db.StartTask(req); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("terminal start: %v", err)
	}
	if _, err := db.Submit(req); err != nil {
		t.Fatalf("ordinary subsequent submission: %v", err)
	}
	archived, _ := db.CreateTask(TaskInput{Repo: "/repo", Text: "change", IssueRef: "42"})
	if err := db.ArchiveTask(archived); err != nil {
		t.Fatal(err)
	}
	req.TaskID = archived
	if _, err := db.StartTask(req); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("archived start: %v", err)
	}
}

func TestTaskEditRacesStart(t *testing.T) {
	root := t.TempDir()
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	id, err := db.CreateTask(TaskInput{Repo: "/repo", Text: "before"})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() {
		results <- other.EditTask(id, TaskInput{Repo: "/repo", Text: "after"})
	}()
	go func() {
		_, err := db.StartTask(Request{Workflow: "build", TaskID: id, Repo: "/repo", Request: "before"})
		results <- err
	}()
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrTaskConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins = %d, conflicts = %d", wins, conflicts)
	}
	task, _ := db.GetTask(id)
	runs, _ := db.TaskRuns(id)
	if len(runs) == 1 && task.Text != "before" || len(runs) == 0 && task.Text != "after" {
		t.Fatalf("task and run disagree: %+v, %+v", task, runs)
	}
}

func TestSubmitTasks(t *testing.T) {
	db := open(t)
	id, err := db.Submit(Request{Workflow: "build", Repo: "/repo", Request: "change", Spec: issueSpec("42")})
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.Get(id)
	if err != nil || row.TaskID == nil {
		t.Fatalf("run = %+v, %v", row, err)
	}
	task, err := db.GetTask(*row.TaskID)
	if err != nil || task.Text != "change" || task.Repo != "/repo" || task.IssueRef == nil || *task.IssueRef != "42" || *task.Tracker != "github" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	attached, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	row, _ = db.Get(attached)
	if row.TaskID == nil || *row.TaskID != task.ID {
		t.Fatalf("attached = %+v", row)
	}
	for _, req := range []Request{
		{Workflow: "build", Repo: "/repo", TaskID: "missing"},
		{Workflow: "build", Repo: "/repo/", TaskID: task.ID},
	} {
		if _, err := db.Submit(req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	if err := db.ArchiveTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: task.ID}); err == nil {
		t.Fatal("accepted archived task")
	}
	if err := db.UnarchiveTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: task.ID}); err != nil {
		t.Fatalf("submit after unarchive: %v", err)
	}
	for _, workflow := range []string{"scout", "plan", "implement"} {
		id, err := db.Submit(Request{Workflow: workflow, Repo: "/repo", TaskID: "missing", Spec: issueSpec("42")})
		if err != nil {
			t.Fatal(err)
		}
		row, _ := db.Get(id)
		if row.TaskID != nil {
			t.Fatalf("non-build has task: %+v", row)
		}
	}
	if tasks, _ := db.Tasks(""); len(tasks) != 1 {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}
}

func TestReviseArchivedTask(t *testing.T) {
	db := open(t)
	id := submit(t, db, "build", "/repo")
	attempt, token := dispatch(t, db, id)
	if err := db.Complete(id, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	row, err := db.Get(id)
	if err != nil || row.TaskID == nil {
		t.Fatalf("run = %+v, %v", row, err)
	}
	if err := db.ArchiveTask(*row.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Revise(id, 0, "revise archived", nil); !errors.Is(err, ErrNotRevisable) {
		t.Fatalf("revision after archive = %v", err)
	}
	row, err = db.Get(id)
	if err != nil || row.Iteration != 0 || row.Status != StatusOK {
		t.Fatalf("refused revision changed run: %+v, %v", row, err)
	}
	assertCounts(t, db, 1)
	if err := db.UnarchiveTask(*row.TaskID); err != nil {
		t.Fatal(err)
	}
	if n, err := db.Revise(id, 0, "revise restored", nil); err != nil || n != 1 {
		t.Fatalf("revision after unarchive = %d, %v", n, err)
	}
}

func TestReviseTasklessRun(t *testing.T) {
	db := open(t)
	id := submit(t, db, "plan", "/repo")
	attempt, token := dispatch(t, db, id)
	if err := db.Complete(id, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	row, err := db.Get(id)
	if err != nil || row.TaskID != nil {
		t.Fatalf("taskless run = %+v, %v", row, err)
	}
	if n, err := db.Revise(id, 0, "revise taskless", nil); err != nil || n != 1 {
		t.Fatalf("taskless revision = %d, %v", n, err)
	}
}

func TestSubmitTaskIssueConflict(t *testing.T) {
	for _, recorded := range []string{"", "42", "https://github.com/o/r/issues/42"} {
		t.Run(recorded, func(t *testing.T) {
			db := open(t)
			id, err := db.Submit(Request{Workflow: "build", Repo: "/repo", Spec: issueSpec(recorded)})
			if err != nil {
				t.Fatal(err)
			}
			row, _ := db.Get(id)
			if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: *row.TaskID, Spec: issueSpec("43")}); err == nil {
				t.Fatal("accepted conflicting issue")
			}
			assertCounts(t, db, 1)
			if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: *row.TaskID, Spec: issueSpec(recorded)}); err != nil {
				t.Fatalf("matching issue refused: %v", err)
			}
		})
	}
}

func TestSubmitTaskRollback(t *testing.T) {
	db := open(t)
	if _, err := db.sql.Exec(`CREATE TRIGGER refuse_iteration BEFORE INSERT ON iterations BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo"}); err == nil {
		t.Fatal("submission succeeded")
	}
	assertCounts(t, db, 0)
}

// assertCounts fails unless tasks, runs and iterations each hold want rows.
func assertCounts(t *testing.T, db *DB, want int) {
	t.Helper()
	for _, table := range []string{"tasks", "runs", "iterations"} {
		if got := one[int](t, db, `SELECT count(*) FROM `+table); got != want {
			t.Fatalf("%s count = %d, want %d", table, got, want)
		}
	}
}

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// submit records a run the way the CLI does.
func submit(t *testing.T, db *DB, workflow, repo string) string {
	t.Helper()
	id, err := db.Submit(Request{
		Workflow: workflow, Repo: repo, Request: "do a thing", Branch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// dispatch takes a queued run to running the way the manager and a worker do.
func dispatch(t *testing.T, db *DB, runID string) (attempt, token string) {
	t.Helper()
	attempt = NewAttemptID(runID)
	if err := db.Reserve(runID, attempt, "/tmp/log"); err != nil {
		t.Fatal(err)
	}
	token = NewClaimToken()
	if _, err := db.Claim(attempt, token, "host", 4242, time.Minute); err != nil {
		t.Fatal(err)
	}
	return attempt, token
}

// Writing runs execute in worktrees of their own, so several may be
// outstanding for one repository: queued alongside each other, alongside one
// that is running, and after it finishes. Reads and plans are accepted too.
func TestConcurrentWritingRunsAccepted(t *testing.T) {
	for _, first := range []string{"build", "implement"} {
		t.Run(first, func(t *testing.T) {
			db := open(t)
			id := submit(t, db, first, "/repo")

			for _, second := range []string{"build", "implement", "scout", "plan"} {
				if _, err := trySubmit(db, second, "/repo"); err != nil {
					t.Fatalf("queued %s alongside %s: %v", second, first, err)
				}
			}

			attempt, token := dispatch(t, db, id)
			if _, err := trySubmit(db, "implement", "/repo"); err != nil {
				t.Fatalf("a second writing run while the first runs: %v", err)
			}
			// The first is now running; the four queued beside it and the one
			// queued while it runs all wait together.
			if queued, _ := db.Queued(); len(queued) != 5 {
				t.Fatalf("queued = %d runs; want 5", len(queued))
			}
			if active, _ := db.Active(); len(active) != 1 || active[0].ID != id {
				t.Fatalf("active = %+v; want only %s", active, id)
			}
			if err := db.Complete(id, attempt, token, StatusOK, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := trySubmit(db, "build", "/repo"); err != nil {
				t.Fatalf("a build after the first finished: %v", err)
			}
		})
	}
}

func trySubmit(db *DB, workflow, repo string) (string, error) {
	return db.Submit(Request{Workflow: workflow, Repo: repo})
}

// Two workers launched for one attempt: exactly one claim succeeds, and the
// loser is told so before it does any work.
func TestCompetingClaims(t *testing.T) {
	db := open(t)
	id := submit(t, db, "scout", "/repo")
	attempt := NewAttemptID(id)
	if err := db.Reserve(id, attempt, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Claim(attempt, NewClaimToken(), "host", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Claim(attempt, NewClaimToken(), "host", 2, time.Minute); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("the duplicate launch claimed the attempt too: %v", err)
	}
	// And dispatching a run that is no longer queued is refused the same way,
	// which is what stops two managers double-dispatching.
	if err := db.Reserve(id, NewAttemptID(id), ""); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("a second dispatch was accepted: %v", err)
	}
}

// A worker whose claim is gone cannot write an outcome, and a terminal state
// is immutable.
func TestTerminalStatesAreImmutable(t *testing.T) {
	db := open(t)
	id := submit(t, db, "scout", "/repo")
	attempt, token := dispatch(t, db, id)

	if err := db.Complete(id, attempt, token, StatusOK, "done"); err != nil {
		t.Fatal(err)
	}
	if err := db.Complete(id, attempt, token, StatusFail, "late"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("a second completion was applied: %v", err)
	}
	if err := db.Complete(id, attempt, NewClaimToken(), StatusFail, "stale worker"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("a stale claim token wrote an outcome: %v", err)
	}
	row, err := db.Get(id)
	if err != nil || row.Status != StatusOK || row.Reason != "done" {
		t.Fatalf("run = %+v, %v", row, err)
	}
	if _, err := db.Beat(attempt, token, time.Minute); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("a settled attempt still accepted a heartbeat: %v", err)
	}
}

// The order of commits decides the cancellation race, in both directions.
func TestCancellationRaces(t *testing.T) {
	db := open(t)

	// Queued: cancelled outright, with no worker involved.
	queued := submit(t, db, "scout", "/a")
	if status, err := db.RequestCancel(queued); err != nil || status != StatusCancelled {
		t.Fatalf("cancelling a queued run = %q, %v", status, err)
	}

	// Running: the request is pending, the worker sees it on its next
	// heartbeat, and it records cancelled.
	running := submit(t, db, "scout", "/b")
	attempt, token := dispatch(t, db, running)
	if status, err := db.RequestCancel(running); err != nil || status != StatusRunning {
		t.Fatalf("cancelling a running run = %q, %v", status, err)
	}
	cancel, err := db.Beat(attempt, token, time.Minute)
	if err != nil || !cancel {
		t.Fatalf("the heartbeat did not report the cancellation: %v, %v", cancel, err)
	}
	if err := db.Complete(running, attempt, token, StatusCancelled, "cancelled"); err != nil {
		t.Fatal(err)
	}

	// Completion first: the cancellation loses and says so.
	done := submit(t, db, "scout", "/c")
	attempt, token = dispatch(t, db, done)
	if err := db.Complete(done, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RequestCancel(done); !errors.Is(err, ErrAlreadyDone) {
		t.Fatalf("cancelling a finished run: %v; want ErrAlreadyDone", err)
	}
	row, _ := db.Get(done)
	if row.Status != StatusOK {
		t.Fatalf("a finished run was moved to %q", row.Status)
	}
}

// A checkout is held by one run at a time, and a lost run keeps holding it:
// clearing a database record does not stop a process.
func TestCheckoutReservations(t *testing.T) {
	db := open(t)
	if err := db.ReserveCheckout("/repo", "host", "run1"); err != nil {
		t.Fatal(err)
	}
	if err := db.ReserveCheckout("/repo", "host", "run1"); err != nil {
		t.Fatalf("a run re-reserving its own checkout: %v", err)
	}
	if err := db.ReserveCheckout("/repo", "host", "run2"); err == nil {
		t.Fatal("two runs reserved one checkout")
	}
	held, err := db.HeldCheckouts()
	if err != nil || held["/repo"] != "run1" {
		t.Fatalf("held = %v, %v", held, err)
	}

	id := submit(t, db, "scout", "/repo")
	dispatch(t, db, id)
	if err := db.MarkLost(id, "the worker is gone"); err != nil {
		t.Fatal(err)
	}
	if held, _ := db.HeldCheckouts(); held["/repo"] != "run1" {
		t.Fatal("marking a run lost released a checkout a process may still be using")
	}
	if err := db.ReleaseCheckout("run1"); err != nil {
		t.Fatal(err)
	}
	if held, _ := db.HeldCheckouts(); len(held) != 0 {
		t.Fatalf("held after release = %v", held)
	}
}

// Queued and active runs are what the scheduler reads, and what `lathe runs`
// has to be able to show.
func TestQueuedAndActive(t *testing.T) {
	db := open(t)
	a := submit(t, db, "scout", "/a")
	b := submit(t, db, "plan", "/b")

	queued, err := db.Queued()
	// Both, oldest first — and within one second, by run ID, so the order is
	// deterministic rather than whatever SQLite felt like.
	if err != nil || len(queued) != 2 || queued[0].ID > queued[1].ID {
		t.Fatalf("queued = %+v, %v", queued, err)
	}
	if queued[0].Spec == nil || queued[0].Branch != "main" {
		t.Fatalf("the queue lost the specification or the branch: %+v", queued[0])
	}
	attempt, token := dispatch(t, db, a)
	active, err := db.Active()
	if err != nil || len(active) != 1 || active[0].ID != a {
		t.Fatalf("active = %+v, %v", active, err)
	}
	if active[0].Started == "" || active[0].Submitted == "" {
		t.Fatal("queue time and execution start are not both recorded")
	}
	if err := db.Complete(a, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	if active, _ := db.Active(); len(active) != 0 {
		t.Fatalf("a finished run still holds capacity: %+v", active)
	}
	if queued, _ := db.Queued(); len(queued) != 1 || queued[0].ID != b {
		t.Fatalf("queue after one dispatch = %+v", queued)
	}
}

// A transaction that reads and then writes, as RequestCancel does, must not
// fail because another process wrote in between. This is how `lathe cancel`
// lost to a run's own event writes.
func TestReadThenWriteSurvivesAnotherWriter(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	tx, err := a.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	other := make(chan error, 1)
	go func() {
		_, err := b.Submit(Request{Workflow: "scout", Repo: "/repo", Request: "meanwhile"})
		other <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := tx.Exec(`UPDATE runs SET reason = reason`); err != nil {
		t.Fatalf("write after read: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-other; err != nil {
		t.Fatalf("the other writer: %v", err)
	}
}
