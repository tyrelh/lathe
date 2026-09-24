package trace

import (
	"math"
	"testing"
)

// costEpsilon is the precision two dollar figures are compared at: below the
// five decimals the UI shows, above the noise of summing float64 costs.
const costEpsilon = 1e-7

// seed writes a running run and a phase so usage has something to attach to.
// It writes the row directly because these tests are about accounting, not
// about the dispatch protocol that normally produces one.
func seed(t *testing.T, db *DB, runID string) string {
	t.Helper()
	seedRun(t, db, runID, "/repo")
	p := NewPhase(runID, 1, "build", "builder")
	if err := db.PhaseUpsert(p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// seedRun writes a running run row directly. These tests are about accounting
// and rendering, not about the dispatch protocol that normally produces one.
func seedRun(tb testing.TB, db *DB, runID, repo string) {
	tb.Helper()
	if _, err := db.sql.Exec(
		`INSERT INTO runs (run_id, workflow, repo, request, status, submitted_at, started_at)
		 VALUES (?, 'build', ?, 'do a thing', 'running', ?, ?)`,
		runID, repo, nowUTC(), nowUTC()); err != nil {
		tb.Fatal(err)
	}
}

// settle puts a seeded run into a terminal state.
func settle(tb testing.TB, db *DB, runID, status string) {
	tb.Helper()
	if _, err := db.sql.Exec(
		`UPDATE runs SET status = ?, ended_at = ? WHERE run_id = ?`, status, nowUTC(), runID); err != nil {
		tb.Fatal(err)
	}
}

func runCost(t *testing.T, db *DB, runID string) (int, float64) {
	t.Helper()
	var tokens int
	var cost float64
	if err := db.sql.QueryRow(`SELECT tokens, cost FROM runs WHERE run_id = ?`, runID).
		Scan(&tokens, &cost); err != nil {
		t.Fatal(err)
	}
	return tokens, cost
}

func one[T any](t *testing.T, db *DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := db.sql.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Two responses inside one attempt each charge the run as they land, and a
// retry of a response already committed charges nothing further — including
// leaving no second usage event behind for the detail view to double count.
func TestRecordUsageIsPerResponseAndIdempotent(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	phase := seed(t, db, "r1")

	u := Usage{RunID: "r1", PhaseID: phase, Agent: "builder", Attempt: 0, Seq: 1,
		Provider: "moonshotai", Model: "kimi", Tokens: 100, Cost: 0.01}
	if err := db.RecordUsage(u); err != nil {
		t.Fatal(err)
	}
	if tokens, cost := runCost(t, db, "r1"); tokens != 100 || cost != 0.01 {
		t.Fatalf("after one response: %d tok $%v; want 100 $0.01", tokens, cost)
	}

	u.Seq = 2
	u.Tokens, u.Cost = 50, 0.005
	if err := db.RecordUsage(u); err != nil {
		t.Fatal(err)
	}
	if tokens, cost := runCost(t, db, "r1"); tokens != 150 || math.Abs(cost-0.015) > costEpsilon {
		t.Fatalf("after two responses: %d tok $%v; want 150 $0.015", tokens, cost)
	}

	// The same response written again: same identity, so it is already charged.
	if err := db.RecordUsage(u); err != nil {
		t.Fatal(err)
	}
	if tokens, cost := runCost(t, db, "r1"); tokens != 150 || math.Abs(cost-0.015) > costEpsilon {
		t.Fatalf("a retry charged again: %d tok $%v", tokens, cost)
	}
	if n := one[int](t, db, `SELECT count(*) FROM usage WHERE run_id = 'r1'`); n != 2 {
		t.Fatalf("usage records = %d; want 2", n)
	}
	if n := one[int](t, db, `SELECT count(*) FROM events WHERE run_id = 'r1' AND type = 'usage'`); n != 2 {
		t.Fatalf("usage events = %d; want 2 — the retry left a duplicate", n)
	}
}

// A write that fails partway commits none of the three: no event, no typed
// record, no run total. The failure is injected by taking the usage table out
// from under the transaction after its event insert has already succeeded.
func TestRecordUsageIsAllOrNothing(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	phase := seed(t, db, "r1")

	if _, err := db.sql.Exec(`ALTER TABLE usage RENAME TO usage_hidden`); err != nil {
		t.Fatal(err)
	}
	err = db.RecordUsage(Usage{RunID: "r1", PhaseID: phase, Seq: 1, Tokens: 100, Cost: 0.01})
	if err == nil {
		t.Fatal("a failed usage write reported success")
	}
	if _, err := db.sql.Exec(`ALTER TABLE usage_hidden RENAME TO usage`); err != nil {
		t.Fatal(err)
	}

	if tokens, cost := runCost(t, db, "r1"); tokens != 0 || cost != 0 {
		t.Fatalf("a failed write still charged the run: %d tok $%v", tokens, cost)
	}
	if n := one[int](t, db, `SELECT count(*) FROM events WHERE type = 'usage'`); n != 0 {
		t.Fatalf("a failed write left %d usage events", n)
	}
	// And the next attempt still works, charging exactly once.
	if err := db.RecordUsage(Usage{RunID: "r1", PhaseID: phase, Seq: 1, Tokens: 100, Cost: 0.01}); err != nil {
		t.Fatal(err)
	}
	if tokens, _ := runCost(t, db, "r1"); tokens != 100 {
		t.Fatalf("the retry after a failure charged %d tokens; want 100", tokens)
	}
}

// A dashboard reader and a run writer share the database the way the dashboard
// and a worker do: WAL lets the reader in, and the reader only ever sees
// whole responses, never half of one.
func TestReaderAndWriterShareTheDatabase(t *testing.T) {
	root := t.TempDir()
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	phase := seed(t, db, "live")

	ro, err := OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	const responses = 40
	done := make(chan error, 1)
	go func() {
		for seq := 1; seq <= responses; seq++ {
			if err := db.RecordUsage(Usage{RunID: "live", PhaseID: phase, Seq: seq,
				Provider: "moonshotai", Model: "kimi", Tokens: 10, Cost: 0.001}); err != nil {
				done <- err
				return
			}
		}
		settle(t, db, "live", "ok")
		done <- nil
	}()

	for reads := 0; ; reads++ {
		o, err := ro.Overview()
		if err != nil {
			t.Fatal(err)
		}
		// A partially applied response would show the run total and the usage
		// records disagreeing, which is exactly what the transaction prevents.
		// Each ranking independently accounts for the full recorded spend, so
		// they are checked separately rather than added together.
		var sumModels, sumProviders float64
		for _, m := range o.TopModels {
			sumModels += m.Cost
		}
		for _, p := range o.TopProviders {
			sumProviders += p.Cost
		}
		if math.Abs(o.Cost-sumModels) > costEpsilon || math.Abs(o.Cost-sumProviders) > costEpsilon {
			t.Fatalf("read %d saw a half-applied response: total $%v, models $%v, providers $%v",
				reads, o.Cost, sumModels, sumProviders)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if _, cost := runCost(t, db, "live"); math.Abs(cost-responses*0.001) > costEpsilon {
				t.Fatalf("final cost = $%v; want $%v", cost, responses*0.001)
			}
			return
		default:
		}
	}
}
