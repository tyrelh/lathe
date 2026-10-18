package trace

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestDatabasePathsWithURICharactersAreIsolated(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"store#first", "store#second", "store?third", "store%fourth"} {
		root := filepath.Join(parent, name)
		db, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		id := submit(t, db, "build", "/repo")
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		for _, opener := range []func(string) (*DB, error){Open, OpenRO} {
			db, err := opener(root)
			if err != nil {
				t.Fatal(err)
			}
			tasks, taskErr := db.Tasks("")
			runs, runErr := db.Recent(10)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if taskErr != nil || len(tasks) != 1 || runErr != nil || len(runs) != 1 || runs[0].ID != id {
				t.Fatalf("database %q is not isolated: tasks=%+v (%v), runs=%+v (%v)", root, tasks, taskErr, runs, runErr)
			}
		}
	}
}

func TestRequestedChangesPhasePersists(t *testing.T) {
	db := open(t)
	id := submit(t, db, "build", "/repo")
	p := NewPhase(id, 1, "review", "plan-reviewer")
	p.Finish(PhaseRequestedChanges, "")
	if err := db.PhaseUpsert(p); err != nil {
		t.Fatal(err)
	}
	phases, err := db.Phases(id)
	if err != nil || len(phases) != 1 {
		t.Fatalf("phases = %+v, %v", phases, err)
	}
	if got := phases[0]; got.Status != PhaseRequestedChanges || got.Error != "" || got.End == "" {
		t.Fatalf("phase = %+v", got)
	}
}

func TestRunReadersPreserveNullableTaskID(t *testing.T) {
	db := open(t)
	build := submit(t, db, "build", "/repo")
	plan := submit(t, db, "plan", "/repo")
	legacy := submit(t, db, "build", "/repo")
	if _, err := db.sql.Exec(`UPDATE runs SET task_id = NULL WHERE run_id = ?`, legacy); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Recent(10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("recent = %+v, %v", rows, err)
	}
	for _, row := range rows {
		if (row.ID == build) != (row.TaskID != nil) {
			t.Fatalf("task association = %+v", row)
		}
	}
	for _, id := range []string{plan, legacy} {
		row, err := db.Get(id)
		if err != nil || row.TaskID != nil {
			t.Fatalf("taskless = %+v, %v", row, err)
		}
	}
	queued, err := db.Queued()
	if err != nil || len(queued) != 3 {
		t.Fatalf("queued = %+v, %v", queued, err)
	}
}

// TestWriteFakeRun checks that a run with two phases and four events reads back
// in the order it was written.
func TestWriteFakeRun(t *testing.T) {
	root := t.TempDir()
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const runID = "20260916T120000Z_scout"
	seedRun(t, db, runID, "/Users/tyrel/Projects/lathe")

	for i, name := range []string{"request", "scout"} {
		p := NewPhase(runID, i, name, name)
		if err := db.PhaseUpsert(p); err != nil {
			t.Fatal(err)
		}
		if err := db.Event(runID, p.ID, "phase_start", name, nil); err != nil {
			t.Fatal(err)
		}
		if err := db.Event(runID, p.ID, "tool_call", "read", map[string]any{"path": "main.go"}); err != nil {
			t.Fatal(err)
		}
		p.Finish("success", "")
		if err := db.PhaseUpsert(p); err != nil {
			t.Fatal(err)
		}
	}
	// Spend reaches the run through the usage path now, one record per model
	// response; completion only settles the lifecycle.
	for _, u := range []Usage{
		{RunID: runID, PhaseID: runID + "_01_scout", Agent: "scout", Seq: 1, Provider: "moonshotai", Model: "kimi", Tokens: 4000, Cost: 0.0030},
		{RunID: runID, PhaseID: runID + "_01_scout", Agent: "scout", Seq: 2, Provider: "moonshotai", Model: "kimi", Tokens: 119, Cost: 0.0001},
	} {
		if err := db.RecordUsage(u); err != nil {
			t.Fatal(err)
		}
	}
	settle(t, db, runID, "ok")

	// Read through a second, read-only connection: what the dashboard will do.
	ro, err := sql.Open("sqlite", "file:"+filepath.Join(root, dbFile)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	var status string
	var tokens int
	var cost float64
	if err := ro.QueryRow(`SELECT status, tokens, cost FROM runs WHERE run_id = ?`, runID).Scan(&status, &tokens, &cost); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || tokens != 4119 || cost != 0.0031 {
		t.Fatalf("run = %q %d %v; want ok 4119 0.0031", status, tokens, cost)
	}

	// The second upsert must have updated the phase, not inserted a duplicate.
	var phases int
	if err := ro.QueryRow(`SELECT count(*) FROM phases WHERE run_id = ? AND status = 'success' AND ended_at IS NOT NULL`, runID).Scan(&phases); err != nil {
		t.Fatal(err)
	}
	if phases != 2 {
		t.Fatalf("finished phases = %d; want 2", phases)
	}

	rows, err := ro.Query(`SELECT type, name, payload FROM events WHERE run_id = ? ORDER BY event_id`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var typ, name string
		var payload sql.NullString
		if err := rows.Scan(&typ, &name, &payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, typ+"/"+name+"/"+payload.String)
	}
	want := []string{
		"phase_start/request/",
		`tool_call/read/{"path":"main.go"}`,
		"phase_start/scout/",
		`tool_call/read/{"path":"main.go"}`,
		`usage/scout/{"attempt":0,"cost":0.003,"model":"kimi","provider":"moonshotai","seq":1,"tokens":4000}`,
		`usage/scout/{"attempt":0,"cost":0.0001,"model":"kimi","provider":"moonshotai","seq":2,"tokens":119}`,
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q; want %q", i, got[i], want[i])
		}
	}
}

// Many connections and processes opening a fresh database at once all
// initialize it: switching to WAL and migrating wait out each other's locks
// instead of failing with "database is locked".
func TestConcurrentOpenInitializes(t *testing.T) {
	for round := range 3 {
		dataRoot := t.TempDir()
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				db, err := Open(dataRoot)
				if err != nil {
					errs <- fmt.Errorf("round %d connection %d: %w", round, i, err)
					return
				}
				if _, err := db.Submit(Request{Workflow: "scout", Repo: "/repo", Request: "look"}); err != nil {
					errs <- fmt.Errorf("round %d connection %d submit: %w", round, i, err)
				}
				db.Close()
			}()
		}
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cmd := exec.Command(os.Args[0], "-test.run=^TestOpenHelper$")
				cmd.Env = append(os.Environ(), "LATHE_TRACE_OPEN="+dataRoot)
				if out, err := cmd.CombinedOutput(); err != nil {
					errs <- fmt.Errorf("round %d process %d: %v: %s", round, i, err, out)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		db, err := Open(dataRoot)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.sql.QueryRow(`SELECT count(*) FROM runs`).Scan(&n); err != nil || n != 12 {
			t.Fatalf("round %d: %d runs recorded, %v; want 12", round, n, err)
		}
		db.Close()
	}
}

// TestOpenHelper is the separate process TestConcurrentOpenInitializes
// starts: it opens the database named by LATHE_TRACE_OPEN and records a run.
func TestOpenHelper(t *testing.T) {
	dataRoot := os.Getenv("LATHE_TRACE_OPEN")
	if dataRoot == "" {
		t.Skip("run by TestConcurrentOpenInitializes")
	}
	db, err := Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Submit(Request{Workflow: "scout", Repo: "/repo", Request: "look"}); err != nil {
		t.Fatal(err)
	}
}

// Only the driver's lock contention is retried: a busy error SQLite itself
// raised, wrapped or not, but neither another SQLite error nor an arbitrary
// error worded like contention.
func TestContendedIsOnlyLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "contended.db")
	holder, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.Exec("CREATE TABLE t (x)"); err != nil {
		t.Fatal(err)
	}
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	waiter, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	_, busy := waiter.Begin()
	if busy == nil {
		t.Fatal("a second writer began while the first held the lock")
	}
	_, missing := waiter.Exec("SELECT * FROM nope")
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"driver busy":         {busy, true},
		"wrapped driver busy": {fmt.Errorf("opening: %w", busy), true},
		"driver other error":  {missing, false},
		"lookalike text":      {fmt.Errorf("database is locked (261) SQLITE_BUSY"), false},
	} {
		if tc.err == nil {
			t.Fatalf("%s: no error produced", name)
		}
		if got := contended(tc.err); got != tc.want {
			t.Errorf("%s: contended(%v) = %v", name, tc.err, got)
		}
	}
}

// A phase belonging to no run is a bug in the caller, and the schema says so.
func TestPhaseRequiresItsRun(t *testing.T) {
	db := open(t)
	if err := db.PhaseUpsert(&Phase{ID: "p1", RunID: "nope", Name: "scout"}); err == nil {
		t.Fatal("accepted a phase with no run")
	}
}

func TestDataRootPrefersXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg")
	got, err := DataRoot()
	if err != nil || got != "/tmp/xdg/lathe" {
		t.Fatalf("DataRoot() = %q, %v", got, err)
	}
}

// RunIDSuffix recovers the random part however many underscores the workflow
// name holds.
func TestRunIDSuffix(t *testing.T) {
	id := NewRunID("my_flow")
	if s := RunIDSuffix(id); len(s) != 8 || id != id[:len(id)-len(s)-1]+"_"+s {
		t.Fatalf("RunIDSuffix(%q) = %q", id, s)
	}
}
