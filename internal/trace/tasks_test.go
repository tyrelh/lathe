package trace

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestStandaloneTaskWrites(t *testing.T) {
	db := open(t)
	in := TaskInput{Repo: "/repo", Text: "GitHub issue 42", IssueRef: "42", IssueURL: "https://github.com/o/r/issues/42", IssueTitle: "title", IssueBody: ""}
	id, err := db.CreateTask(in)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask(id)
	if err != nil || task.CreatedAt == "" || task.UpdatedAt == "" || task.IssueBody == nil || *task.IssueBody != "" || task.IssueTitle == nil || *task.IssueTitle != "title" {
		t.Fatalf("created task = %+v, %v", task, err)
	}
	runs, err := db.TaskRuns(id)
	if err != nil || len(runs) != 0 {
		t.Fatalf("standalone runs = %v, %v", runs, err)
	}
	created := task.CreatedAt
	if _, err := db.sql.Exec(`UPDATE tasks SET updated_at = '2000-01-01T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	in.IssueRef, in.IssueURL, in.IssueTitle, in.IssueBody = "43", "https://github.com/o/r/issues/43", "replacement", "body"
	if err := db.EditTask(id, in); err != nil {
		t.Fatal(err)
	}
	task, _ = db.GetTask(id)
	if *task.IssueRef != "43" || *task.IssueBody != "body" || task.CreatedAt != created || task.UpdatedAt == "2000-01-01T00:00:00Z" {
		t.Fatalf("replacement = %+v", task)
	}
	if err := db.EditTask(id, TaskInput{Repo: "/other", Text: "plain"}); err != nil {
		t.Fatal(err)
	}
	task, _ = db.GetTask(id)
	if task.Tracker != nil || task.IssueRef != nil || task.IssueURL != nil || task.IssueTitle != nil || task.IssueBody != nil || task.Text != "plain" || task.Repo != "/other" {
		t.Fatalf("text replacement = %+v", task)
	}
	if err := db.EditTask("missing", in); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
	if err := db.ArchiveTask(id); err != nil {
		t.Fatal(err)
	}
	before, _ := db.GetTask(id)
	if err := db.EditTask(id, in); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("archived: %v", err)
	}
	after, _ := db.GetTask(id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused edit changed task")
	}
	for _, status := range []string{StatusQueued, StatusOK, StatusFail, StatusCancelled, StatusLost} {
		run := submit(t, db, "build", "/repo")
		row, _ := db.Get(run)
		setStatus(t, db, run, status)
		if err := db.EditTask(*row.TaskID, in); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("%s: %v", status, err)
		}
	}
}

func TestTaskWriteRollback(t *testing.T) {
	db := open(t)
	id, err := db.CreateTask(TaskInput{Repo: "/repo", Text: "before"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER refuse_task_update BEFORE UPDATE ON tasks BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.EditTask(id, TaskInput{Repo: "/repo", Text: "after"}); err == nil {
		t.Fatal("edit succeeded")
	}
	if _, err := db.CreateTask(TaskInput{Repo: "/repo", Text: "new"}); err == nil {
		t.Fatal("create succeeded")
	}
	tasks, err := db.Tasks("")
	if err != nil || len(tasks) != 1 || tasks[0].Text != "before" {
		t.Fatalf("rollback: %+v, %v", tasks, err)
	}
}

func TestArchiveTask(t *testing.T) {
	for _, status := range []string{"backlog", StatusQueued, StatusStarting, StatusRunning, StatusOK, StatusFail} {
		t.Run(status, func(t *testing.T) {
			db := open(t)
			id := submit(t, db, "build", "/repo")
			row, _ := db.Get(id)
			taskID := *row.TaskID
			if status == "backlog" {
				if _, err := db.sql.Exec(`UPDATE runs SET task_id = NULL WHERE run_id = ?`, id); err != nil {
					t.Fatal(err)
				}
			} else {
				setStatus(t, db, id, status)
			}
			if err := db.ArchiveTask(taskID); err != nil {
				t.Fatal(err)
			}
			task, _ := db.GetTask(taskID)
			if task.ArchivedAt == nil {
				t.Fatal("not archived")
			}
			cancelled, err := db.Cancelled(id)
			live := status == StatusQueued || status == StatusStarting || status == StatusRunning
			if err != nil || cancelled != live {
				t.Fatalf("cancelled = %v, %v", cancelled, err)
			}
			got, _ := db.Get(id)
			if status == StatusQueued && got.Status != StatusCancelled || (status == StatusStarting || status == StatusRunning) && got.Status != status {
				t.Fatalf("status = %s", got.Status)
			}
			if err := db.ArchiveTask(taskID); err != nil {
				t.Fatal(err)
			}
			again, _ := db.GetTask(taskID)
			if *again.ArchivedAt != *task.ArchivedAt || again.UpdatedAt != task.UpdatedAt {
				t.Fatal("repeat archive changed timestamps")
			}
			for range 2 {
				if err := db.UnarchiveTask(taskID); err != nil {
					t.Fatal(err)
				}
			}
			task, _ = db.GetTask(taskID)
			if task.ArchivedAt != nil {
				t.Fatal("still archived")
			}
		})
	}
	db := open(t)
	for _, action := range []func(string) error{db.ArchiveTask, db.UnarchiveTask} {
		if err := action("missing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing task: %v", err)
		}
	}
}

func TestArchiveCurrentSubmission(t *testing.T) {
	db := open(t)
	first := submit(t, db, "build", "/repo")
	row, _ := db.Get(first)
	second, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: *row.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	for id, at := range map[string]string{first: "2020", second: "2021"} {
		if _, err := db.sql.Exec(`UPDATE runs SET submitted_at = ? WHERE run_id = ?`, at, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ArchiveTask(*row.TaskID); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{first: false, second: true} {
		if got, err := db.Cancelled(id); err != nil || got != want {
			t.Fatalf("cancelled %s = %v, %v", id, got, err)
		}
	}
	if err := db.UnarchiveTask(*row.TaskID); err != nil {
		t.Fatal(err)
	}
	setStatus(t, db, second, StatusOK)
	if err := db.ArchiveTask(*row.TaskID); err != nil {
		t.Fatal(err)
	}
	if cancelled, err := db.Cancelled(first); err != nil || cancelled {
		t.Fatalf("earlier live run cancelled = %v, %v", cancelled, err)
	}
}

func setStatus(t *testing.T, db *DB, runID, status string) {
	t.Helper()
	for _, table := range []string{"runs", "iterations"} {
		if _, err := db.sql.Exec(`UPDATE `+table+` SET status = ? WHERE run_id = ?`, status, runID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTaskLane(t *testing.T) {
	open, closed, merged := "open", "closed", "merged"
	for _, tc := range []struct {
		status  string
		prState *string
		hasPR   bool
		want    string
	}{
		{StatusRunning, &merged, true, LaneDone},
		{StatusFail, &merged, true, LaneDone},
		{StatusQueued, nil, false, LaneInProgress},
		{StatusStarting, &closed, true, LaneInProgress},
		{StatusRunning, &open, true, LaneInProgress},
		{StatusOK, &open, true, LaneReadyForReview},
		{StatusOK, nil, true, LaneReadyForReview},
		{StatusOK, nil, false, LaneBlocked},
		{StatusOK, &closed, true, LaneBlocked},
		{StatusFail, &open, true, LaneBlocked},
		{StatusCancelled, nil, false, LaneBlocked},
		{StatusLost, nil, false, LaneBlocked},
		{"unknown", nil, true, LaneBlocked},
	} {
		if got := TaskLane(tc.status, tc.prState, tc.hasPR); got != tc.want {
			t.Errorf("TaskLane(%q, %v, %v) = %q, want %q", tc.status, tc.prState, tc.hasPR, got, tc.want)
		}
	}
}

// issueSpec returns a run spec recording issue ref.
func issueSpec(ref string) []byte {
	spec, _ := json.Marshal(map[string]string{"issue": ref})
	return spec
}

func TestTaskReadsAndBacklog(t *testing.T) {
	db := open(t)
	tx, err := db.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := insertTask(tx, "/repo", "backlog", "", nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask(id)
	if err != nil || task.Text != "backlog" || task.Tracker != nil || task.IssueRef != nil || task.IssueURL != nil || task.IssueTitle != nil || task.IssueBody != nil || task.ArchivedAt != nil {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if rows, err := db.TaskRuns(id); err != nil || len(rows) != 0 {
		t.Fatalf("backlog runs = %+v, %v", rows, err)
	}
	if _, err := db.GetTask("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing = %v", err)
	}
	submit(t, db, "build", "/other")
	for repo, want := range map[string]int{"": 2, "/repo": 1, "/rep": 0} {
		if tasks, err := db.Tasks(repo); err != nil || len(tasks) != want {
			t.Fatalf("Tasks(%q) = %+v, %v", repo, tasks, err)
		}
	}
	if _, err := db.sql.Exec(`UPDATE tasks SET archived_at = 'yesterday' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if tasks, err := db.Tasks("/repo"); err != nil || len(tasks) != 1 || tasks[0].ArchivedAt == nil {
		t.Fatalf("archived read = %+v, %v", tasks, err)
	}
}

func TestTaskRunsOrderBySubmission(t *testing.T) {
	db := open(t)
	first := submit(t, db, "build", "/repo")
	row, _ := db.Get(first)
	second, err := db.Submit(Request{Workflow: "build", Repo: "/repo", TaskID: *row.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	for id, at := range map[string]string{first: "2026-01-01", second: "2026-01-02"} {
		if _, err := db.sql.Exec(`UPDATE runs SET submitted_at = ?, activity_at = '2027-01-01' WHERE run_id = ?`, at, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.sql.Exec(`UPDATE runs SET activity_at = '2028-01-01', iteration = 5 WHERE run_id = ?`, first); err != nil {
		t.Fatal(err)
	}
	runs, err := db.TaskRuns(*row.TaskID)
	if err != nil || len(runs) != 2 || runs[0].ID != second {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if _, err := db.sql.Exec(`UPDATE runs SET submitted_at = 'same'`); err != nil {
		t.Fatal(err)
	}
	runs, err = db.TaskRuns(*row.TaskID)
	if err != nil || runs[0].ID < runs[1].ID {
		t.Fatalf("tie order = %+v, %v", runs, err)
	}
}

func TestFillTaskIssue(t *testing.T) {
	db := open(t)
	id, err := db.Submit(Request{Workflow: "build", Repo: "/repo", Spec: issueSpec("42")})
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://github.com/o/r/issues/42"
	row, _ := db.Get(id)
	if _, err := db.sql.Exec(`UPDATE tasks SET issue_title = 'original', issue_body = '', updated_at = 'before' WHERE id = ?`, *row.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := db.FillTaskIssue(id, "42", url, "new", "body"); err != nil {
		t.Fatal(err)
	}
	task, _ := db.GetTask(*row.TaskID)
	if *task.IssueTitle != "original" || *task.IssueURL != url || *task.IssueBody != "body" || task.UpdatedAt == "before" {
		t.Fatalf("snapshot = %+v", task)
	}
	if _, err := db.sql.Exec(`UPDATE tasks SET updated_at = 'unchanged' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.FillTaskIssue(id, "42", url, "different", "different"); err != nil {
		t.Fatal(err)
	}
	task, _ = db.GetTask(task.ID)
	if task.UpdatedAt != "unchanged" || *task.IssueURL != url || *task.IssueBody != "body" {
		t.Fatalf("repeat snapshot = %+v", task)
	}
	if err := db.FillTaskIssue(submit(t, db, "plan", "/repo"), "42", url, "title", "body"); err != nil {
		t.Fatal(err)
	}
}

func TestFillTaskIssueRejectsConflicts(t *testing.T) {
	const issueURL = "https://github.com/o/r/issues/42"
	for _, tc := range []struct {
		name, recorded, resolvedRef, resolvedURL, storedURL string
	}{
		{"different numeric ref", "42", "43", "https://github.com/o/r/issues/43", ""},
		{"different numeric URL", "42", "42", "https://github.com/o/r/issues/43", ""},
		{"different URL repo", issueURL, issueURL, "https://github.com/o/other/issues/42", ""},
		{"different URL owner", issueURL, issueURL, "https://github.com/other/r/issues/42", ""},
		{"different URL host", issueURL, issueURL, "https://github.example/o/r/issues/42", ""},
		{"different URL number", issueURL, issueURL, "https://github.com/o/r/issues/43", ""},
		{"existing URL host", "42", "42", issueURL, "https://github.example/o/r/issues/42"},
		{"existing URL owner", "42", "42", issueURL, "https://github.com/other/r/issues/42"},
		{"existing URL number", "42", "42", issueURL, "https://github.com/o/r/issues/43"},
		{"different URL ref", issueURL, "https://github.com/other/repo/issues/42", "https://github.com/other/repo/issues/42", ""},
		{"existing numeric snapshot URL", "42", "42", "https://github.com/other/repo/issues/42", issueURL},
		{"no recorded issue", "", "42", issueURL, ""},
		{"invalid resolved URL", "42", "42", "not-an-issue-url", ""},
	} {
		for _, partial := range []bool{false, true} {
			t.Run(tc.name+fmt.Sprint("/partial=", partial), func(t *testing.T) {
				db := open(t)
				id, err := db.Submit(Request{Workflow: "build", Repo: "/repo", Spec: issueSpec(tc.recorded)})
				if err != nil {
					t.Fatal(err)
				}
				row, _ := db.Get(id)
				var title any
				if partial {
					title = "original title"
				}
				if _, err := db.sql.Exec(`UPDATE tasks SET issue_url = ?, issue_title = ?, issue_body = '', updated_at = 'before' WHERE id = ?`, nullIfEmpty(tc.storedURL), title, *row.TaskID); err != nil {
					t.Fatal(err)
				}
				before, err := db.GetTask(*row.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.FillTaskIssue(id, tc.resolvedRef, tc.resolvedURL, "wrong title", "wrong body"); err == nil {
					t.Fatal("accepted conflicting snapshot")
				}
				after, err := db.GetTask(*row.TaskID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("conflict changed task: %+v -> %+v, %v", before, after, err)
				}
			})
		}
	}
}

func TestFillTaskIssueURLReference(t *testing.T) {
	const resolved = "https://github.example/Owner/Repo/issues/42"
	for _, recorded := range []string{resolved, "https://GITHUB.example/owner/REPO/issues/42", "42"} {
		t.Run(recorded, func(t *testing.T) {
			db := open(t)
			id, err := db.Submit(Request{Workflow: "build", Repo: "/repo", Spec: issueSpec(recorded)})
			if err != nil {
				t.Fatal(err)
			}
			row, _ := db.Get(id)
			const stored = "https://GitHub.Example/OWNER/repo/issues/42"
			if _, err := db.sql.Exec(`UPDATE tasks SET issue_url = ?, issue_title = 'original' WHERE id = ?`, stored, *row.TaskID); err != nil {
				t.Fatal(err)
			}
			ref := recorded
			if ref != "42" {
				ref = resolved
			}
			if err := db.FillTaskIssue(id, ref, resolved, "title", "body"); err != nil {
				t.Fatal(err)
			}
			task, err := db.GetTask(*row.TaskID)
			if err != nil || task.IssueURL == nil || *task.IssueURL != stored || task.IssueBody == nil || *task.IssueBody != "body" || task.IssueTitle == nil || *task.IssueTitle != "original" {
				t.Fatalf("URL snapshot = %+v, %v", task, err)
			}
		})
	}
}

func TestBackfillMissingIssueAndRollback(t *testing.T) {
	db := open(t)
	for _, id := range []string{"missing", "malformed"} {
		if _, err := db.sql.Exec(`INSERT INTO runs (run_id, workflow, repo, request, status) VALUES (?, 'build', '/repo', 'legacy', 'queued')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.sql.Exec(`INSERT INTO iterations (run_id, iteration, spec) VALUES ('malformed', 0, 'invalid json')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER refuse_attachment BEFORE UPDATE OF task_id ON runs BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.BackfillTasks(); err == nil {
		t.Fatal("backfill succeeded despite refused attachment")
	}
	if tasks, _ := db.Tasks(""); len(tasks) != 0 {
		t.Fatalf("orphan tasks = %+v", tasks)
	}
	if _, err := db.sql.Exec(`DROP TRIGGER refuse_attachment`); err != nil {
		t.Fatal(err)
	}
	if err := db.BackfillTasks(); err != nil {
		t.Fatal(err)
	}
	tasks, err := db.Tasks("")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}
	for _, task := range tasks {
		if task.IssueRef != nil || task.Tracker != nil {
			t.Fatalf("invented issue: %+v", task)
		}
	}
}

func TestBackfillTasks(t *testing.T) {
	db := open(t)
	for _, tc := range []struct {
		id, workflow, status string
		pr                   any
		want                 bool
	}{
		{"queued", "build", StatusQueued, nil, true},
		{"starting", "build", StatusStarting, nil, true},
		{"running", "build", StatusRunning, "merged", true},
		{"open", "build", StatusOK, "open", true},
		{"failed-open", "build", StatusFail, "open", true},
		{"merged", "build", StatusOK, "merged", false},
		{"closed", "build", StatusFail, "closed", false},
		{"unchecked", "build", StatusOK, nil, false},
		{"cancelled", "build", StatusCancelled, nil, false},
		{"lost", "build", StatusLost, nil, false},
		{"plan", "plan", StatusQueued, "open", false},
	} {
		if _, err := db.sql.Exec(`INSERT INTO runs (run_id, workflow, repo, request, status, pr_state) VALUES (?, ?, '/repo', 'original', ?, ?)`, tc.id, tc.workflow, tc.status, tc.pr); err != nil {
			t.Fatal(err)
		}
		if _, err := db.sql.Exec(`INSERT INTO iterations (run_id, iteration, spec) VALUES (?, 0, '{"issue":"42"}')`, tc.id); err != nil {
			t.Fatal(err)
		}
	}
	existing := submit(t, db, "build", "/existing")
	before, _ := db.Get(existing)
	for range 2 {
		if err := db.BackfillTasks(); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"queued", "starting", "running", "open", "failed-open"} {
		task, err := db.GetTask(one[string](t, db, `SELECT task_id FROM runs WHERE run_id = ?`, id))
		if err != nil || task.Repo != "/repo" || task.Text != "original" || task.IssueRef == nil || *task.IssueRef != "42" || *task.Tracker != "github" {
			t.Fatalf("backfill task = %+v, %v", task, err)
		}
	}
	if skipped := one[int](t, db, `SELECT count(*) FROM runs WHERE task_id IS NULL`); skipped != 6 {
		t.Fatalf("skipped = %d", skipped)
	}
	tasks, _ := db.Tasks("")
	if len(tasks) != 6 {
		t.Fatalf("duplicate tasks: %d", len(tasks))
	}
	after, _ := db.Get(existing)
	if *before.TaskID != *after.TaskID {
		t.Fatal("replaced existing task")
	}
	// A skipped successful run becomes eligible after revision.
	if _, err := db.Revise("unchecked", 0, "next", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Recovery also mirrors a new live state onto the run.
	if _, err := db.sql.Exec(`UPDATE runs SET status = 'queued', iteration = 1 WHERE run_id = 'lost'`); err != nil {
		t.Fatal(err)
	}
	if err := db.BackfillTasks(); err != nil {
		t.Fatal(err)
	}
	tasks, _ = db.Tasks("")
	if len(tasks) != 8 {
		t.Fatalf("newly eligible tasks = %d", len(tasks))
	}
}
