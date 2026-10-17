package trace

import (
	"database/sql"
	"encoding/json"
)

// Task is a request and its optional issue snapshot, independent of its runs.
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

func scanTasks(rows *sql.Rows) ([]Task, error) {
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Repo, &t.Text, &t.ArchivedAt, &t.CreatedAt, &t.UpdatedAt,
			&t.Tracker, &t.IssueRef, &t.IssueURL, &t.IssueTitle, &t.IssueBody); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTask returns one task. A missing task is sql.ErrNoRows.
func (d *DB) GetTask(id string) (Task, error) {
	rows, err := d.sql.Query(taskColumns+` WHERE id = ?`, id)
	if err != nil {
		return Task{}, err
	}
	tasks, err := scanTasks(rows)
	if err != nil {
		return Task{}, err
	}
	if len(tasks) == 0 {
		return Task{}, sql.ErrNoRows
	}
	return tasks[0], nil
}

// Tasks lists tasks, including archived tasks. An empty repo selects all repos.
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
	return scanTasks(rows)
}

// TaskRuns returns a task's runs, newest submission first.
func (d *DB) TaskRuns(id string) ([]Row, error) {
	rows, err := d.sql.Query(runColumns+` FROM runs WHERE task_id = ? ORDER BY submitted_at DESC, run_id DESC`, id)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

func createTask(tx *sql.Tx, repo, text, issue, at string) (string, error) {
	id := NewRunID("task")
	var tracker any
	if issue != "" {
		tracker = "github"
	}
	_, err := tx.Exec(`INSERT INTO tasks (id, repo, text, created_at, updated_at, tracker, issue_ref)
	 VALUES (?, ?, ?, ?, ?, ?, ?)`, id, repo, text, at, at, tracker, nullIfEmpty(issue))
	return id, err
}

// FillTaskIssue fills empty snapshot fields through a run's task link.
// Missing links and mismatched references are harmless; nonempty fields are preserved.
func (d *DB) FillTaskIssue(runID, ref, url, title, body string) error {
	_, err := d.sql.Exec(`UPDATE tasks SET
	 issue_url = CASE WHEN COALESCE(issue_url, '') = '' AND ? != '' THEN ? ELSE issue_url END,
	 issue_title = CASE WHEN COALESCE(issue_title, '') = '' AND ? != '' THEN ? ELSE issue_title END,
	 issue_body = CASE WHEN COALESCE(issue_body, '') = '' AND ? != '' THEN ? ELSE issue_body END,
	 updated_at = ?
	 WHERE id = (SELECT task_id FROM runs WHERE run_id = ?)
	 AND issue_ref = ? AND ? != ''
	 AND ((COALESCE(issue_url, '') = '' AND ? != '')
	 OR (COALESCE(issue_title, '') = '' AND ? != '')
	 OR (COALESCE(issue_body, '') = '' AND ? != ''))`,
		url, url, title, title, body, body, nowUTC(), runID, ref, ref, url, title, body)
	return err
}

// BackfillTasks attaches eligible legacy builds to newly created tasks atomically.
func (d *DB) BackfillTasks() error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT r.run_id, COALESCE(r.repo, ''), COALESCE(r.request, ''),
	 COALESCE(r.submitted_at, ''), COALESCE(i.spec, '')
	 FROM runs r LEFT JOIN iterations i ON i.run_id = r.run_id AND i.iteration = 0
	 WHERE r.workflow = 'build' AND r.task_id IS NULL
	 AND (COALESCE(r.status, '') NOT IN (?, ?, ?, ?) OR r.pr_state = 'open')`,
		StatusOK, StatusFail, StatusCancelled, StatusLost)
	if err != nil {
		return err
	}
	type candidate struct {
		id, repo, request, submitted, spec string
	}
	var runs []candidate
	for rows.Next() {
		var r candidate
		if err := rows.Scan(&r.id, &r.repo, &r.request, &r.submitted, &r.spec); err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, r)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, r := range runs {
		var spec struct {
			Issue string `json:"issue"`
		}
		if err := json.Unmarshal([]byte(r.spec), &spec); err != nil {
			spec.Issue = ""
		}
		id, err := createTask(tx, r.repo, r.request, spec.Issue, r.submitted)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE runs SET task_id = ? WHERE run_id = ? AND task_id IS NULL`, id, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
