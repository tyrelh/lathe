package trace

import "fmt"

// PRCandidates returns all runs whose PR state is not final.
func (d *DB) PRCandidates() ([]Row, error) {
	rows, err := d.sql.Query(`SELECT run_id, repo, iteration FROM runs WHERE pr_state IS NULL OR pr_state = 'open' ORDER BY run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var row Row
		if err := rows.Scan(&row.ID, &row.Repo, &row.Iteration); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ObservePR records a state and observation time unless the run is already final.
func (d *DB) ObservePR(runID, state string) error {
	switch state {
	case "open", "merged", "closed":
	default:
		return fmt.Errorf("invalid PR state %q", state)
	}
	_, err := d.sql.Exec(`UPDATE runs SET pr_state = ?, pr_state_at = ? WHERE run_id = ? AND (pr_state IS NULL OR pr_state = 'open')`, state, nowUTC(), runID)
	return err
}
