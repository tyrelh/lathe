package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/trace"
)

// TestRoutes is Phase 6's done-when, minus the browser: a run written by the
// writer is readable through the read-only handles, and ?after= only returns
// what the caller has not seen — which is the whole polling contract.
func TestRoutes(t *testing.T) {
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "20260916T120000Z_scout_1"
	if err := db.RunStart(runID, "scout", "/Users/tyrel/Projects/lathe", "where is the parser"); err != nil {
		t.Fatal(err)
	}
	p := trace.NewPhase(runID, 1, "scout", "agent", "scout")
	if err := db.PhaseUpsert(p); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read", "grep"} {
		if err := db.Event(runID, p.ID, "tool_call", name, map[string]string{"path": "main.go"}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	h := handler(ro, "v0.2.0-test")

	get := func(path string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}

	if body := get("/"); !strings.Contains(body, "<title>lathe</title>") {
		t.Error("/ did not serve the embedded page")
	}
	if body := get("/api/runs"); !strings.Contains(body, runID) {
		t.Errorf("/api/runs missing the run: %s", body)
	}

	var d struct {
		Run    trace.Row
		Phases []trace.PhaseRow
		Events []trace.EventRow
	}
	if err := json.Unmarshal([]byte(get("/api/runs/"+runID)), &d); err != nil {
		t.Fatal(err)
	}
	if d.Run.Status != "running" || len(d.Phases) != 1 || len(d.Events) != 2 {
		t.Fatalf("run %+v, %d phases, %d events", d.Run, len(d.Phases), len(d.Events))
	}
	if d.Phases[0].Status != "fail" {
		t.Errorf("an unfinished phase should read 'fail', got %q", d.Phases[0].Status)
	}

	// The cursor is the dashboard's whole state: past the last event seen,
	// a poll returns nothing rather than the log over again.
	var tail struct{ Events []trace.EventRow }
	last := d.Events[len(d.Events)-1].ID
	if err := json.Unmarshal([]byte(get("/api/runs/"+runID+"?after="+strconv.FormatInt(last, 10))), &tail); err != nil {
		t.Fatal(err)
	}
	if len(tail.Events) != 0 {
		t.Errorf("after=%d returned %d events", last, len(tail.Events))
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/runs/nope", nil))
	if w.Code != 404 {
		t.Errorf("unknown run: want 404, got %d", w.Code)
	}
}

// Exercise pagination, rendering, and state retention in the shipped script.
func TestClient(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	if out, err := exec.Command("node", "index_test.cjs").CombinedOutput(); err != nil {
		t.Fatalf("dashboard client: %v\n%s", err, out)
	}
}

// The Overview route, the version route, and the total-count metadata the Runs
// status bar needs — plus the initialization step that lets the dashboard open
// a database it has never seen, or none at all.
func TestOverviewAndMeta(t *testing.T) {
	root := t.TempDir()
	// No database yet: Init is what makes this a first-use empty dashboard
	// rather than "run something first".
	if err := trace.Init(root); err != nil {
		t.Fatal(err)
	}
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("20260920T12000%dZ_build_1", i)
		if err := db.RunStart(id, "build", "/Users/tyrel/Projects/lathe", "add retries"); err != nil {
			t.Fatal(err)
		}
		p := trace.NewPhase(id, 1, "build", "agent", "builder")
		if err := db.PhaseUpsert(p); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordUsage(trace.Usage{RunID: id, PhaseID: p.ID, Agent: "builder",
			Seq: 1, Provider: "moonshotai", Model: "kimi", Tokens: 100, Cost: 0.01}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	h := handler(ro, "v0.2.0")

	do := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		return w
	}

	if body := do("/api/meta").Body.String(); !strings.Contains(body, `"version":"v0.2.0"`) {
		t.Errorf("/api/meta = %s", body)
	}
	// The body stays the bare array every existing consumer reads; the total
	// rides alongside it.
	runs := do("/api/runs?n=2")
	if got := runs.Header().Get("X-Total-Runs"); got != "3" {
		t.Errorf("X-Total-Runs = %q; want 3", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(runs.Body.String()), "[") {
		t.Errorf("/api/runs is no longer an array: %s", runs.Body)
	}

	var o trace.Overview
	if err := json.Unmarshal(do("/api/overview").Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if o.Runs != 3 || len(o.TopRuns) != 3 || len(o.TopModels) != 1 {
		t.Fatalf("overview = %+v", o)
	}
	if o.TopModels[0].Model != "kimi" || o.TopModels[0].Phases != 3 {
		t.Fatalf("models = %+v", o.TopModels)
	}
	// A dashboard opened on nothing at all: Init creates the database, and the
	// empty rankings marshal as [] so the client can map over them.
	blank := t.TempDir()
	if err := trace.Init(blank); err != nil {
		t.Fatal(err)
	}
	fresh, err := trace.OpenRO(blank)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	w := httptest.NewRecorder()
	handler(fresh, "dev").ServeHTTP(w, httptest.NewRequest("GET", "/api/overview", nil))
	if body := w.Body.String(); !strings.Contains(body, `"top_runs":[]`) ||
		!strings.Contains(body, `"top_models":[]`) || !strings.Contains(body, `"runs":0`) {
		t.Errorf("empty overview = %s", body)
	}
}
