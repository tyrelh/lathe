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
	// 4: iterations. A revision extends a finished run rather than starting a
	// new one, so a run is now one or more submitted requests. The run row keeps
	// its identity, its totals and its original timestamps; each iteration keeps
	// its own request, configuration, lifecycle and publication evidence.
	// activity_at is the latest submission, which is what orders the queue and
	// the run lists. Every existing run becomes its own iteration 0 from what
	// its row recorded; nothing it did not record is filled in.
	`
CREATE TABLE IF NOT EXISTS iterations (
  run_id       TEXT NOT NULL REFERENCES runs,
  iteration    INTEGER NOT NULL,
  request      TEXT,
  spec         TEXT,
  status       TEXT,
  reason       TEXT,
  submitted_at TEXT,
  started_at   TEXT,
  ended_at     TEXT,
  base_commit  TEXT,
  commit_sha   TEXT,
  remote_sha   TEXT,
  PRIMARY KEY (run_id, iteration)
);
ALTER TABLE runs ADD COLUMN iteration INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN activity_at TEXT;
ALTER TABLE attempts ADD COLUMN iteration INTEGER NOT NULL DEFAULT 0;
ALTER TABLE phases ADD COLUMN iteration INTEGER NOT NULL DEFAULT 0;
UPDATE runs SET activity_at = submitted_at;
INSERT INTO iterations (run_id, iteration, request, spec, status, reason, submitted_at, started_at, ended_at)
  SELECT run_id, 0, request, spec, status, reason, submitted_at, started_at, ended_at FROM runs;
CREATE INDEX IF NOT EXISTS runs_activity ON runs (COALESCE(activity_at, '') DESC, run_id DESC);
CREATE INDEX IF NOT EXISTS runs_repo_activity ON runs (COALESCE(repo, ''), COALESCE(activity_at, '') DESC, run_id DESC);
`,
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
