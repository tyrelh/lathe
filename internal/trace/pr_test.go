package trace

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPRCandidatesMinimalRows(t *testing.T) {
	db := open(t)
	if _, err := db.sql.Exec(`INSERT INTO runs (run_id, workflow, repo, iteration, spec, status) VALUES ('old', 'build', '/repo', 2, 'large snapshot', 'ok')`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.PRCandidates()
	if err != nil || len(rows) != 1 {
		t.Fatalf("candidates: %+v, %v", rows, err)
	}
	row := rows[0]
	if row.ID != "old" || row.Repo != "/repo" || row.Iteration != 2 || row.Spec != nil || row.Status != "" {
		t.Fatalf("candidate loaded unexpected fields: %+v", row)
	}
	// A write can use the single connection after the query returns.
	if err := db.ObservePR(row.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DROP TABLE runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PRCandidates(); err == nil {
		t.Fatal("query failure was lost")
	}
}

func TestPRObservations(t *testing.T) {
	db := open(t)
	for _, final := range []string{"merged", "closed"} {
		id, err := db.Submit(Request{Workflow: "build", Repo: "/repo", Request: "x"})
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.Get(id)
		if err != nil || row.PRState != nil || row.PRStateAt != nil {
			t.Fatalf("initial row: %+v, %v", row, err)
		}
		b, _ := json.Marshal(row)
		if !strings.Contains(string(b), `"pr_state":null`) {
			t.Fatalf("missing JSON null: %s", b)
		}
		if err := db.ObservePR(id, "OPEN"); err == nil {
			t.Fatal("accepted invalid state")
		}
		for range 2 {
			if err := db.ObservePR(id, "open"); err != nil {
				t.Fatal(err)
			}
			row, _ = db.Get(id)
			if row.PRState == nil || *row.PRState != "open" || row.PRStateAt == nil {
				t.Fatalf("open: %+v", row)
			}
		}
		if err := db.ObservePR(id, final); err != nil {
			t.Fatal(err)
		}
		row, _ = db.Get(id)
		at := *row.PRStateAt
		for _, state := range []string{"open", "merged", "closed"} {
			if err := db.ObservePR(id, state); err != nil {
				t.Fatal(err)
			}
		}
		row, _ = db.Get(id)
		if *row.PRState != final || *row.PRStateAt != at {
			t.Fatalf("final changed: %+v", row)
		}
	}
	rows, err := db.PRCandidates()
	if err != nil || len(rows) != 0 {
		t.Fatalf("final candidates: %+v, %v", rows, err)
	}
}
