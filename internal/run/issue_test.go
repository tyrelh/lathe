package run

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/trace"
)

func TestIssueRequestMixedCaseURL(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "gh", `printf '%s' '{"number":42,"title":"Fix it","body":"Details","url":"https://github.com/Owner/Repo/issues/42"}'`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	db, err := trace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const submitted = "https://GitHub.com/OWNER/repo/issues/42"
	id, err := db.Submit(trace.Request{Workflow: "build", Repo: bin, Spec: []byte(`{"issue":"` + submitted + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	r := &Run{ID: id, Repo: bin, Issue: submitted, Dir: t.TempDir(), db: db, ctx: context.Background()}
	h := &Handle{run: r, phase: trace.NewPhase(id, 1, "issue", "lathe")}
	request, err := h.IssueRequest()
	if err != nil || !strings.Contains(request, "Details") {
		t.Fatalf("mixed-case issue request = %q, %v", request, err)
	}
	row, err := db.Get(id)
	if err != nil || row.TaskID == nil {
		t.Fatalf("run = %+v, %v", row, err)
	}
	task, err := db.GetTask(*row.TaskID)
	if err != nil || task.IssueURL == nil || *task.IssueURL != "https://github.com/Owner/Repo/issues/42" || task.IssueBody == nil || *task.IssueBody != "Details" {
		t.Fatalf("mixed-case snapshot = %+v, %v", task, err)
	}
	attached, err := db.Submit(trace.Request{Workflow: "build", Repo: bin, TaskID: task.ID, Spec: []byte(`{"issue":"https://github.com/owner/REPO/issues/42"}`)})
	if err != nil {
		t.Fatalf("equivalent issue attachment: %v", err)
	}
	r.ID, r.Issue = attached, "https://github.com/owner/REPO/issues/42"
	h.phase = trace.NewPhase(attached, 1, "issue", "lathe")
	if _, err := h.IssueRequest(); err != nil {
		t.Fatalf("equivalent existing snapshot: %v", err)
	}
}

func TestIssueRequestSnapshotsTask(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "gh", `printf '%s' '{"number":42,"title":"Fix it","body":"Details","url":"https://github.com/o/r/issues/42"}'`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, workflow := range []string{"build", "plan"} {
		id, err := db.Submit(trace.Request{Workflow: workflow, Repo: bin, Request: "issue 42", Spec: []byte(`{"issue":"42"}`)})
		if err != nil {
			t.Fatal(err)
		}
		r := &Run{ID: id, Repo: bin, Issue: "42", Dir: t.TempDir(), db: db, ctx: context.Background()}
		h := &Handle{run: r, phase: trace.NewPhase(id, 1, "issue", "lathe")}
		request, err := h.IssueRequest()
		if err != nil || !strings.Contains(request, "Details") {
			t.Fatalf("request = %q, %v", request, err)
		}
		data, err := os.ReadFile(filepath.Join(r.Dir, IssueFile))
		if err != nil || !strings.Contains(string(data), "Fix it") {
			t.Fatalf("issue.json = %s, %v", data, err)
		}
		row, _ := db.Get(id)
		if workflow == "build" {
			task, err := db.GetTask(*row.TaskID)
			if err != nil || task.IssueURL == nil || *task.IssueURL != "https://github.com/o/r/issues/42" || *task.IssueTitle != "Fix it" || *task.IssueBody != "Details" {
				t.Fatalf("snapshot = %+v, %v", task, err)
			}
			raw, err := sql.Open("sqlite", filepath.Join(root, "lathe.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`UPDATE tasks SET issue_body = NULL`); err != nil {
				t.Fatal(err)
			}
			writeScript(t, bin, "gh", `printf '%s' '{"number":43,"title":"Wrong issue","body":"Wrong body","url":"https://github.com/o/r/issues/43"}'`)
			r.Issue = "43"
			if _, err := h.IssueRequest(); err == nil {
				t.Fatal("conflicting issue snapshot was accepted")
			}
			task, err = db.GetTask(*row.TaskID)
			if err != nil || task.IssueBody != nil || task.IssueTitle == nil || *task.IssueTitle != "Fix it" || task.IssueURL == nil || *task.IssueURL != "https://github.com/o/r/issues/42" {
				t.Fatalf("conflict changed partial snapshot: %+v, %v", task, err)
			}
			data, err = os.ReadFile(filepath.Join(r.Dir, IssueFile))
			if err != nil || !strings.Contains(string(data), "Wrong issue") {
				t.Fatalf("conflict lost issue.json: %s, %v", data, err)
			}
			r.Issue = "42"
			writeScript(t, bin, "gh", `printf '%s' '{"number":42,"title":"Fix it","body":"Details","url":"https://github.com/o/r/issues/42"}'`)
			if _, err := raw.Exec(`CREATE TRIGGER refuse_snapshot BEFORE UPDATE ON tasks BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := h.IssueRequest(); err == nil {
				t.Fatal("snapshot write error was swallowed")
			}
			if _, err := os.Stat(filepath.Join(r.Dir, IssueFile)); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`DROP TRIGGER refuse_snapshot`); err != nil {
				t.Fatal(err)
			}
		} else if row.TaskID != nil {
			t.Fatal("plan has task")
		}
	}
}
