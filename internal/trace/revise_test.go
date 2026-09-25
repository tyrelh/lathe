package trace

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// built is a build that ran and succeeded: a run a revision may extend.
func built(t *testing.T, db *DB, repo string) (id, attempt string) {
	t.Helper()
	id = submit(t, db, "build", repo)
	attempt, token := dispatch(t, db, id)
	if err := db.SetShipped(id, attempt, "feat/x", "c0"); err != nil {
		t.Fatal(err)
	}
	if err := db.Complete(id, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	return id, attempt
}

// at pins a run's latest activity, so ordering does not depend on two writes
// landing in different seconds.
func at(t *testing.T, db *DB, runID, stamp string) {
	t.Helper()
	if _, err := db.sql.Exec(`UPDATE runs SET activity_at = ? WHERE run_id = ?`, stamp, runID); err != nil {
		t.Fatal(err)
	}
}

// A revision is the same run again: it keeps its ID, its creation and first
// start, and is counted once, while each iteration keeps its own request,
// configuration, lifecycle and evidence.
func TestReviseExtendsTheRun(t *testing.T) {
	db := open(t)
	id, _ := built(t, db, "/repo")
	before, _ := db.Get(id)

	n, err := db.Revise(id, 0, "handle the empty result", []byte(`{"workflow":"revise"}`))
	if err != nil || n != 1 {
		t.Fatalf("revise = %d, %v", n, err)
	}
	row, _ := db.Get(id)
	if row.Status != StatusQueued || row.Iteration != 1 || string(row.Spec) != `{"workflow":"revise"}` ||
		row.Ended != "" || row.Reason != "" || row.Activity == "" {
		t.Fatalf("revised run = %+v", row)
	}
	if row.Submitted != before.Submitted || row.Started != before.Started || row.Request != before.Request ||
		row.Branch != "feat/x" || row.Commit != "c0" {
		t.Fatalf("the run lost what it recorded: %+v, was %+v", row, before)
	}
	if q, _ := db.Queued(); len(q) != 1 || q[0].ID != id {
		t.Fatalf("queued = %+v", q)
	}

	attempt, token := dispatch(t, db, id)
	if err := db.SetCommit(id, attempt, "c0"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetEvidence(id, attempt, "c1", "c0"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetShipped(id, attempt, "feat/x", "c1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Complete(id, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	row, _ = db.Get(id)
	if row.Started != before.Started || row.Commit != "c1" || row.Status != StatusOK || row.Ended == "" {
		t.Fatalf("after the revision: %+v", row)
	}
	its, err := db.Iterations(id)
	if err != nil || len(its) != 2 {
		t.Fatalf("iterations = %+v, %v", its, err)
	}
	if its[0].Status != StatusOK || its[0].Request != "do a thing" || its[0].Commit != "c0" || its[0].Ended == "" {
		t.Errorf("iteration 0 = %+v", its[0])
	}
	if its[1].Status != StatusOK || its[1].Request != "handle the empty result" || its[1].Base != "c0" ||
		its[1].Commit != "c1" || its[1].Started == "" || string(its[1].Spec) != `{"workflow":"revise"}` {
		t.Errorf("iteration 1 = %+v", its[1])
	}
	if p, err := db.Project("/repo"); err != nil || p.Runs != 1 {
		t.Fatalf("project counts the revised run %d times: %v", p.Runs, err)
	}
}

// A refusal changes nothing, and says why.
func TestReviseRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(t *testing.T, db *DB) (id string, expect int)
	}{
		{"queued", "still queued", func(t *testing.T, db *DB) (string, int) {
			return submit(t, db, "build", "/r"), 0
		}},
		{"running", "still running", func(t *testing.T, db *DB) (string, int) {
			id := submit(t, db, "build", "/r")
			dispatch(t, db, id)
			return id, 0
		}},
		{"failed", "ended fail", func(t *testing.T, db *DB) (string, int) {
			id := submit(t, db, "build", "/r")
			a, tok := dispatch(t, db, id)
			db.Complete(id, a, tok, StatusFail, "boom")
			return id, 0
		}},
		{"cancelled", "ended cancelled", func(t *testing.T, db *DB) (string, int) {
			id := submit(t, db, "build", "/r")
			db.RequestCancel(id)
			return id, 0
		}},
		{"lost", "ended lost", func(t *testing.T, db *DB) (string, int) {
			id := submit(t, db, "build", "/r")
			dispatch(t, db, id)
			db.MarkLost(id, "gone")
			return id, 0
		}},
		{"a failed revision after a good build", "ended fail", func(t *testing.T, db *DB) (string, int) {
			id, _ := built(t, db, "/r")
			db.Revise(id, 0, "more", nil)
			a, tok := dispatch(t, db, id)
			db.Complete(id, a, tok, StatusFail, "red")
			return id, 1
		}},
		{"stale check", "iteration 1 was submitted after", func(t *testing.T, db *DB) (string, int) {
			id, _ := built(t, db, "/r")
			db.Revise(id, 0, "more", nil)
			a, tok := dispatch(t, db, id)
			db.Complete(id, a, tok, StatusOK, "")
			return id, 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := open(t)
			id, expect := tc.setup(t, db)
			before, _ := db.Get(id)
			itsBefore, _ := db.Iterations(id)
			_, err := db.Revise(id, expect, "again", nil)
			if !errors.Is(err, ErrNotRevisable) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want ErrNotRevisable mentioning %q", err, tc.want)
			}
			after, _ := db.Get(id)
			itsAfter, _ := db.Iterations(id)
			if fmt.Sprint(before) != fmt.Sprint(after) || len(itsBefore) != len(itsAfter) {
				t.Fatalf("a refused revision changed the run: %+v -> %+v", before, after)
			}
		})
	}

	// Another run holding the checkout blocks the revision, as it blocks a build.
	db := open(t)
	id, _ := built(t, db, "/shared")
	other := submit(t, db, "implement", "/shared")
	if _, err := db.Revise(id, 0, "again", nil); !errors.Is(err, ErrDuplicateBuild) || !strings.Contains(err.Error(), other) {
		t.Fatalf("err = %v; want ErrDuplicateBuild naming %s", err, other)
	}
	// And once accepted, the revision holds it against a new build.
	db.RequestCancel(other)
	if _, err := db.Revise(id, 0, "again", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := trySubmit(db, "build", "/shared"); !errors.Is(err, ErrDuplicateBuild) {
		t.Fatalf("a build was accepted into a checkout a queued revision owns: %v", err)
	}
}

// Two submitters racing to revise one run from separate connections: exactly
// one is accepted, and the run gains exactly one iteration.
func TestConcurrentRevisionsAcceptOne(t *testing.T) {
	root := t.TempDir()
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, _ := built(t, db, "/repo")

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := Open(root)
			if err != nil {
				t.Error(err)
				return
			}
			defer other.Close()
			if _, err := other.Revise(id, 0, fmt.Sprint("revision ", i), nil); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	its, _ := db.Iterations(id)
	if accepted != 1 || len(its) != 2 {
		t.Fatalf("%d revisions accepted, %d iterations; want exactly one", accepted, len(its))
	}
}

// A worker from an earlier iteration cannot write into a later one: not its
// phases, not the run's shipped branch and commit, not the iteration's
// evidence, and not its outcome.
func TestStaleWorkerCannotAlterALaterIteration(t *testing.T) {
	db := open(t)
	id, stale := built(t, db, "/repo")
	if _, err := db.Revise(id, 0, "more", nil); err != nil {
		t.Fatal(err)
	}
	current, token := dispatch(t, db, id)

	p := NewPhase(id, 9, "plan", "planner")
	p.Iteration, p.Attempt = 1, stale
	if err := db.PhaseUpsert(p); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale phase write: %v", err)
	}
	for name, write := range map[string]func() error{
		"shipped":  func() error { return db.SetShipped(id, stale, "evil", "bad") },
		"evidence": func() error { return db.SetEvidence(id, stale, "bad", "bad") },
		"commit":   func() error { return db.SetCommit(id, stale, "bad") },
		"complete": func() error { return db.Complete(id, stale, "whatever", StatusFail, "stale") },
	} {
		if err := write(); !errors.Is(err, ErrClaimLost) {
			t.Errorf("stale %s: %v", name, err)
		}
	}
	row, _ := db.Get(id)
	its, _ := db.Iterations(id)
	if row.Branch != "feat/x" || row.Commit != "c0" || row.Status != StatusRunning || its[1].Commit != "" || its[0].Commit != "c0" {
		t.Fatalf("a stale worker changed the run: %+v %+v", row, its)
	}
	if phases, _ := db.Phases(id); len(phases) != 0 {
		t.Fatalf("stale phase recorded: %+v", phases)
	}

	p.Attempt = current
	if err := db.PhaseUpsert(p); err != nil {
		t.Fatal(err)
	}
	if err := db.Complete(id, current, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	// Once settled, not even the current attempt writes.
	p.Status = "success"
	if err := db.PhaseUpsert(p); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("write after settlement: %v", err)
	}
	if n, _ := db.MaxSeq(id); n != 9 {
		t.Fatalf("max seq = %d", n)
	}
}

// Queue order and list order both follow the latest submission, while the
// original creation time stays put.
func TestRevisionsQueueAndListByLatestActivity(t *testing.T) {
	db := open(t)
	old, _ := built(t, db, "/a")
	newer := submit(t, db, "scout", "/b")
	at(t, db, old, "2026-09-24T10:00:00Z")
	at(t, db, newer, "2026-09-24T11:00:00Z")
	if rows, _ := db.Recent(2); rows[0].ID != newer {
		t.Fatalf("recent before the revision = %s first", rows[0].ID)
	}
	if _, err := db.Revise(old, 0, "more", nil); err != nil {
		t.Fatal(err)
	}
	if q, _ := db.Queued(); len(q) != 2 || q[0].ID != newer || q[1].ID != old {
		t.Fatalf("queue = %v; the revision was submitted last and waits its turn", ids(q))
	}
	rows, _ := db.Recent(2)
	if rows[0].ID != old || rows[0].Submitted > rows[1].Submitted {
		t.Fatalf("recent = %v; a revised run moves to the top and keeps its creation time", ids(rows))
	}
}

func ids(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// A database from before iterations gets each run as its own iteration 0,
// from exactly what the row recorded: no base or commit is filled in.
func TestMigrationRecordsExistingRunsAsTheirFirstIteration(t *testing.T) {
	root := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(root, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range append([]string{schema}, migrations[:3]...) {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO runs (run_id, workflow, repo, request, status, reason, branch, commit_sha, spec,
		submitted_at, started_at, ended_at, tokens, cost)
		VALUES ('old', 'build', '/repo', 'add retry', 'ok', '', 'feat/retry', 'abc123', '{}',
		'2026-09-01T10:00:00Z', '2026-09-01T10:00:05Z', '2026-09-01T10:09:00Z', 900, 1.5)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row, _ := db.Get("old")
	if row.Iteration != 0 || row.Activity != "2026-09-01T10:00:00Z" || row.Commit != "abc123" {
		t.Fatalf("migrated run = %+v", row)
	}
	its, err := db.Iterations("old")
	if err != nil || len(its) != 1 {
		t.Fatalf("iterations = %+v, %v", its, err)
	}
	it := its[0]
	if it.Request != "add retry" || it.Status != StatusOK || it.Submitted != row.Submitted ||
		it.Started != row.Started || it.Ended != row.Ended || it.Base != "" || it.Commit != "" {
		t.Fatalf("iteration 0 = %+v", it)
	}
	if _, err := db.Revise("old", 0, "and log it", nil); err != nil {
		t.Fatalf("a migrated run cannot be revised: %v", err)
	}
}
