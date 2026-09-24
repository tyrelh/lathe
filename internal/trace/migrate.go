package trace

import (
	"database/sql"
	"fmt"
)

// migrations run in order; PRAGMA user_version counts how many have been
// applied. Never edit one that has shipped — append a new one. Each runs in
// its own transaction, so a failure leaves the version where it was.
var migrations = []string{
	// 1: typed usage records.
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
`,
	// 2: exclusive marks a run that owns the checkout it executes in. The
	// duplicate check reads this instead of a workflow name, so which workflows
	// write is the workflow package's business and not the database's.
	`
ALTER TABLE runs ADD COLUMN exclusive INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS runs_exclusive ON runs (repo, exclusive, status);
`,
	// 3: project run pages read one repository in newest-first order.
	`CREATE INDEX IF NOT EXISTS runs_repo_recent ON runs (COALESCE(repo, ''), COALESCE(submitted_at, '') DESC, run_id DESC);`,
}

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
