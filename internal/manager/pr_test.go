package manager

import (
	"context"
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
