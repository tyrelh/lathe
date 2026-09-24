package trace

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestProjectsAndRunPages(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 12; i++ {
		repo := fmt.Sprintf("/one/p%02d", i)
		if i == 1 {
			repo = "/two/p00" // same basename as another project
		}
		id := fmt.Sprintf("r%02d", i)
		seedRun(t, db, id, repo)
		if _, err := db.sql.Exec(`UPDATE runs SET cost = ?, tokens = ? WHERE run_id = ?`, 12-i, (12-i)*10, id); err != nil {
			t.Fatal(err)
		}
	}
	seedRun(t, db, "zero", "/zero")
	seedRun(t, db, "empty", "")
	if _, err := db.sql.Exec(`UPDATE runs SET repo = NULL WHERE run_id = 'empty'`); err != nil {
		t.Fatal(err)
	}
	projects, err := db.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 14 || projects[0].Repo != "/one/p00" || projects[1].Repo != "/two/p00" {
		t.Fatalf("projects = %+v", projects)
	}
	if projects[len(projects)-1].Cost != 0 || projects[len(projects)-2].Cost != 0 {
		t.Fatalf("zero spend projects missing: %+v", projects)
	}
	for i := 1; i < len(projects); i++ {
		if projects[i-1].Cost < projects[i].Cost {
			t.Fatalf("projects out of spend order: %+v", projects)
		}
	}
	p, err := db.Project("/two/p00")
	if err != nil || p.Runs != 1 || p.Cost != 11 || p.Tokens != 110 {
		t.Fatalf("project = %+v, %v", p, err)
	}
	if _, err := db.Project("/missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing project: %v", err)
	}
	if p, err := db.Project(""); err != nil || p.Runs != 1 {
		t.Fatalf("empty repo = %+v, %v", p, err)
	}

	// All three runs share a submission time, so the run id must break ties.
	for _, id := range []string{"z3", "z2", "z1"} {
		seedRun(t, db, id, "/zero")
	}
	if _, err := db.sql.Exec(`UPDATE runs SET submitted_at = '2026-09-23T12:00:00Z' WHERE repo = '/zero'`); err != nil {
		t.Fatal(err)
	}
	rows, more, err := db.ProjectRuns("/zero", "", "", 2)
	if err != nil || !more || len(rows) != 2 || rows[0].ID != "zero" || rows[1].ID != "z3" {
		t.Fatalf("first page = %+v, more %v, %v", rows, more, err)
	}
	rows, more, err = db.ProjectRuns("/zero", rows[1].Submitted, rows[1].ID, 2)
	if err != nil || more || len(rows) != 2 || rows[0].ID != "z2" || rows[1].ID != "z1" {
		t.Fatalf("second page = %+v, more %v, %v", rows, more, err)
	}
}
