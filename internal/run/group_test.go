package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every worker is inside its body at once before any is let out, which only
// happens if they run concurrently; done sees them all finished.
func TestGroupOverlapsWorkersAndJoinsBeforeDone(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(true, "")
	const n = 4
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	go func() {
		arrived.Wait()
		close(release)
	}()
	var finished atomic.Int32
	var workers []Worker
	for _, name := range []string{"a", "b", "c", "d"} {
		workers = append(workers, Worker{Name: name, Owner: "engineer", Run: func(e *Entry) error {
			arrived.Done()
			select {
			case <-release:
			case <-time.After(5 * time.Second):
				return errors.New("workers ran one at a time")
			}
			finished.Add(1)
			return nil
		}})
	}
	var joined int32
	g := NewGraph(r)
	g.AddGroup(Node{Name: "validate", Owner: "engineer"}, workers, func(e *Entry, res GroupResult) (string, error) {
		joined = finished.Load()
		if len(res.Failed) != 0 {
			t.Errorf("failed: %v", res.Failed)
		}
		return "", nil
	})
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if joined != n {
		t.Fatalf("done ran with %d of %d workers finished", joined, n)
	}
	// One phase per worker plus the group's own, each attributed separately.
	if got := scalar[int](t, readDB(t), "SELECT count(DISTINCT phase_id) FROM phases WHERE status = 'success'"); got != n+1 {
		t.Fatalf("successful phases: %d", got)
	}
}

// A failed attempt is retried against the same entry without touching its
// peers, stays in the trace as a failed phase, and does not fail the run once
// a retry recovers it. A worker that never recovers stops at its bound.
func TestGroupRetriesAFailedWorker(t *testing.T) {
	r := newRun(t, "")
	var flaky, broken, steady atomic.Int32
	var sessions []string
	g := NewGraph(r)
	g.AddGroup(Node{Name: "validate", Owner: "engineer"}, []Worker{
		{Name: "flaky", Owner: "engineer", Run: func(e *Entry) error {
			sessions = append(sessions, e.SessionID())
			if flaky.Add(1) < 3 {
				return errors.New("garbled")
			}
			return nil
		}},
		{Name: "broken", Owner: "engineer", Run: func(e *Entry) error {
			broken.Add(1)
			return errors.New("always garbled")
		}},
		{Name: "steady", Owner: "engineer", Run: func(e *Entry) error {
			steady.Add(1)
			return nil
		}},
	}, func(e *Entry, res GroupResult) (string, error) {
		if names := res.FailedNames(); len(names) != 1 || names[0] != "broken" {
			t.Errorf("failed: %v", names)
		}
		return "", nil
	})
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if flaky.Load() != 3 || broken.Load() != 1+WorkerRetries || steady.Load() != 1 {
		t.Fatalf("attempts: flaky %d, broken %d, steady %d", flaky.Load(), broken.Load(), steady.Load())
	}
	if sessions[0] == sessions[1] || sessions[1] == sessions[2] {
		t.Fatalf("a retry reused the failed session: %v", sessions)
	}
	db := readDB(t)
	if got := scalar[string](t, db, "SELECT group_concat(status) FROM (SELECT status FROM phases WHERE name = 'flaky' ORDER BY seq)"); got != "fail,fail,success" {
		t.Fatalf("flaky phases: %s", got)
	}
	if code := r.Finish(true, ""); code != 0 || r.Status() != "ok" {
		t.Fatalf("a recovered worker left the run %s: %s", r.Status(), r.Reason())
	}
}

// Cancellation stops every worker, is not retried, and never reaches done.
func TestGroupCancellationStopsWorkers(t *testing.T) {
	r := newRun(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	var attempts atomic.Int32
	worker := func(name string) Worker {
		return Worker{Name: name, Owner: "engineer", Run: func(e *Entry) error {
			attempts.Add(1)
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}}
	}
	g := NewGraph(r)
	g.AddGroup(Node{Name: "validate", Owner: "engineer"}, []Worker{worker("a"), worker("b")},
		func(e *Entry, res GroupResult) (string, error) {
			t.Error("done ran after cancellation")
			return "", nil
		})
	if err := g.Run(); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d; cancellation must not be retried", attempts.Load())
	}
	if code := r.Finish(true, ""); code != 1 {
		t.Fatal("a cancelled group left the run passing")
	}
}

// Source changed while workers ran is reported before the sweep cleans up, and
// cleanup waits for every worker: a file one worker is using survives another
// worker finishing first.
func TestGroupDetectsMutationsAndDefersCleanup(t *testing.T) {
	r := newRun(t, "")
	defer r.Finish(true, "")
	r.Repo = gitRepo(t)
	if err := r.EnsureClean(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.accepted = []string{"README.md"}

	output := filepath.Join(r.Repo, "coverage.out")
	quickDone := make(chan struct{})
	var survived bool
	g := NewGraph(r)
	g.AddGroup(Node{Name: "validate", Owner: "engineer"}, []Worker{
		{Name: "slow", Owner: "engineer", Run: func(e *Entry) error {
			if err := os.WriteFile(output, []byte("x"), 0o644); err != nil {
				return err
			}
			<-quickDone
			// The quick worker's turn has ended; its cleanup must not have run.
			_, err := os.Stat(output)
			survived = err == nil
			return os.WriteFile(filepath.Join(r.Repo, "README.md"), []byte("rewritten by a test\n"), 0o644)
		}},
		{Name: "quick", Owner: "engineer", Run: func(e *Entry) error {
			// A phase ending runs enforcement; frozen, it must leave the tree alone.
			_, err := r.enforce(e.Handle)
			close(quickDone)
			return err
		}},
	}, func(e *Entry, res GroupResult) (string, error) {
		if strings.Join(res.Mutated, ",") != "README.md" {
			t.Errorf("mutated = %v", res.Mutated)
		}
		return "", nil
	})
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if !survived {
		t.Fatal("cleanup ran while a worker was still using the tree")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("test output survived the sweep after the join")
	}
}
