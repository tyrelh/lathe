package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
)

// finish takes a run's current iteration through dispatch to status.
func finish(t *testing.T, db *trace.DB, id, status string) {
	t.Helper()
	attempt, token := trace.NewAttemptID(id), trace.NewClaimToken()
	if err := db.Reserve(id, attempt, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Claim(attempt, token, "host", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.Complete(id, attempt, token, status, ""); err != nil {
		t.Fatal(err)
	}
}

// A wait for one iteration answers with that iteration's outcome, even when
// another has been submitted since and the run as a whole is live again.
func TestWaitTargetsItsOwnIteration(t *testing.T) {
	dataRoot := t.TempDir()
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.Submit(trace.Request{Workflow: "build", Repo: "/r", Request: "greet", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, db, id, trace.StatusOK)
	if _, err := db.Revise(id, 0, "louder", nil); err != nil {
		t.Fatal(err)
	}
	finish(t, db, id, trace.StatusOK)
	if _, err := db.Revise(id, 1, "louder still", nil); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() { done <- block(db, dataRoot, id, 1, true) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("iteration 1 = exit %d, want its own success", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting for iteration 1 waited on the queued iteration 2")
	}
	if _, err := db.RequestCancel(id); err != nil {
		t.Fatal(err)
	}
	if code := block(db, dataRoot, id, 2, true); code != 130 {
		t.Fatalf("iteration 2 = exit %d, want cancelled", code)
	}
	if code := block(db, dataRoot, id, -1, true); code != 130 {
		t.Fatalf("the run = exit %d, want its latest iteration's", code)
	}
	if code := block(db, dataRoot, id, 1, true); code != 0 {
		t.Fatalf("iteration 1 changed its outcome: exit %d", code)
	}
}

// Submission refuses what the records rule out before touching the checkout,
// and says why.
func TestSubmitRevisionRefusesFromTheRecords(t *testing.T) {
	dataRoot := t.TempDir()
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	scout, _ := db.Submit(trace.Request{Workflow: "scout", Repo: "/r", Request: "look", Branch: "main"})
	finish(t, db, scout, trace.StatusOK)
	failed, _ := db.Submit(trace.Request{Workflow: "build", Repo: "/s", Request: "x", Branch: "main"})
	finish(t, db, failed, trace.StatusFail)
	unshipped, _ := db.Submit(trace.Request{Workflow: "build", Repo: "/u", Request: "x", Branch: "main"})
	finish(t, db, unshipped, trace.StatusOK)

	for id, want := range map[string]string{
		scout:     "only a build opens a pull request",
		failed:    "ended fail",
		unshipped: "no recorded pull request",
		"nope":    "no run nope",
	} {
		_, err := submitRevision(db, dataRoot, id, "change it", config.Overrides{}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v; want %q", id, err, want)
		}
		if id != "nope" && !errors.Is(err, trace.ErrNotRevisable) {
			t.Errorf("%s: %v is not ErrNotRevisable", id, err)
		}
	}
	if _, err := submitRevision(db, dataRoot, failed, "  ", config.Overrides{}, io.Discard); err == nil {
		t.Error("an empty revision was accepted")
	}
	if its, _ := db.Iterations(failed); len(its) != 1 {
		t.Fatal("a refused revision was recorded")
	}
}
