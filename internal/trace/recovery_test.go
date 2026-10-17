package trace

import (
	"errors"
	"sync"
	"testing"
)

// failed is a run whose only iteration failed.
func failed(t *testing.T, db *DB, status string) string {
	t.Helper()
	id := submit(t, db, "implement", "/repo")
	attempt, token := dispatch(t, db, id)
	if status == StatusLost {
		if err := db.MarkLost(id, "worker gone"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if err := db.Complete(id, attempt, token, status, "it broke"); err != nil {
		t.Fatal(err)
	}
	return id
}

func recovery(source int) Recovery {
	return Recovery{Source: source, Kind: KindRecovery, Resolved: "greet", Guidance: "try again", Spec: []byte(`{"workflow":"implement"}`)}
}

// Failed, cancelled and lost iterations may be resumed; the run keeps its ID
// and history, and the new iteration records where it came from.
func TestRecoverAppendsAnIteration(t *testing.T) {
	for _, status := range []string{StatusFail, StatusCancelled, StatusLost} {
		t.Run(status, func(t *testing.T) {
			db := open(t)
			id := failed(t, db, status)
			n, err := db.Recover(id, recovery(0))
			if err != nil || n != 1 {
				t.Fatalf("recover = %d, %v", n, err)
			}
			row, _ := db.Get(id)
			if row.Status != StatusQueued || row.Iteration != 1 {
				t.Fatalf("row = %+v", row)
			}
			its, err := db.Iterations(id)
			if err != nil || len(its) != 2 || its[0].Status != status {
				t.Fatalf("history lost: %+v, %v", its, err)
			}
			if its[0].Kind != KindSubmission || its[0].Source != -1 || its[1].Kind != KindRecovery || its[1].Source != 0 ||
				its[1].Guidance != "try again" || its[1].Resolved != "greet" {
				t.Fatalf("provenance = %+v", its)
			}
		})
	}
}

// A successful or active run is not resumable, and a refusal appends nothing.
func TestRecoverRefusals(t *testing.T) {
	db := open(t)
	ok := submit(t, db, "implement", "/repo")
	attempt, token := dispatch(t, db, ok)
	if err := db.Complete(ok, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	active := submit(t, db, "implement", "/repo")
	for _, id := range []string{ok, active} {
		if _, err := db.Recover(id, recovery(0)); !errors.Is(err, ErrNotRecoverable) {
			t.Fatalf("%s: %v", id, err)
		}
		if its, _ := db.Iterations(id); len(its) != 1 {
			t.Fatalf("a refusal appended an iteration: %+v", its)
		}
	}
	// A stale source iteration is refused.
	id := failed(t, db, StatusFail)
	if _, err := db.Recover(id, recovery(3)); !errors.Is(err, ErrNotRecoverable) {
		t.Fatalf("stale source: %v", err)
	}
}

// Writers not confirmed stopped refuse recovery: an open launch, a held
// reservation, or a worktree not yet preserved and removed.
func TestRecoverRequiresStoppedWriters(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(db *DB, id string) error
	}{
		{"launch", func(db *DB, id string) error {
			_, err := db.sql.Exec(`INSERT INTO launches (launch_id, run_id, attempt_id, state) VALUES ('l1', ?, 'a', 'registered')`, id)
			return err
		}},
		{"reservation", func(db *DB, id string) error { return db.ReserveCheckout("/wt", "host", id) }},
		{"worktree", func(db *DB, id string) error {
			_, err := db.sql.Exec(`UPDATE iterations SET worktree_path = '/wt', worktree_state = 'pending' WHERE run_id = ?`, id)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := open(t)
			id := failed(t, db, StatusFail)
			if err := tc.set(db, id); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Recover(id, recovery(0)); !errors.Is(err, ErrNotRecoverable) {
				t.Fatalf("recover = %v", err)
			}
			if its, _ := db.Iterations(id); len(its) != 1 {
				t.Fatal("a refusal appended an iteration")
			}
		})
	}
}

// Concurrent submissions against the same source accept at most one.
func TestConcurrentRecoveriesAcceptOne(t *testing.T) {
	db := open(t)
	id := failed(t, db, StatusFail)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := db.Recover(id, recovery(0)); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if its, _ := db.Iterations(id); accepted != 1 || len(its) != 2 {
		t.Fatalf("accepted %d, iterations %d", accepted, len(its))
	}
}
