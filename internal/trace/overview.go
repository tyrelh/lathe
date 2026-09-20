package trace

import "database/sql"

// topN is how many rows each Overview ranking carries. Ten is what the
// dashboard shows, and bounding it in SQL is what keeps a poll cheap on a
// database with years of runs in it.
const topN = 10

// ModelSpend is one provider/model pair's share of recorded spend. Phases
// counts the distinct phases that spent on it, which is the unit that actually
// pins a model: a run moves between agents and so between models, a phase does
// not. Counting runs would collapse a run's three phases on one model into one.
type ModelSpend struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Tokens   int     `json:"tokens"`
	Cost     float64 `json:"cost"`
	Share    float64 `json:"share"`
	Phases   int     `json:"phases"`
}

// Overview is every lifetime figure the dashboard's first page shows, across
// every repository in the database.
type Overview struct {
	Runs      int          `json:"runs"`
	Tokens    int          `json:"tokens"`
	Cost      float64      `json:"cost"`
	TopRuns   []Row        `json:"top_runs"`
	TopModels []ModelSpend `json:"top_models"`
	// Unattributed is recorded spend no model can be named for: legacy run
	// totals the usage events did not explain. It counts toward Cost.
	Unattributed float64 `json:"unattributed"`
	// Unexplained is how many runs reconciled to a nonzero signed difference,
	// and Malformed how many usage events would not parse. Both are coverage
	// signals: the totals are what was recorded, not a provider invoice.
	Unexplained int    `json:"unexplained_runs"`
	Malformed   int    `json:"malformed_usage"`
	At          string `json:"at"`
}

// Overview reads every figure inside one transaction, so the totals and the
// rankings describe the same database rather than two moments of it.
func (d *DB) Overview() (Overview, error) {
	var o Overview
	tx, err := d.sql.Begin()
	if err != nil {
		return o, err
	}
	defer tx.Rollback()

	// Counted from runs, never through a join to usage: a run that used three
	// models is still one run, and a run with no usage at all is still one run.
	if err := tx.QueryRow(
		`SELECT count(*), COALESCE(SUM(tokens), 0), COALESCE(SUM(cost), 0), COALESCE(SUM(unattributed), 0)
		 FROM runs`).Scan(&o.Runs, &o.Tokens, &o.Cost, &o.Unattributed); err != nil {
		return o, err
	}
	if err := tx.QueryRow(
		`SELECT COALESCE(SUM(abs(difference) > 0), 0), COALESCE(SUM(malformed), 0)
		 FROM usage_reconcile`).Scan(&o.Unexplained, &o.Malformed); err != nil {
		return o, err
	}

	// Ties break on run_id, which is unique — so the same database always
	// produces the same ten rows in the same order.
	rows, err := tx.Query(
		`SELECT run_id, workflow, repo, request, status, started_at, ended_at, tokens, cost
		 FROM runs ORDER BY cost DESC, run_id LIMIT ?`, topN)
	if err != nil {
		return o, err
	}
	o.TopRuns, err = scanRows(rows)
	if err != nil {
		return o, err
	}

	rows, err = tx.Query(
		`SELECT COALESCE(provider, ''), COALESCE(model, ''),
		        SUM(tokens), SUM(cost), COUNT(DISTINCT phase_id)
		 FROM usage GROUP BY provider, model
		 ORDER BY SUM(cost) DESC, provider, model LIMIT ?`, topN)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	o.TopModels = []ModelSpend{}
	for rows.Next() {
		var m ModelSpend
		if err := rows.Scan(&m.Provider, &m.Model, &m.Tokens, &m.Cost, &m.Phases); err != nil {
			return o, err
		}
		if o.Cost > 0 {
			m.Share = m.Cost / o.Cost
		}
		o.TopModels = append(o.TopModels, m)
	}
	if err := rows.Err(); err != nil {
		return o, err
	}

	o.At = nowUTC()
	return o, tx.Commit()
}

// Total is how many runs the database holds, which is what the Runs page
// compares its displayed count against.
func (d *DB) Total() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT count(*) FROM runs`).Scan(&n)
	return n, err
}

func scanRows(rows *sql.Rows) ([]Row, error) {
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		var ended sql.NullString
		if err := rows.Scan(&r.ID, &r.Workflow, &r.Repo, &r.Request, &r.Status, &r.Started, &ended, &r.Tokens, &r.Cost); err != nil {
			return nil, err
		}
		r.Ended = ended.String
		out = append(out, r)
	}
	return out, rows.Err()
}
