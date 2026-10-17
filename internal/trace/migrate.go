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
	// column and index have been unwritten and unread since writing runs moved
	// to worktrees (ADR 0009); left in place rather than migrated away.
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
	// 5: managed worktrees and supervised launches. A writing iteration runs
	// in its own worktree, recorded with its cleanup state and where its
	// result was preserved. An attempt records how it executes; every attempt
	// before this one ran in the checkout. A launch is one supervised
	// subprocess: intent, then registration, then drained.
	`
ALTER TABLE iterations ADD COLUMN worktree_path TEXT;
ALTER TABLE iterations ADD COLUMN worktree_state TEXT;
ALTER TABLE iterations ADD COLUMN worktree_error TEXT;
ALTER TABLE iterations ADD COLUMN patch_path TEXT;
ALTER TABLE iterations ADD COLUMN recovery_ref TEXT;
ALTER TABLE attempts ADD COLUMN mode TEXT NOT NULL DEFAULT 'checkout';
CREATE TABLE IF NOT EXISTS launches (
  launch_id     TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL,
  attempt_id    TEXT NOT NULL,
  state         TEXT NOT NULL,
  argv          TEXT,
  host          TEXT,
  pid           INTEGER NOT NULL DEFAULT 0,
  pgid          INTEGER NOT NULL DEFAULT 0,
  endpoint      TEXT,
  token         TEXT,
  intent_at     TEXT,
  registered_at TEXT,
  drained_at    TEXT
);
CREATE INDEX IF NOT EXISTS launches_attempt ON launches (attempt_id, state);
CREATE INDEX IF NOT EXISTS iterations_worktree ON iterations (worktree_state);
`,
	// An iteration records why it exists: a submission, a revision, or a
	// recovery or restart of an earlier iteration, with the engineer's
	// guidance. The task an issue resolved to is kept, so a recovery never
	// refetches an issue body that may have changed. NULL kind predates this.
	`
ALTER TABLE iterations ADD COLUMN kind TEXT;
ALTER TABLE iterations ADD COLUMN source_iteration INTEGER;
ALTER TABLE iterations ADD COLUMN guidance TEXT;
ALTER TABLE iterations ADD COLUMN resolved_request TEXT;
ALTER TABLE iterations ADD COLUMN restored INTEGER NOT NULL DEFAULT 0;
ALTER TABLE iterations ADD COLUMN preservation TEXT;
`,
	// 7: observed pull request state; NULL means never checked.
	`
ALTER TABLE runs ADD COLUMN pr_state TEXT CHECK (pr_state IN ('open', 'merged', 'closed'));
ALTER TABLE runs ADD COLUMN pr_state_at TEXT;
`,
	// 8: tracked tasks and nullable run associations; backfill follows PR polling.
	`
CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  repo TEXT NOT NULL,
  text TEXT NOT NULL,
  archived_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  tracker TEXT CHECK (tracker IN ('github')),
  issue_ref TEXT,
  issue_url TEXT,
  issue_title TEXT,
  issue_body TEXT
);
ALTER TABLE runs ADD COLUMN task_id TEXT REFERENCES tasks(id);
CREATE INDEX tasks_repo ON tasks (repo, created_at DESC, id DESC);
CREATE INDEX runs_task ON runs (task_id, submitted_at DESC, run_id DESC);
`,
}

// migrate applies every migration the database has not seen yet.
//
// The version is read inside each migration's transaction, after it holds
// the write lock (the connection begins transactions IMMEDIATE), so another
// opener that migrated meanwhile is seen and its migration not repeated.
func migrate(db *sql.DB) error {
	for {
		done, err := migrateOne(db)
		if err != nil || done {
			return err
		}
	}
}

// migrateOne applies the next pending migration and its version in one
// transaction, and reports whether none was pending.
func migrateOne(db *sql.DB) (bool, error) {
	tx, err := retryContended(db.Begin)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return false, err
	}
	if version >= len(migrations) {
		return true, nil
	}
	if _, err := tx.Exec(migrations[version]); err != nil {
		return false, fmt.Errorf("migration %d: %w", version+1, err)
	}
	// PRAGMA takes no bound parameter, and version indexes a literal.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version+1)); err != nil {
		return false, fmt.Errorf("migration %d: %w", version+1, err)
	}
	return false, tx.Commit()
}
