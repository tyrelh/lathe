package trace

import "database/sql"

// Projects returns every repository with a run, including repositories whose
// runs have no recorded spend. Shares use the same lifetime total as Overview.
func (d *DB) Projects() ([]ProjectSpend, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var total float64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(cost), 0) FROM runs`).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT COALESCE(repo, ''), COUNT(*), COALESCE(SUM(tokens), 0), COALESCE(SUM(cost), 0)
		FROM runs GROUP BY COALESCE(repo, '') ORDER BY SUM(cost) DESC, COALESCE(repo, '')`)
	if err != nil {
		return nil, err
	}
	projects, err := scanProjects(rows, total)
	if err != nil {
		return nil, err
	}
	return projects, tx.Commit()
}

// Project returns lifetime totals for one exact repository path.
func (d *DB) Project(repo string) (ProjectSpend, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return ProjectSpend{}, err
	}
	defer tx.Rollback()
	var total float64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(cost), 0) FROM runs`).Scan(&total); err != nil {
		return ProjectSpend{}, err
	}
	rows, err := tx.Query(`SELECT COALESCE(repo, ''), COUNT(*), COALESCE(SUM(tokens), 0), COALESCE(SUM(cost), 0)
		FROM runs WHERE COALESCE(repo, '') = ? GROUP BY COALESCE(repo, '')`, repo)
	if err != nil {
		return ProjectSpend{}, err
	}
	projects, err := scanProjects(rows, total)
	if err != nil {
		return ProjectSpend{}, err
	}
	if len(projects) == 0 {
		return ProjectSpend{}, sql.ErrNoRows
	}
	return projects[0], tx.Commit()
}

func scanProjects(rows *sql.Rows, total float64) ([]ProjectSpend, error) {
	defer rows.Close()
	projects := []ProjectSpend{}
	for rows.Next() {
		var p ProjectSpend
		if err := rows.Scan(&p.Repo, &p.Runs, &p.Tokens, &p.Cost); err != nil {
			return nil, err
		}
		if total > 0 {
			p.Share = p.Cost / total
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// ProjectRuns pages through every run for an exact repository, most recently
// active first. The cursor is the last displayed row's ordering pair, so new
// runs cannot shift an offset; a run revised mid-page moves to the top, where
// the dashboard's merge by ID absorbs it.
func (d *DB) ProjectRuns(repo, beforeAt, beforeID string, limit int) ([]Row, bool, error) {
	if limit < 1 {
		limit = 50
	}
	query := runColumns + ` FROM runs WHERE COALESCE(repo, '') = ?`
	args := []any{repo}
	if beforeID != "" {
		query += ` AND (COALESCE(activity_at, '') < ? OR
			(COALESCE(activity_at, '') = ? AND run_id < ?))`
		args = append(args, beforeAt, beforeAt, beforeID)
	}
	query += ` ORDER BY COALESCE(activity_at, '') DESC, run_id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	runs, err := scanRows(rows)
	if err != nil {
		return nil, false, err
	}
	more := len(runs) > limit
	if more {
		runs = runs[:limit]
	}
	return runs, more, nil
}
