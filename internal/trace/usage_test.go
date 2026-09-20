package trace

import (
	"fmt"
	"math"
	"testing"
)

// seed writes a run and a phase so usage has something to attach to.
func seed(t *testing.T, db *DB, runID string) string {
	t.Helper()
	if err := db.RunStart(runID, "build", "/repo", "do a thing"); err != nil {
		t.Fatal(err)
	}
	p := NewPhase(runID, 1, "build", "agent", "builder")
	if err := db.PhaseUpsert(p); err != nil {
		t.Fatal(err)
	}
	return p.ID
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

	// Run completion settles the lifecycle and nothing else.
	if err := db.RunFinish("r1", "ok"); err != nil {
		t.Fatal(err)
	}
	if tokens, cost := runCost(t, db, "r1"); tokens != 150 || math.Abs(cost-0.015) > costEpsilon {
		t.Fatalf("RunFinish charged the run again: %d tok $%v", tokens, cost)
	}
	// Spend committed before an interruption survives it.
	if err := db.RunInterrupt("r1"); err != nil {
		t.Fatal(err)
	}
	if _, cost := runCost(t, db, "r1"); math.Abs(cost-0.015) > costEpsilon {
		t.Fatalf("interrupt lost committed spend: $%v", cost)
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

// legacyRun writes a v0.1-shaped run: attempt-level usage events and a run
// total written once at the end, with no typed usage records at all.
func legacyRun(t *testing.T, db *DB, runID string, total float64, payloads ...string) {
	t.Helper()
	phase := seed(t, db, runID)
	for _, p := range payloads {
		if _, err := db.sql.Exec(
			`INSERT INTO events (run_id, phase_id, type, name, payload, at)
			 VALUES (?, ?, 'usage', 'builder', ?, '2026-09-01T00:00:00Z')`,
			runID, phase, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.sql.Exec(`UPDATE runs SET status='ok', tokens=1, cost=? WHERE run_id=?`, total, runID); err != nil {
		t.Fatal(err)
	}
	// Migration has already seen this database, so the run must be put back on
	// the backfill's list the way an older lathe would have left it.
	if _, err := db.sql.Exec(`DELETE FROM usage_reconcile WHERE run_id = ?`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM usage WHERE run_id = ?`, runID); err != nil {
		t.Fatal(err)
	}
}

func usagePayloadJSON(attempt int, tokens int, cost float64, model string) string {
	return fmt.Sprintf(`{"attempt":%d,"tokens":%d,"cost":%v,"model":%q}`, attempt, tokens, cost, model)
}

// The backfill attributes what the events explain, keeps what they do not as
// unattributed legacy spend, never subtracts, and produces the same answer
// however many times it runs.
func TestBackfillReconcilesLegacyRuns(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Explained exactly: two attempts summing to the stored total.
	legacyRun(t, db, "exact", 0.03,
		usagePayloadJSON(0, 100, 0.01, "kimi"), usagePayloadJSON(1, 200, 0.02, "kimi"))
	// The stored total exceeds the events — spend nothing can name a model for.
	legacyRun(t, db, "short", 0.05, usagePayloadJSON(0, 100, 0.01, "kimi"))
	// The events exceed the stored total, which is what an interrupted run
	// looks like: the events win and the difference is kept, not cancelled.
	legacyRun(t, db, "interrupted", 0.0, usagePayloadJSON(0, 100, 0.04, "kimi"))
	// Rounding noise must not read as unexplained spend.
	legacyRun(t, db, "noise", 0.1+0.2,
		usagePayloadJSON(0, 1, 0.1, "kimi"), usagePayloadJSON(1, 2, 0.2, "kimi"))
	// A model that was never recorded, and an event that will not parse.
	legacyRun(t, db, "unknown", 0.02, usagePayloadJSON(0, 10, 0.02, ""), `{not json`)

	for pass := 0; pass < 2; pass++ {
		if err := db.Backfill(); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			run          string
			cost         float64
			unattributed float64
			difference   float64
			legacy       float64
		}{
			{"exact", 0.03, 0, 0, 0.03},
			{"short", 0.05, 0.04, 0.04, 0.05},
			{"interrupted", 0.04, 0, -0.04, 0},
			{"noise", 0.3, 0, 0, 0.1 + 0.2},
			{"unknown", 0.02, 0, 0, 0.02},
		} {
			tokens, cost := runCost(t, db, c.run)
			un := one[float64](t, db, `SELECT unattributed FROM runs WHERE run_id = ?`, c.run)
			diff := one[float64](t, db, `SELECT difference FROM usage_reconcile WHERE run_id = ?`, c.run)
			legacy := one[float64](t, db, `SELECT legacy_total FROM usage_reconcile WHERE run_id = ?`, c.run)
			if math.Abs(cost-c.cost) > costEpsilon || math.Abs(un-c.unattributed) > costEpsilon ||
				math.Abs(diff-c.difference) > costEpsilon {
				t.Fatalf("pass %d %s: cost $%v unattributed $%v difference $%v; want $%v $%v $%v",
					pass, c.run, cost, un, diff, c.cost, c.unattributed, c.difference)
			}
			if tokens < 1 {
				t.Fatalf("pass %d %s: tokens = %d", pass, c.run, tokens)
			}
			// The original total is kept whatever happened to the displayed one,
			// so the evidence outlives the adjustment.
			if math.Abs(legacy-c.legacy) > costEpsilon {
				t.Fatalf("pass %d %s: legacy_total = $%v; want $%v", pass, c.run, legacy, c.legacy)
			}
		}
		// Attribution comes only from the events, so the unattributed excess is
		// never charged to a model, and repeating the pass never duplicates one.
		if n := one[int](t, db, `SELECT count(*) FROM usage WHERE run_id = 'short'`); n != 1 {
			t.Fatalf("pass %d: 'short' has %d usage records; want 1", pass, n)
		}
		if got := one[float64](t, db, `SELECT SUM(cost) FROM usage WHERE run_id = 'short'`); math.Abs(got-0.01) > costEpsilon {
			t.Fatalf("pass %d: unattributed spend was charged to a model: $%v", pass, got)
		}
		if n := one[int](t, db, `SELECT malformed FROM usage_reconcile WHERE run_id = 'unknown'`); n != 1 {
			t.Fatalf("pass %d: malformed = %d; want 1", pass, n)
		}
		if m := one[string](t, db, `SELECT model FROM usage WHERE run_id = 'unknown'`); m != "" {
			t.Fatalf("pass %d: a missing model was invented as %q", pass, m)
		}
	}
}

// A dashboard reader and a run writer share the database the way lathe dash
// and lathe build do: WAL lets the reader in, and the reader only ever sees
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
		done <- db.RunFinish("live", "ok")
	}()

	for reads := 0; ; reads++ {
		o, err := ro.Overview()
		if err != nil {
			t.Fatal(err)
		}
		// A partially applied response would show the run total and the usage
		// records disagreeing, which is exactly what the transaction prevents.
		var attributed float64
		for _, m := range o.TopModels {
			attributed += m.Cost
		}
		if math.Abs(o.Cost-attributed-o.Unattributed) > costEpsilon {
			t.Fatalf("read %d saw a half-applied response: total $%v, models $%v",
				reads, o.Cost, attributed)
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
