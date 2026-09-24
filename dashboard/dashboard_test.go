package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/trace"
)

// claimed submits a run and takes it through dispatch and claim the way the
// manager and a worker do, leaving it running.
func claimed(t *testing.T, db *trace.DB, workflow, repo, request string) (id, attempt, token string) {
	t.Helper()
	id, err := db.Submit(trace.Request{Workflow: workflow, Repo: repo, Request: request, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	attempt = trace.NewAttemptID(id)
	if err := db.Reserve(id, attempt, ""); err != nil {
		t.Fatal(err)
	}
	token = trace.NewClaimToken()
	if _, err := db.Claim(attempt, token, "host", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	return id, attempt, token
}

// TestRoutes is Phase 6's done-when, minus the browser: a run written by the
// writer is readable through the read-only handles, and ?after= only returns
// what the caller has not seen — which is the whole polling contract.
func TestRoutes(t *testing.T) {
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	runID, _, _ := claimed(t, db, "scout", "/Users/tyrel/Projects/lathe", "where is the parser")
	p := trace.NewPhase(runID, 1, "scout", "scout")
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
	} else {
		for _, want := range []string{"<h2>phases</h2>", "<h2>costs</h2>", "<h2>details</h2>", "<h2>events</h2>"} {
			if !strings.Contains(body, want) {
				t.Errorf("/ missing %q", want)
			}
		}
		for _, unwanted := range []string{"<h2>run</h2>", "<h2>permissions</h2>", `id="permits"`} {
			if strings.Contains(body, unwanted) {
				t.Errorf("/ still contains %q", unwanted)
			}
		}
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
		// Finished each time round: a second outstanding build for one
		// checkout is rejected at submission, which is the point of the rule.
		id, attempt, token := claimed(t, db, "build", "/Users/tyrel/Projects/lathe", "add retries")
		p := trace.NewPhase(id, 1, "build", "builder")
		if err := db.PhaseUpsert(p); err != nil {
			t.Fatal(err)
		}
		if err := db.RecordUsage(trace.Usage{RunID: id, PhaseID: p.ID, Agent: "builder",
			Seq: 1, Provider: "moonshotai", Model: "kimi", Tokens: 100, Cost: 0.01}); err != nil {
			t.Fatal(err)
		}
		if err := db.Complete(id, attempt, token, trace.StatusOK, ""); err != nil {
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
	if o.Runs != 3 || len(o.TopRuns) != 3 || len(o.TopModels) != 1 || len(o.TopProjects) != 1 {
		t.Fatalf("overview = %+v", o)
	}
	if o.TopModels[0].Model != "kimi" || o.TopModels[0].Phases != 3 {
		t.Fatalf("models = %+v", o.TopModels)
	}
	if o.TopProjects[0].Repo != "/Users/tyrel/Projects/lathe" || o.TopProjects[0].Runs != 3 {
		t.Fatalf("projects = %+v", o.TopProjects)
	}
	var projects []trace.ProjectSpend
	if err := json.Unmarshal(do("/api/projects").Body.Bytes(), &projects); err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Runs != 3 {
		t.Fatalf("all projects = %+v", projects)
	}
	var project trace.ProjectSpend
	if err := json.Unmarshal(do("/api/projects/detail?repo=%2FUsers%2Ftyrel%2FProjects%2Flathe").Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if project.Runs != 3 || project.Cost != o.Cost || project.Tokens != o.Tokens {
		t.Fatalf("project totals = %+v, overview = %+v", project, o)
	}
	var page struct {
		Runs []trace.Row `json:"runs"`
		More bool        `json:"more"`
	}
	if err := json.Unmarshal(do("/api/projects/runs?repo=%2FUsers%2Ftyrel%2FProjects%2Flathe&n=2").Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 2 || !page.More {
		t.Fatalf("first project runs page = %+v", page)
	}
	next := "/api/projects/runs?repo=%2FUsers%2Ftyrel%2FProjects%2Flathe&n=2&before_at=" +
		page.Runs[1].Submitted + "&before_id=" + page.Runs[1].ID
	if err := json.Unmarshal(do(next).Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 || page.More {
		t.Fatalf("second project runs page = %+v", page)
	}
	for _, path := range []string{"/api/projects/detail?repo=%2Fmissing", "/api/projects/detail"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		want := 404
		if path == "/api/projects/detail" {
			want = 400
		}
		if w.Code != want {
			t.Errorf("GET %s: %d, want %d", path, w.Code, want)
		}
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
		!strings.Contains(body, `"top_models":[]`) || !strings.Contains(body, `"top_projects":[]`) ||
		!strings.Contains(body, `"runs":0`) {
		t.Errorf("empty overview = %s", body)
	}
}
