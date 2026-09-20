package trace

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// migrations run in order; PRAGMA user_version counts how many have been
// applied. Never edit one that has shipped — append a new one. Each runs in
// its own transaction, so a failure leaves the version where it was.
var migrations = []string{
	// 1: typed usage records and the legacy reconciliation ledger.
	`
CREATE TABLE IF NOT EXISTS usage (
  usage_id     INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id     INTEGER UNIQUE,
  run_id       TEXT NOT NULL,
  phase_id     TEXT,
  attempt      INTEGER NOT NULL DEFAULT 0,
  response_seq INTEGER NOT NULL DEFAULT 0,
  agent        TEXT,
  provider     TEXT,
  model        TEXT,
  tokens       INTEGER NOT NULL DEFAULT 0,
  cost         REAL    NOT NULL DEFAULT 0,
  at           TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS usage_response ON usage (run_id, phase_id, attempt, response_seq);
CREATE INDEX IF NOT EXISTS usage_run ON usage (run_id);
CREATE INDEX IF NOT EXISTS usage_model ON usage (provider, model);

CREATE TABLE IF NOT EXISTS usage_reconcile (
  run_id       TEXT PRIMARY KEY,
  legacy_total REAL    NOT NULL DEFAULT 0,
  event_sum    REAL    NOT NULL DEFAULT 0,
  difference   REAL    NOT NULL DEFAULT 0,
  malformed    INTEGER NOT NULL DEFAULT 0,
  at           TEXT
);

ALTER TABLE runs ADD COLUMN unattributed REAL DEFAULT 0;
`,
}

// costEpsilon is the precision two dollar figures are compared at. It sits
// below the five decimals the UI shows and above the noise of summing a
// handful of float64 costs, so rounding never reads as unexplained spend.
const costEpsilon = 1e-7

// migrate applies every migration the database has not seen yet.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA takes no bound parameter, and i is an index into a literal.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// legacyUsage is one recorded usage event as the backfill reads it back.
type legacyUsage struct {
	eventID int64
	runID   string
	phaseID string
	agent   string
	at      string
	payload sql.NullString
}

// usagePayload is what a usage event carries. Old events have no provider and
// may have no model; new ones carry both plus the response sequence.
type usagePayload struct {
	Attempt  int     `json:"attempt"`
	Seq      int     `json:"seq"`
	Tokens   int     `json:"tokens"`
	Cost     float64 `json:"cost"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
}

// Backfill turns recorded usage events into typed usage records and reconciles
// them against each run's stored total. It is repeatable by construction: a
// run that already has a reconciliation record is left exactly as it was, so
// re-running it never re-attributes spend or charges anything twice.
//
// Attribution comes only from events. Where a run's stored total exceeds what
// its events explain, the excess is kept as unattributed legacy spend — it
// still counts toward totals, but never toward a named model. Where the events
// explain more than the stored total, the events win and the signed difference
// is preserved for inspection rather than being cancelled out.
func (d *DB) Backfill() error {
	// ponytail: a deferred transaction that upgrades to a write can lose to
	// another lathe process, and busy_timeout does not retry that one. Backfill
	// is idempotent, so retrying the whole pass is the entire recovery story.
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = d.backfill(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return err
}

func (d *DB) backfill() error {
	pending, err := d.unreconciled()
	if err != nil || len(pending) == 0 {
		return err
	}
	events, err := d.legacyUsage()
	if err != nil {
		return err
	}
	byRun := map[string][]legacyUsage{}
	for _, e := range events {
		byRun[e.runID] = append(byRun[e.runID], e)
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	at := nowUTC()
	for _, run := range pending {
		var costSum float64
		var tokenSum, malformed int
		// The sequence a legacy attempt-level record gets is its position
		// within its own attempt, which is what a response-level record uses
		// too — so a re-read of a v0.2 run lands on the same identity.
		seq := map[string]int{}
		for _, e := range byRun[run.ID] {
			var p usagePayload
			if !e.payload.Valid || json.Unmarshal([]byte(e.payload.String), &p) != nil {
				malformed++
				continue
			}
			key := fmt.Sprintf("%s\x00%d", e.phaseID, p.Attempt)
			seq[key]++
			n := p.Seq
			if n == 0 {
				n = seq[key]
			}
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO usage
				   (event_id, run_id, phase_id, attempt, response_seq, agent, provider, model, tokens, cost, at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				e.eventID, e.runID, e.phaseID, p.Attempt, n, e.agent, p.Provider, p.Model, p.Tokens, p.Cost, e.at); err != nil {
				return err
			}
			costSum += p.Cost
			tokenSum += p.Tokens
		}

		diff := run.Cost - costSum
		unattributed := 0.0
		if diff > costEpsilon {
			unattributed = diff
		}
		tokens := run.Tokens
		if tokenSum > tokens {
			tokens = tokenSum
		}
		if _, err := tx.Exec(
			`UPDATE runs SET cost = ?, tokens = ?, unattributed = ? WHERE run_id = ?`,
			costSum+unattributed, tokens, unattributed, run.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO usage_reconcile (run_id, legacy_total, event_sum, difference, malformed, at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			run.ID, run.Cost, costSum, signed(diff), malformed, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// signed drops differences that are only float noise, so a reconciled run does
// not read as unexplained because two sums took different addition orders.
func signed(diff float64) float64 {
	if math.Abs(diff) <= costEpsilon {
		return 0
	}
	return diff
}

// unreconciled is every run the backfill has not settled yet. Runs and events
// are read outside the writing transaction: this connection is serial, so
// holding a cursor open across writes would deadlock against itself.
func (d *DB) unreconciled() ([]Row, error) {
	rows, err := d.sql.Query(
		`SELECT run_id, tokens, cost FROM runs
		 WHERE run_id NOT IN (SELECT run_id FROM usage_reconcile)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Tokens, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) legacyUsage() ([]legacyUsage, error) {
	rows, err := d.sql.Query(
		`SELECT event_id, run_id, COALESCE(phase_id, ''), name, payload, at
		 FROM events WHERE type = 'usage'
		   AND run_id NOT IN (SELECT run_id FROM usage_reconcile)
		 ORDER BY event_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []legacyUsage
	for rows.Next() {
		var e legacyUsage
		if err := rows.Scan(&e.eventID, &e.runID, &e.phaseID, &e.agent, &e.payload, &e.at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
