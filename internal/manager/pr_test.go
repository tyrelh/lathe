package manager

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workspace"
)

func TestPRPollingBackfillsAfterObservations(t *testing.T) {
	m, db, _ := newManager(t, 1, &fake{})
	raw, err := sql.Open("sqlite", filepath.Join(m.dataRoot, "lathe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, id := range []string{"open", "merged"} {
		if _, err := raw.Exec(`INSERT INTO runs (run_id, workflow, repo, request, status) VALUES (?, 'build', '/repo', 'legacy', 'ok')`, id); err != nil {
			t.Fatal(err)
		}
		recordedPR(t, m, id, 0)
	}
	m.viewPR = func(repo, url string) (workspace.PR, error) {
		// Distinguish fixtures by the recorded URL.
		if strings.HasSuffix(url, "/2") {
			return workspace.PR{State: "MERGED"}, nil
		}
		return workspace.PR{State: "OPEN"}, nil
	}
	path := filepath.Join(worker.ReportDir(m.dataRoot, "merged", 0), "pr.json")
	if err := os.WriteFile(path, []byte(`{"url":"https://github.com/o/r/pull/2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	passes := 0
	m.pollPRs(ctx, time.Millisecond, func() {
		passes++
		m.backfillTasks()
		if passes == 1 {
			open, _ := db.Get("open")
			merged, _ := db.Get("merged")
			if open.TaskID == nil || open.PRState == nil || *open.PRState != "open" || merged.TaskID != nil || merged.PRState == nil || *merged.PRState != "merged" {
				t.Errorf("observations/backfill: open=%+v merged=%+v", open, merged)
			}
			// Finish polling candidates, then make a skipped run live again.
			if err := db.ObservePR("open", "closed"); err != nil {
				t.Error(err)
			}
			if _, err := raw.Exec(`UPDATE runs SET status = 'queued' WHERE run_id = 'merged'`); err != nil {
				t.Error(err)
			}
		}
		if passes == 3 {
			cancel()
		}
	})
	tasks, err := db.Tasks("")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks after empty/repeated passes = %+v, %v", tasks, err)
	}
	row, _ := db.Get("merged")
	if row.TaskID == nil {
		t.Fatal("empty pass did not backfill newly live run")
	}
}

func recordedPR(t *testing.T, m *manager, id string, iteration int) {
	t.Helper()
	dir := worker.ReportDir(m.dataRoot, id, iteration)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr.json"), []byte(`{"url":"https://github.com/o/r/pull/1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPRPassStatesAndErrors(t *testing.T) {
	m, db, _ := newManager(t, 1, &fake{})
	for _, state := range []string{"OPEN", "MERGED", "CLOSED"} {
		id := submit(t, db, "build", "/missing/repository")
		recordedPR(t, m, id, 0)
		calls := 0
		m.viewPR = func(repo, url string) (workspace.PR, error) {
			calls++
			return workspace.PR{State: state}, nil
		}
		m.pollPRPass(context.Background())
		row, _ := db.Get(id)
		want := strings.ToLower(state)
		if row.PRState == nil || *row.PRState != want {
			t.Fatalf("state = %+v", row)
		}
		at := *row.PRStateAt
		m.viewPR = func(string, string) (workspace.PR, error) {
			return workspace.PR{}, errors.New("gh failed: repository missing")
		}
		m.pollPRPass(context.Background())
		row, _ = db.Get(id)
		if *row.PRState != want || *row.PRStateAt != at || calls == 0 {
			t.Fatalf("failure changed observation: %+v", row)
		}
	}
	id := submit(t, db, "build", "/gone")
	recordedPR(t, m, id, 0)
	m.pollPRPass(context.Background())
	row, _ := db.Get(id)
	if row.PRState != nil || row.PRStateAt != nil {
		t.Fatal("gh failure changed null observation")
	}
}

func TestPRPollingDoesNotBlockDispatch(t *testing.T) {
	launcher := &fake{}
	m, db, _ := newManager(t, 1, launcher)
	m.out = io.Discard
	id := submit(t, db, "build", "/repo")
	recordedPR(t, m, id, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	m.viewPR = func(string, string) (workspace.PR, error) {
		close(entered)
		<-release
		return workspace.PR{State: "MERGED"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	hook := make(chan struct{}, 2)
	go func() {
		defer close(done)
		m.pollPRs(ctx, time.Millisecond, func() { hook <- struct{}{} })
	}()
	<-entered
	if err := m.dispatch(); err != nil {
		t.Error(err)
	}
	row, _ := db.Get(id)
	if row.Status != trace.StatusStarting {
		t.Error("dispatch did not progress")
	}
	close(release)
	<-hook
	row, _ = db.Get(id)
	if row.PRState == nil || *row.PRState != "merged" {
		t.Error("hook ran before commit")
	}
	<-hook // A second, empty pass still invokes the hook.
	cancel()
	<-done
}
