package trace

import (
	"fmt"
	"math"
	"testing"
)

// An empty database is a page, not an error: zero totals and empty rankings.
func TestOverviewEmpty(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	o, err := db.Overview()
	if err != nil {
		t.Fatal(err)
	}
	if o.Runs != 0 || o.Cost != 0 || len(o.TopRuns) != 0 || len(o.TopModels) != 0 || len(o.TopProjects) != 0 {
		t.Fatalf("empty database: %+v", o)
	}
	if o.At == "" {
		t.Error("no generation timestamp")
	}
}

// The whole database, not a page of it: totals count every run, the rankings
// are global and deterministic, and a run that used several models is counted
// once in the run total and once under each model it used.
func TestOverviewCoversEveryRun(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// More than the 50 /api/runs would return, so a total taken from that list
	// would come out wrong. Each run has three phases on one model and every
	// third run a fourth phase on another — the shape a build workflow makes.
	// Counting runs instead of phases would report 63 for kimi rather than 189.
	const runs, kimiPhases = 63, 3
	opusPhases := 0
	for i := 0; i < runs; i++ {
		id := fmt.Sprintf("r%02d", i)
		repo := "/repo/alpha"
		if i >= 42 {
			repo = "/repo/beta"
		}
		seedRun(t, db, id, repo)
		for seq := 1; seq <= kimiPhases; seq++ {
			p := NewPhase(id, seq, "work", "builder")
			if err := db.PhaseUpsert(p); err != nil {
				t.Fatal(err)
			}
			if err := db.RecordUsage(Usage{RunID: id, PhaseID: p.ID, Seq: 1,
				Provider: "moonshotai", Model: "kimi", Tokens: 10, Cost: 0.001}); err != nil {
				t.Fatal(err)
			}
		}
		// A phase pins one model, so a second model means a fourth phase.
		if i%3 == 0 {
			p := NewPhase(id, kimiPhases+1, "review", "reviewer")
			if err := db.PhaseUpsert(p); err != nil {
				t.Fatal(err)
			}
			if err := db.RecordUsage(Usage{RunID: id, PhaseID: p.ID, Seq: 1,
				Provider: "anthropic", Model: "opus", Tokens: 5, Cost: 0.002}); err != nil {
				t.Fatal(err)
			}
			opusPhases++
		}
		settle(t, db, id, "ok")
	}
	// A run with no usage at all is still a run.
	seedRun(t, db, "empty", "/repo")

	o, err := db.Overview()
	if err != nil {
		t.Fatal(err)
	}
	if o.Runs != runs+1 {
		t.Fatalf("runs = %d; want %d — counted through a join, not from runs", o.Runs, runs+1)
	}
	// 189 kimi phases at $0.001 and 21 opus phases at $0.002.
	want := runs*kimiPhases*0.001 + float64(opusPhases)*0.002
	if math.Abs(o.Cost-want) > costEpsilon {
		t.Fatalf("cost = $%v; want $%v", o.Cost, want)
	}

	if len(o.TopRuns) != topN {
		t.Fatalf("top runs = %d; want %d", len(o.TopRuns), topN)
	}
	// Every run spending 0.003 ties, so the ranking has to fall back on the run
	// id — and every call has to produce the same ten.
	for i := 1; i < len(o.TopRuns); i++ {
		if o.TopRuns[i-1].Cost < o.TopRuns[i].Cost {
			t.Fatalf("top runs are not descending: %+v", o.TopRuns)
		}
	}
	again, err := db.Overview()
	if err != nil {
		t.Fatal(err)
	}
	for i := range o.TopRuns {
		if again.TopRuns[i].ID != o.TopRuns[i].ID {
			t.Fatalf("ranking is not stable at %d: %s then %s", i, o.TopRuns[i].ID, again.TopRuns[i].ID)
		}
	}
	// The costliest runs are the ones with a fourth, pricier phase, and ties
	// break on run_id, so the first is deterministic.
	if o.TopRuns[0].ID != "r00" {
		t.Fatalf("the most expensive run is %s; want r00", o.TopRuns[0].ID)
	}

	if len(o.TopModels) != 2 {
		t.Fatalf("models = %+v", o.TopModels)
	}
	kimi, opus := o.TopModels[0], o.TopModels[1]
	// Phases, not runs: every run's three kimi phases each count, so counting
	// distinct runs here would report 63 instead of 189.
	if kimi.Provider+"/"+kimi.Model != "moonshotai/kimi" || kimi.Phases != runs*kimiPhases {
		t.Fatalf("kimi = %+v; want %d phases", kimi, runs*kimiPhases)
	}
	if opus.Provider+"/"+opus.Model != "anthropic/opus" || opus.Phases != opusPhases {
		t.Fatalf("opus = %+v; want %d phases", opus, opusPhases)
	}
	// Phase counts are not run counts and must never be read as one.
	if kimi.Phases+opus.Phases <= o.Runs {
		t.Fatalf("phase counts %d+%d look like run counts against %d runs",
			kimi.Phases, opus.Phases, o.Runs)
	}
	// Every dollar is attributed to a model now, so the two shares are the
	// whole of recorded spend.
	if share := kimi.Cost / o.Cost; math.Abs(kimi.Share-share) > 1e-9 || math.Abs(kimi.Share+opus.Share-1) > 1e-9 {
		t.Fatalf("shares %v %v of $%v", kimi.Share, opus.Share, o.Cost)
	}

	if len(o.TopProjects) != 3 {
		t.Fatalf("top projects = %+v", o.TopProjects)
	}
	for i := 1; i < len(o.TopProjects); i++ {
		if o.TopProjects[i-1].Cost < o.TopProjects[i].Cost {
			t.Fatalf("top projects are not descending: %+v", o.TopProjects)
		}
	}
	alpha, beta, emptyRepo := o.TopProjects[0], o.TopProjects[1], o.TopProjects[2]
	if alpha.Repo != "/repo/alpha" || alpha.Runs != 42 {
		t.Fatalf("alpha = %+v; want 42 runs", alpha)
	}
	if beta.Repo != "/repo/beta" || beta.Runs != 21 {
		t.Fatalf("beta = %+v; want 21 runs", beta)
	}
	if emptyRepo.Repo != "/repo" || emptyRepo.Runs != 1 || emptyRepo.Cost != 0 {
		t.Fatalf("empty repo = %+v; want 1 run, $0", emptyRepo)
	}
	if math.Abs(alpha.Cost+beta.Cost-o.Cost) > costEpsilon {
		t.Fatalf("project costs %v+%v != total %v", alpha.Cost, beta.Cost, o.Cost)
	}
	if share := alpha.Cost / o.Cost; math.Abs(alpha.Share-share) > 1e-9 || math.Abs(alpha.Share+beta.Share-1) > 1e-9 {
		t.Fatalf("project shares %v %v of $%v", alpha.Share, beta.Share, o.Cost)
	}

	if total, err := db.Total(); err != nil || total != runs+1 {
		t.Fatalf("Total() = %d, %v; want %d", total, err, runs+1)
	}
}

// BenchmarkOverview times the whole-database aggregate against a fixture with
// a realistic amount of raw event history behind it, which is what decides
// whether the Overview page can poll as often as a run's event log.
func BenchmarkOverview(b *testing.B) {
	db, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()

	// Payloads the size of a tool result, so a query that touched them would
	// show it. Overview must never read one.
	blob := make([]byte, 4096)
	for i := range blob {
		blob[i] = 'x'
	}
	const runs, eventsPerRun = 2000, 40
	models := []string{"kimi-k2.7-code", "kimi-k2.7", "opus", "sonnet", "haiku"}
	for i := 0; i < runs; i++ {
		id := fmt.Sprintf("bench%05d", i)
		seedRun(b, db, id, fmt.Sprintf("/repos/p%d", i%20))
		p := NewPhase(id, 1, "build", "builder")
		if err := db.PhaseUpsert(p); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < eventsPerRun; j++ {
			if err := db.Event(id, p.ID, "tool_call", "read", map[string]string{"result": string(blob)}); err != nil {
				b.Fatal(err)
			}
		}
		for seq := 1; seq <= 3; seq++ {
			if err := db.RecordUsage(Usage{RunID: id, PhaseID: p.ID, Seq: seq,
				Provider: "moonshotai", Model: models[(i+seq)%len(models)],
				Tokens: 1000, Cost: 0.0123}); err != nil {
				b.Fatal(err)
			}
		}
		settle(b, db, id, "ok")
	}
	b.Logf("fixture: %d runs, %d events, %d usage records",
		runs, runs*(eventsPerRun+3), runs*3)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Overview(); err != nil {
			b.Fatal(err)
		}
	}
}
