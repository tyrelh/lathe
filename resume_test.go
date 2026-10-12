package main

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
)

// The CLI and dashboard submission refuses missing guidance, active and
// successful runs, stray --base and unknown runs, and a refusal appends no
// iteration.
func TestSubmitRecoveryRefusals(t *testing.T) {
	dataRoot := t.TempDir()
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	submit := func() string {
		id, err := db.Submit(trace.Request{Workflow: "implement", Repo: "/repo", Request: "greet", Spec: []byte(`{"workflow":"implement"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	active := submit()
	ok := submit()
	attempt, token := trace.NewAttemptID(ok), trace.NewClaimToken()
	if err := db.Reserve(ok, attempt, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Claim(attempt, token, "host", os.Getpid(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.Complete(ok, attempt, token, trace.StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		id, guidance string
		o            worker.ResumeOptions
		recoverable  bool
	}{
		"no guidance": {ok, "  ", worker.ResumeOptions{}, false},
		"active":      {active, "go on", worker.ResumeOptions{}, true},
		"succeeded":   {ok, "go on", worker.ResumeOptions{}, true},
		"stray base":  {ok, "go on", worker.ResumeOptions{Base: "main"}, false},
		"unknown run": {"build_nope", "go on", worker.ResumeOptions{}, false},
	} {
		_, err := submitRecovery(db, dataRoot, tc.id, tc.guidance, tc.o, io.Discard)
		if err == nil || errors.Is(err, trace.ErrNotRecoverable) != tc.recoverable {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, id := range []string{active, ok} {
		if its, _ := db.Iterations(id); len(its) != 1 {
			t.Fatalf("%s: a refusal appended an iteration", id)
		}
	}
}
