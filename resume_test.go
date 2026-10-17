package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workspace"
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
	finish(t, db, ok, trace.StatusOK)
	for name, tc := range map[string]struct {
		id, guidance   string
		o              worker.ResumeOptions
		notRecoverable bool
	}{
		"no guidance": {ok, "  ", worker.ResumeOptions{}, false},
		"active":      {active, "go on", worker.ResumeOptions{}, true},
		"succeeded":   {ok, "go on", worker.ResumeOptions{}, true},
		"stray base":  {ok, "go on", worker.ResumeOptions{Base: "main"}, false},
		"unknown run": {"build_nope", "go on", worker.ResumeOptions{}, false},
	} {
		_, err := submitRecovery(db, dataRoot, tc.id, tc.guidance, tc.o, io.Discard)
		if err == nil || errors.Is(err, trace.ErrNotRecoverable) != tc.notRecoverable {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, id := range []string{active, ok} {
		if its, _ := db.Iterations(id); len(its) != 1 {
			t.Fatalf("%s: a refusal appended an iteration", id)
		}
	}
}

// show refuses recovery for the reasons resume would, from the run's record,
// and offers it otherwise.
func TestShowRecoveryMatchesResume(t *testing.T) {
	dataRoot := t.TempDir()
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	failed := func(s worker.Spec) string {
		s.Version, s.Workflow, s.Request = worker.SpecVersion, "scout", "look"
		spec, err := s.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		id, err := db.Submit(trace.Request{Workflow: "scout", Repo: "/r", Request: "look", Branch: "main", Spec: spec})
		if err != nil {
			t.Fatal(err)
		}
		finish(t, db, id, trace.StatusFail)
		return id
	}
	here := workspace.Desc{Kind: workspace.KindLocalPath, Path: "/r", Host: host}
	elsewhere := workspace.Desc{Kind: workspace.KindLocalPath, Path: "/r", Host: host + "-elsewhere"}
	for name, tc := range map[string]struct {
		id, want string
	}{
		"eligible":         {failed(worker.Spec{Workspace: here}), "recovery: may be resumed; lathe resume "},
		"foreign host":     {failed(worker.Spec{Workspace: elsewhere}), "recovery: workspace /r belongs to host"},
		"unresolved issue": {failed(worker.Spec{Workspace: here, Issue: "o/r#1"}), "recovery: this run cannot be resumed: iteration 0 never resolved issue"},
	} {
		row, err := db.Get(tc.id)
		if err != nil {
			t.Fatal(err)
		}
		its, err := db.Iterations(tc.id)
		if err != nil {
			t.Fatal(err)
		}
		lines := recoveryLines(db, row, its, dataRoot, row.Iteration)
		if len(lines) == 0 || !strings.HasPrefix(lines[len(lines)-1], tc.want) {
			t.Errorf("%s: %q; want a line starting %q", name, lines, tc.want)
		}
	}
}
