package trace

import (
	"errors"
	"testing"
	"time"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func submit(t *testing.T, db *DB, workflow, repo string) string {
	t.Helper()
	id, err := db.Submit(Request{Workflow: workflow, Repo: repo, Request: "do a thing", Branch: "main"})
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

// A second outstanding build for one checkout is refused while the first is
// queued, while it is running, and accepted once it is finished. Reads and
// plans are not builds and are never refused.
func TestDuplicateBuildRejection(t *testing.T) {
	db := open(t)
	first := submit(t, db, "build", "/repo")

	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo"}); !errors.Is(err, ErrDuplicateBuild) {
		t.Fatalf("second queued build: %v; want ErrDuplicateBuild", err)
	}
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/other"}); err != nil {
		t.Fatalf("a build in a different checkout was refused: %v", err)
	}
	for _, workflow := range []string{"scout", "plan"} {
		if _, err := db.Submit(Request{Workflow: workflow, Repo: "/repo"}); err != nil {
			t.Fatalf("%s alongside a build: %v", workflow, err)
		}
	}

	attempt, token := dispatch(t, db, first)
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo"}); !errors.Is(err, ErrDuplicateBuild) {
		t.Fatalf("second build while the first runs: %v; want ErrDuplicateBuild", err)
	}
	if err := db.Complete(first, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo"}); err != nil {
		t.Fatalf("a build after the first finished: %v", err)
	}
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
