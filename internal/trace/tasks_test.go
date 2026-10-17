package trace

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

func TestTaskSubmission(t *testing.T) {
	db := open(t)
	id, err := db.Submit(Request{Workflow: "build", Repo: "/repo", Request: "fix", Issue: "42"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := db.Get(id)
	if err != nil || r.TaskID == nil {
		t.Fatalf("run = %+v, %v", r, err)
	}
	task, err := db.GetTask(*r.TaskID)
	if err != nil || task.Text != "fix" || task.Repo != "/repo" || task.Tracker == nil || *task.Tracker != "github" || task.IssueRef == nil || *task.IssueRef != "42" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if task.ArchivedAt != nil || task.IssueTitle != nil || task.IssueBody != nil || task.IssueURL != nil {
		t.Fatalf("unexpected snapshot: %+v", task)
	}
	attached, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := db.Get(attached)
	if second.TaskID == nil || *second.TaskID != task.ID {
		t.Fatalf("attachment = %+v", second)
	}
	for _, req := range []Request{
		{Workflow: "build", Repo: "/repo", TaskID: "missing"},
		{Workflow: "build", Repo: "/other", TaskID: task.ID},
		{Workflow: "build", Repo: "/repo", TaskID: task.ID, Issue: "43"},
	} {
		if _, err := db.Submit(req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	runs, err := db.TaskRuns(task.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("refused attachments changed runs: %+v, %v", runs, err)
	}
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: task.ID, Issue: "42"}); err != nil {
		t.Fatalf("matching issue refused: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE tasks SET archived_at = 'archived' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: task.ID}); err == nil {
		t.Fatal("accepted archived task")
	}
	for _, workflow := range []string{"scout", "plan", "implement"} {
		id, err := db.Submit(Request{Workflow: workflow, Repo: "/repo", TaskID: "missing", Issue: "42"})
		if err != nil {
			t.Fatal(err)
		}
		r, err := db.Get(id)
		if err != nil || r.TaskID != nil {
			t.Fatalf("non-build = %+v, %v", r, err)
		}
	}
	tasks, err := db.Tasks("")
	if err != nil || len(tasks) != 1 || tasks[0].ArchivedAt == nil {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
}

func TestTaskSubmissionRollback(t *testing.T) {
	db := open(t)
	if _, err := db.sql.Exec(`CREATE TRIGGER reject_iteration BEFORE INSERT ON iterations BEGIN SELECT RAISE(ABORT, 'reject'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Submit(Request{Workflow: "build", Repo: "/repo"}); err == nil {
		t.Fatal("submission succeeded")
	}
	tasks, err := db.Tasks("")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks survived rollback: %+v, %v", tasks, err)
	}
	runs, err := db.Recent(10)
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs survived rollback: %+v, %v", runs, err)
	}
}

func TestTaskReaders(t *testing.T) {
	db := open(t)
	if _, err := db.sql.Exec(`INSERT INTO tasks (id, repo, text, created_at, updated_at) VALUES ('backlog', '/empty', 'todo', 'old', 'old')`); err != nil {
		t.Fatal(err)
	}
	tasks, err := db.Tasks("/empty")
	if err != nil || len(tasks) != 1 || tasks[0].Tracker != nil || tasks[0].IssueRef != nil {
		t.Fatalf("backlog = %+v, %v", tasks, err)
	}
	runs, err := db.TaskRuns("backlog")
	if err != nil || len(runs) != 0 {
		t.Fatalf("backlog runs = %+v, %v", runs, err)
	}
	if _, err := db.GetTask("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing = %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := db.sql.Exec(`INSERT INTO runs (run_id, workflow, repo, request, status, submitted_at, activity_at, task_id) VALUES (?, 'build', '/empty', '', 'ok', ?, ?, 'backlog')`, id, map[string]string{"a": "1", "b": "2", "c": "2"}[id], map[string]string{"a": "9", "b": "1", "c": "0"}[id]); err != nil {
			t.Fatal(err)
		}
	}
	runs, err = db.TaskRuns("backlog")
	if err != nil || len(runs) != 3 || runs[0].ID != "c" || runs[1].ID != "b" || runs[2].ID != "a" {
		t.Fatalf("ordered runs = %+v, %v", runs, err)
	}
	if tasks, err := db.Tasks("/other"); err != nil || len(tasks) != 0 {
		t.Fatalf("filter = %+v, %v", tasks, err)
	}
}

func TestFillTaskIssue(t *testing.T) {
	db := open(t)
	id, err := db.Submit(Request{Workflow: "build", Repo: "/repo", Issue: "42"})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := db.Get(id)
	before, err := db.GetTask(*r.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"43", ""} {
		if err := db.FillTaskIssue(id, ref, "wrong-url", "wrong-title", "wrong-body"); err != nil {
			t.Fatal(err)
		}
		after, err := db.GetTask(*r.TaskID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("mismatched snapshot changed task: %+v -> %+v, %v", before, after, err)
		}
	}
	if _, err := db.sql.Exec(`UPDATE tasks SET issue_title = 'original', issue_body = '', updated_at = 'old' WHERE id = ?`, *r.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := db.FillTaskIssue(id, "42", "url", "new", "body"); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask(*r.TaskID)
	if err != nil || *task.IssueTitle != "original" || *task.IssueURL != "url" || *task.IssueBody != "body" || task.UpdatedAt == "old" {
		t.Fatalf("snapshot = %+v, %v", task, err)
	}
	if _, err := db.sql.Exec(`UPDATE tasks SET updated_at = 'unchanged' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.FillTaskIssue(id, "42", "other", "other", "other"); err != nil {
		t.Fatal(err)
	}
	task, _ = db.GetTask(task.ID)
	if task.UpdatedAt != "unchanged" || *task.IssueURL != "url" || *task.IssueBody != "body" {
		t.Fatalf("snapshot overwritten: %+v", task)
	}
	if err := db.FillTaskIssue(submit(t, db, "plan", "/repo"), "42", "url", "title", "body"); err != nil {
		t.Fatal(err)
	}
	if err := db.FillTaskIssue("missing", "42", "url", "title", "body"); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillRevisedIssueBuild(t *testing.T) {
	db := open(t)
	const ref = "https://github.com/o/r/issues/42"
	id, err := db.Submit(Request{
		Workflow: "build", Repo: "/repo", Request: "original issue request", Issue: ref,
		Spec: []byte(`{"issue":"https://github.com/o/r/issues/42"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, token := dispatch(t, db, id)
	if err := db.Complete(id, attempt, token, StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	original, err := db.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE runs SET task_id = NULL WHERE run_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM tasks WHERE id = ?`, *original.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Revise(id, 0, "revision guidance", []byte(`{"workflow":"build","request":"revision guidance"}`)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.BackfillTasks(); err != nil {
			t.Fatal(err)
		}
	}
	row, err := db.Get(id)
	if err != nil || row.TaskID == nil {
		t.Fatalf("revised legacy run = %+v, %v", row, err)
	}
	task, err := db.GetTask(*row.TaskID)
	if err != nil || task.IssueRef == nil || *task.IssueRef != ref || task.Tracker == nil || *task.Tracker != "github" || task.Text != original.Request || task.CreatedAt != original.Submitted {
		t.Fatalf("revised legacy task = %+v, %v", task, err)
	}
	tasks, err := db.Tasks("")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("backfill created duplicate tasks: %+v, %v", tasks, err)
	}
}

func TestBackfillTasks(t *testing.T) {
	db := open(t)
	cases := []struct {
		id, workflow, status string
		pr                   any
		want                 bool
	}{
		{"live", "build", StatusRunning, nil, true},
		{"queued", "build", StatusQueued, nil, true},
		{"starting", "build", StatusStarting, nil, true},
		{"open", "build", StatusOK, "open", true},
		{"merged", "build", StatusOK, "merged", false},
		{"closed", "build", StatusOK, "closed", false},
		{"unchecked", "build", StatusOK, nil, false},
		{"fail", "build", StatusFail, nil, false},
		{"cancelled", "build", StatusCancelled, nil, false},
		{"lost", "build", StatusLost, nil, false},
		{"scout", "scout", StatusRunning, "open", false},
		{"plan", "plan", StatusQueued, nil, false},
		{"implement", "implement", StatusQueued, nil, false},
	}
	for _, c := range cases {
		if _, err := db.sql.Exec(`INSERT INTO runs (run_id, workflow, repo, request, status, submitted_at, spec, pr_state) VALUES (?, ?, '/repo', 'original', ?, 'timestamp', ?, ?)`, c.id, c.workflow, c.status, `{"issue":"42"}`, c.pr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.sql.Exec(`INSERT INTO iterations (run_id, iteration, spec) SELECT run_id, 0, spec FROM runs`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.BackfillTasks(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cases {
		r, err := db.Get(c.id)
		if err != nil || (r.TaskID != nil) != c.want {
			t.Fatalf("%s: %+v, %v", c.id, r, err)
		}
		if c.want {
			task, err := db.GetTask(*r.TaskID)
			if err != nil || task.CreatedAt != "timestamp" || task.Text != "original" || task.IssueRef == nil || *task.IssueRef != "42" {
				t.Fatalf("%s task = %+v, %v", c.id, task, err)
			}
		}
	}
	tasks, err := db.Tasks("")
	if err != nil || len(tasks) != 4 {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	// A later revision or recovery becomes eligible without replaying migration.
	if _, err := db.sql.Exec(`UPDATE runs SET status = 'queued', iteration = 1, spec = 'invalid' WHERE run_id IN ('unchecked', 'lost')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE iterations SET spec = 'invalid' WHERE run_id = 'unchecked'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM iterations WHERE run_id = 'lost'`); err != nil {
		t.Fatal(err)
	}
	if err := db.BackfillTasks(); err != nil {
		t.Fatal(err)
	}
	tasks, err = db.Tasks("")
	if err != nil || len(tasks) != 6 {
		t.Fatalf("later tasks = %+v, %v", tasks, err)
	}
	for _, id := range []string{"unchecked", "lost"} {
		r, _ := db.Get(id)
		task, err := db.GetTask(*r.TaskID)
		if err != nil || task.IssueRef != nil {
			t.Fatalf("malformed spec = %+v, %v", task, err)
		}
	}
}
