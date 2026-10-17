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
	for _, id := range []string{"open", "merged", "later"} {
		if _, err := raw.Exec(`INSERT INTO runs (run_id, workflow, repo, request, status, submitted_at) VALUES (?, 'build', '/repo', 'legacy', 'ok', 'old')`, id); err != nil {
			t.Fatal(err)
		}
		recordedPR(t, m, id, 0)
	}
	calls := 0
	m.viewPR = func(string, string) (workspace.PR, error) {
		calls++
		// PR candidates are ordered by run ID: later, merged, open.
		switch calls {
		case 1:
			return workspace.PR{}, errors.New("temporarily unavailable")
		case 2:
			return workspace.PR{State: "MERGED"}, nil
		default:
			return workspace.PR{State: "OPEN"}, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	passes := 0
	m.pollPRs(ctx, time.Millisecond, func() {
		m.backfillTasks()
		passes++
		open, _ := db.Get("open")
		merged, _ := db.Get("merged")
		later, _ := db.Get("later")
		if open.TaskID == nil || open.PRState == nil || *open.PRState != "open" || merged.TaskID != nil {
			t.Errorf("backfill after observation: open=%+v merged=%+v", open, merged)
		}
		if passes == 1 && later.TaskID != nil {
			t.Error("never-checked terminal run was backfilled")
		}
		if passes == 2 {
			if later.TaskID == nil {
				t.Error("subsequent pass did not backfill newly open run")
			}
			cancel()
		}
	})
	if passes != 2 {
		t.Fatalf("passes = %d", passes)
	}
}

func TestInterruptedPRPassSkipsBackfill(t *testing.T) {
	m, db, _ := newManager(t, 1, &fake{})
	id := submit(t, db, "build", "/repo")
	recordedPR(t, m, id, 0)
	ctx, cancel := context.WithCancel(context.Background())
	m.viewPR = func(string, string) (workspace.PR, error) {
		cancel()
		return workspace.PR{State: "OPEN"}, nil
	}
	called := false
	m.pollPRs(ctx, time.Millisecond, func() {
		called = true
		m.backfillTasks()
	})
	if called {
		t.Fatal("interrupted pass invoked backfill")
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
