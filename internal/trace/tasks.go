package trace

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	LaneBacklog        = "Backlog"
	LaneInProgress     = "In progress"
	LaneBlocked        = "Blocked"
	LaneReadyForReview = "Ready for review"
	LaneDone           = "Done"
)

// TaskLane derives a lane from the current run's status and PR.
func TaskLane(status string, prState *string, hasPR bool) string {
	state := ""
	if prState != nil {
		state = *prState
	}
	if state == "merged" {
		return LaneDone
	}
	switch status {
	case StatusQueued, StatusStarting, StatusRunning:
		return LaneInProgress
	}
	if state == "closed" {
		return LaneBlocked
	}
	if status == StatusOK && hasPR {
		return LaneReadyForReview
	}
	return LaneBlocked
}

// Task is a tracked change in one repository, optionally linked to an issue.
type Task struct {
	ID         string  `json:"id"`
	Repo       string  `json:"repo"`
	Text       string  `json:"text"`
	ArchivedAt *string `json:"archived_at"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	Tracker    *string `json:"tracker"`
	IssueRef   *string `json:"issue_ref"`
	IssueURL   *string `json:"issue_url"`
	IssueTitle *string `json:"issue_title"`
	IssueBody  *string `json:"issue_body"`
}

const taskColumns = `SELECT id, repo, text, archived_at, created_at, updated_at,
 tracker, issue_ref, issue_url, issue_title, issue_body FROM tasks`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Repo, &t.Text, &t.ArchivedAt, &t.CreatedAt, &t.UpdatedAt,
		&t.Tracker, &t.IssueRef, &t.IssueURL, &t.IssueTitle, &t.IssueBody)
	return t, err
}

func getTask(q queryer, id string) (Task, error) {
	return scanTask(q.QueryRow(taskColumns+` WHERE id = ?`, id))
}

// GetTask returns a task, including archived tasks, or sql.ErrNoRows.
func (d *DB) GetTask(id string) (Task, error) { return getTask(d.sql, id) }

// Tasks lists all tasks, including archived ones, optionally filtering by exact repo.
func (d *DB) Tasks(repo string) ([]Task, error) {
	query := taskColumns
	var args []any
	if repo != "" {
		query += ` WHERE repo = ?`
		args = append(args, repo)
	}
	rows, err := d.sql.Query(query+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskRuns returns a task's runs newest submission first, independent of revisions.
func (d *DB) TaskRuns(id string) ([]Row, error) {
	rows, err := d.sql.Query(runColumns+` FROM runs WHERE task_id = ? ORDER BY submitted_at DESC, run_id DESC`, id)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// TasksRuns returns every task-linked run keyed by task ID, newest submission first.
func (d *DB) TasksRuns() (map[string][]Row, error) {
	rows, err := d.sql.Query(runColumns + ` FROM runs WHERE task_id IS NOT NULL ORDER BY submitted_at DESC, run_id DESC`)
	if err != nil {
		return nil, err
	}
	all, err := scanRows(rows)
	out := map[string][]Row{}
	for _, r := range all {
		out[*r.TaskID] = append(out[*r.TaskID], r)
	}
	return out, err
}

// ArchiveTask requests cancellation of the current live run and archives the task.
func (d *DB) ArchiveTask(id string) error {
	task, err := d.GetTask(id)
	if err != nil {
		return err
	}
	if task.ArchivedAt != nil {
		return nil
	}
	var runID string
	switch err := d.sql.QueryRow(`SELECT run_id FROM runs WHERE task_id = ? ORDER BY submitted_at DESC, run_id DESC LIMIT 1`, id).Scan(&runID); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		if _, err := d.RequestCancel(runID); err != nil && !errors.Is(err, ErrAlreadyDone) {
			return err
		}
	}
	at := nowUTC()
	_, err = d.sql.Exec(`UPDATE tasks SET archived_at = ?, updated_at = ? WHERE id = ? AND archived_at IS NULL`, at, at, id)
	return err
}

// UnarchiveTask clears a task's archive timestamp without changing its runs.
func (d *DB) UnarchiveTask(id string) error {
	if _, err := d.GetTask(id); err != nil {
		return err
	}
	_, err := d.sql.Exec(`UPDATE tasks SET archived_at = NULL, updated_at = ? WHERE id = ? AND archived_at IS NOT NULL`, nowUTC(), id)
	return err
}

// ErrTaskConflict means a task is no longer eligible for the requested write.
var ErrTaskConflict = errors.New("task is not eligible or changed during preparation")

// TaskInput is a task's request and optional complete issue snapshot.
type TaskInput struct {
	Repo       string
	Text       string
	IssueRef   string
	IssueURL   string
	IssueTitle string
	IssueBody  string
}

// CreateTask records a task without a run.
func (d *DB) CreateTask(in TaskInput) (string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := insertTask(tx, in.Repo, in.Text, in.IssueRef, nowUTC())
	if err != nil {
		return "", err
	}
	if err := writeTask(tx, id, in); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// EditTask replaces the input of an unarchived task with no runs.
func (d *DB) EditTask(id string, in TaskInput) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := backlogTask(tx, id); err != nil {
		return err
	}
	if err := writeTask(tx, id, in); err != nil {
		return err
	}
	return tx.Commit()
}

func writeTask(tx *sql.Tx, id string, in TaskInput) error {
	var tracker, ref, url, title, body any
	if in.IssueRef != "" {
		tracker, ref, url, title, body = "github", in.IssueRef, in.IssueURL, in.IssueTitle, in.IssueBody
	}
	_, err := tx.Exec(`UPDATE tasks SET repo = ?, text = ?, tracker = ?, issue_ref = ?,
	 issue_url = ?, issue_title = ?, issue_body = ?, updated_at = ? WHERE id = ?`,
		in.Repo, in.Text, tracker, ref, url, title, body, nowUTC(), id)
	return err
}

func backlogTask(tx *sql.Tx, id string) (Task, error) {
	// Acquire the writer lock before reading eligibility.
	if _, err := tx.Exec(`UPDATE tasks SET id = id WHERE id = ?`, id); err != nil {
		return Task{}, err
	}
	return eligibleTask(tx, id)
}

// BacklogTask returns an unarchived task with no runs, or ErrTaskConflict.
func (d *DB) BacklogTask(id string) (Task, error) { return eligibleTask(d.sql, id) }

func eligibleTask(q queryer, id string) (Task, error) {
	task, err := getTask(q, id)
	if err != nil {
		return Task{}, err
	}
	var runs int
	if err := q.QueryRow(`SELECT COUNT(*) FROM runs WHERE task_id = ?`, id).Scan(&runs); err != nil {
		return Task{}, err
	}
	if task.ArchivedAt != nil || runs != 0 {
		return Task{}, ErrTaskConflict
	}
	return task, nil
}

func insertTask(tx *sql.Tx, repo, text, issue, at string) (string, error) {
	id := "task_" + randomHex(12)
	var tracker any
	if issue != "" {
		tracker = "github"
	}
	_, err := tx.Exec(`INSERT INTO tasks (id, repo, text, created_at, updated_at, tracker, issue_ref)
	 VALUES (?, ?, ?, ?, ?, ?, ?)`, id, repo, text, at, at, tracker, nullIfEmpty(issue))
	return id, err
}

// specIssue returns the issue ref recorded in a run spec, or "".
func specIssue(spec []byte) string {
	var s struct {
		Issue string `json:"issue"`
	}
	json.Unmarshal(spec, &s) // a missing or unreadable spec leaves Issue empty
	return s.Issue
}

// issueURLNumber returns the issue number of a GitHub issue URL.
func issueURLNumber(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] != "issues" {
		return "", false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 || strconv.Itoa(n) != parts[3] {
		return "", false
	}
	return parts[3], true
}

// issueRefsMatch compares canonical issue refs, ignoring case.
func issueRefsMatch(a, b string) bool { return strings.EqualFold(a, b) }

// issueSnapshotMatches checks a resolved GitHub URL against a numeric or URL ref.
func issueSnapshotMatches(ref, resolved string) bool {
	number, ok := issueURLNumber(resolved)
	return ok && (ref == number || issueRefsMatch(ref, resolved))
}

// FillTaskIssue fills missing snapshot fields only for the task's recorded issue.
// Taskless runs are ignored; conflicting refs or resolved URLs return an error.
func (d *DB) FillTaskIssue(runID, ref, url, title, body string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taskID string
	err = tx.QueryRow(`SELECT task_id FROM runs WHERE run_id = ? AND task_id IS NOT NULL`, runID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	task, err := getTask(tx, taskID)
	if err != nil {
		return err
	}
	if task.IssueRef == nil || !issueRefsMatch(ref, *task.IssueRef) || !issueSnapshotMatches(ref, url) ||
		(task.IssueURL != nil && *task.IssueURL != "" && !issueRefsMatch(*task.IssueURL, url)) {
		return fmt.Errorf("resolved issue conflicts with task %s", taskID)
	}
	_, err = tx.Exec(`UPDATE tasks SET
	 issue_url = CASE WHEN COALESCE(issue_url, '') = '' THEN NULLIF(?, '') ELSE issue_url END,
	 issue_title = CASE WHEN COALESCE(issue_title, '') = '' THEN NULLIF(?, '') ELSE issue_title END,
	 issue_body = CASE WHEN COALESCE(issue_body, '') = '' THEN NULLIF(?, '') ELSE issue_body END,
	 updated_at = ?
	 WHERE id = ?
	 AND ((COALESCE(issue_url, '') = '' AND ? != '')
	   OR (COALESCE(issue_title, '') = '' AND ? != '')
	   OR (COALESCE(issue_body, '') = '' AND ? != ''))`,
		url, title, body, nowUTC(), taskID, url, title, body)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// BackfillTasks attaches eligible legacy builds atomically; repeated passes are no-ops.
func (d *DB) BackfillTasks() error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT run_id, COALESCE(repo, ''), COALESCE(request, ''),
	 COALESCE((SELECT spec FROM iterations i WHERE i.run_id = runs.run_id AND i.iteration = 0), '')
	 FROM runs WHERE workflow = 'build' AND task_id IS NULL
	 AND (COALESCE(status, '') NOT IN ('` + StatusOK + `', '` + StatusFail + `', '` + StatusCancelled + `', '` + StatusLost + `') OR pr_state = 'open')`)
	if err != nil {
		return err
	}
	type candidate struct{ id, repo, text, spec string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.repo, &c.text, &c.spec); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		id, err := insertTask(tx, c.repo, c.text, specIssue([]byte(c.spec)), nowUTC())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE runs SET task_id = ? WHERE run_id = ? AND task_id IS NULL`, id, c.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
