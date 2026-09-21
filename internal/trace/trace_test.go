package trace

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestWriteFakeRun is Phase 2's done-when: a run with two phases and four
// events, read back in the order they were written.
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
		p := NewPhase(runID, i, name, "agent", name)
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

// A phase belonging to no run is a bug in the caller, and the schema says so.
func TestPhaseRequiresItsRun(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
