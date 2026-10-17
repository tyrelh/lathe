package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workspace"
)

func TestIssueRequestTaskSnapshot(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "gh", `if [ "$3" = 43 ]; then
printf '%s\n' '{"number":43,"title":"wrong title","body":"wrong body","url":"https://github.com/o/r/issues/43"}'
else
printf '%s\n' '{"number":42,"title":"title","body":"body","url":"https://github.com/o/r/issues/42"}'
fi`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	db, err := trace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, workflow := range []string{"build", "plan"} {
		id, err := db.Submit(trace.Request{Workflow: workflow, Repo: bin, Issue: "42"})
		if err != nil {
			t.Fatal(err)
		}
		r := &Run{ID: id, Repo: bin, Issue: "42", Dir: t.TempDir(), db: db, ctx: context.Background()}
		h := &Handle{run: r, phase: &trace.Phase{ID: "issue-phase"}}
		if workflow == "build" {
			// A legacy spec can disagree with the task even when Submit now refuses it.
			r.Issue = "43"
			if _, err := h.IssueRequest(); err != nil {
				t.Fatal(err)
			}
			row, _ := db.Get(id)
			task, err := db.GetTask(*row.TaskID)
			if err != nil || task.IssueTitle != nil || task.IssueBody != nil || task.IssueURL != nil {
				t.Fatalf("conflicting resolution changed snapshot: %+v, %v", task, err)
			}
			r.Issue = "42"
		}
		for range 2 {
			request, err := h.IssueRequest()
			if err != nil || !strings.Contains(request, "title") || !strings.Contains(request, "body") {
				t.Fatalf("request = %q, %v", request, err)
			}
		}
		data, err := os.ReadFile(filepath.Join(r.Dir, IssueFile))
		if err != nil {
			t.Fatal(err)
		}
		var issue workspace.Issue
		if err := json.Unmarshal(data, &issue); err != nil || issue.Title != "title" {
			t.Fatalf("issue.json = %s, %v", data, err)
		}
		row, _ := db.Get(id)
		if workflow == "build" {
			task, err := db.GetTask(*row.TaskID)
			if err != nil || task.IssueTitle == nil || *task.IssueTitle != "title" || task.IssueBody == nil || *task.IssueBody != "body" || task.IssueURL == nil || *task.IssueURL != issue.URL {
				t.Fatalf("snapshot = %+v, %v", task, err)
			}
		} else if row.TaskID != nil {
			t.Fatal("non-build gained a task")
		}
	}
}
