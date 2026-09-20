package trace

import "encoding/json"

// Usage is one model response's recorded spend. Seq is the response's position
// within its invocation, assigned before the write so a retry lands on the
// same identity rather than charging the run twice.
type Usage struct {
	RunID    string
	PhaseID  string
	Agent    string
	Attempt  int
	Seq      int
	Provider string
	Model    string
	Tokens   int
	Cost     float64
}

// RecordUsage writes the usage event, its typed record, and the run-total
// increment in one transaction: either the response is charged once and
// visible to the dashboard, or nothing about it is recorded at all.
//
// A response already charged — a retry after a commit the caller did not see —
// commits nothing, including the duplicate event. Callers must propagate the
// error rather than letting spend disappear.
func (d *DB) RecordUsage(u Usage) error {
	payload, err := json.Marshal(map[string]any{
		"attempt": u.Attempt, "seq": u.Seq, "tokens": u.Tokens, "cost": u.Cost,
		"provider": u.Provider, "model": u.Model,
	})
	if err != nil {
		return err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	at := nowUTC()
	res, err := tx.Exec(
		`INSERT INTO events (run_id, phase_id, type, name, payload, at) VALUES (?, ?, 'usage', ?, ?, ?)`,
		u.RunID, nullIfEmpty(u.PhaseID), u.Agent, string(payload), at)
	if err != nil {
		return err
	}
	eventID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	res, err = tx.Exec(
		`INSERT OR IGNORE INTO usage
		   (event_id, run_id, phase_id, attempt, response_seq, agent, provider, model, tokens, cost, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, u.RunID, u.PhaseID, u.Attempt, u.Seq, u.Agent, u.Provider, u.Model, u.Tokens, u.Cost, at)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return nil // already charged; the rollback drops the duplicate event too
	}

	if _, err := tx.Exec(
		`UPDATE runs SET tokens = tokens + ?, cost = cost + ? WHERE run_id = ?`,
		u.Tokens, u.Cost, u.RunID); err != nil {
		return err
	}
	return tx.Commit()
}
