package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
)

// local is a request as the browser on this machine sends it: to the address
// the dashboard listens on.
func local(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Host = Addr
	return r
}

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
	h := handler(ro, "v0.2.0-test", Actions{}, Sources{})

	get := func(path string) string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}

	if body := get("/"); !strings.Contains(body, "<title>lathe</title>") {
		t.Error("/ did not serve the embedded page")
	} else {
		for _, want := range []string{`role="tablist"`, `aria-controls="phases-panel"`, `aria-controls="events-panel"`, `id="run-side"`, "<h2>costs</h2>"} {
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
	h.ServeHTTP(w, local("GET", "/api/runs/nope", nil))
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
	h := handler(ro, "v0.2.0", Actions{}, Sources{})

	do := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", path, nil))
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
	if o.Runs != 3 || len(o.TopRuns) != 3 || len(o.TopModels) != 1 || len(o.TopProviders) != 1 || len(o.TopProjects) != 1 {
		t.Fatalf("overview = %+v", o)
	}
	if o.TopProviders[0].Provider != "moonshotai" || o.TopProviders[0].Phases != 3 {
		t.Fatalf("providers = %+v", o.TopProviders)
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
		h.ServeHTTP(w, local("GET", path, nil))
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
	handler(fresh, "dev", Actions{}, Sources{}).ServeHTTP(w, local("GET", "/api/overview", nil))
	if body := w.Body.String(); !strings.Contains(body, `"top_runs":[]`) ||
		!strings.Contains(body, `"top_models":[]`) || !strings.Contains(body, `"top_providers":[]`) ||
		!strings.Contains(body, `"top_projects":[]`) ||
		!strings.Contains(body, `"runs":0`) {
		t.Errorf("empty overview = %s", body)
	}
}

// The New run route: who may call it, what it accepts, and how submission
// outcomes map to status codes. The stub stands in for the CLI's submission
// path, so nothing here touches a checkout.
func TestSubmitRun(t *testing.T) {
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Submit(trace.Request{Workflow: "scout", Repo: "/repos/alpha", Request: "look", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	var calls [][4]string
	var result error
	h := handler(ro, "dev", Actions{Submit: func(workflow, repo, prompt, issue string) (string, error) {
		calls = append(calls, [4]string{workflow, repo, prompt, issue})
		if result != nil {
			return "", result
		}
		return "20260923T120000Z_build_1", nil
	}}, Sources{})
	post := func(h http.Handler, host, origin, contentType, body string) (int, map[string]string) {
		r := local("POST", "/api/projects/runs", strings.NewReader(body))
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]string
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	send := func(body string) (int, map[string]string) {
		return post(h, Addr, "http://"+Addr, "application/json", body)
	}
	build := `{"repo":"/repos/alpha","workflow":"build","prompt":"  add retries  "}`

	if code, _ := post(handler(ro, "dev", Actions{}, Sources{}), Addr, "http://"+Addr, "application/json", build); code != http.StatusMethodNotAllowed {
		t.Errorf("nil submit: %d, want the POST route unregistered (405)", code)
	}

	w := httptest.NewRecorder()
	r := local("GET", "/api/meta", nil)
	r.Host = "evil.example:4700"
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("GET with a foreign Host: %d, want 403", w.Code)
	}

	for name, c := range map[string][3]string{
		"foreign host":    {"evil.example:4700", "http://evil.example:4700", "application/json"},
		"missing origin":  {Addr, "", "application/json"},
		"foreign origin":  {Addr, "http://evil.example", "application/json"},
		"cross-host":      {Addr, "http://localhost:4700", "application/json"},
		"form post":       {Addr, "http://" + Addr, "application/x-www-form-urlencoded"},
		"no content type": {Addr, "http://" + Addr, ""},
	} {
		if code, _ := post(h, c[0], c[1], c[2], build); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
	if code, _ := post(h, "localhost:4700", "http://localhost:4700", "application/json; charset=utf-8", build); code != http.StatusCreated {
		t.Errorf("localhost with a charset: %d, want 201", code)
	}

	big := `{"repo":"/repos/alpha","workflow":"build","prompt":"` + strings.Repeat("x", 64<<10) + `"}`
	if code, _ := send(big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", code)
	}

	for name, body := range map[string]string{
		"not json":         `{`,
		"unknown workflow": `{"repo":"/repos/alpha","workflow":"deploy","prompt":"x"}`,
		"both inputs":      `{"repo":"/repos/alpha","workflow":"build","prompt":"x","issue":"12"}`,
		"neither input":    `{"repo":"/repos/alpha","workflow":"build","prompt":"  "}`,
		"scout with issue": `{"repo":"/repos/alpha","workflow":"scout","issue":"12"}`,
		"unrecorded repo":  `{"repo":"/repos/beta","workflow":"build","prompt":"x"}`,
	} {
		if code, out := send(body); code != http.StatusBadRequest || out["error"] == "" {
			t.Errorf("%s: %d %v, want 400 with an error", name, code, out)
		}
	}

	calls = nil
	if code, out := send(build); code != http.StatusCreated || out["run_id"] != "20260923T120000Z_build_1" {
		t.Errorf("success: %d %v", code, out)
	}
	if len(calls) != 1 || calls[0] != [4]string{"build", "/repos/alpha", "add retries", ""} {
		t.Errorf("submit called with %v", calls)
	}
	if code, _ := send(`{"repo":"/repos/alpha","workflow":"plan","issue":"owner/repo#12"}`); code != http.StatusCreated {
		t.Errorf("issue run: %d", code)
	}

	result = fmt.Errorf("%w: 20260923T110000Z_build_1", trace.ErrDuplicateBuild)
	if code, out := send(build); code != http.StatusConflict || out["run_id"] != "20260923T110000Z_build_1" {
		t.Errorf("duplicate build: %d %v, want 409 naming the holder", code, out)
	}
	result = errors.New("checkout has uncommitted changes")
	if code, out := send(build); code != http.StatusBadRequest || out["error"] != result.Error() {
		t.Errorf("checkout problem: %d %v, want 400 with the reason", code, out)
	}
}

// The run page's two writes: revise and cancel. They carry New run's
// protections, go through the actions handed in, and map refusals to
// statuses the page can explain. The run payload carries the iterations and
// the models each ran with.
func TestReviseAndCancelRoutes(t *testing.T) {
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	id, attempt, token := claimed(t, db, "build", "/repos/alpha", "greet")
	if err := db.Complete(id, attempt, token, trace.StatusOK, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Revise(id, 0, "greet louder",
		[]byte(`{"roster":{"agents":{"planner":{"provider":"moonshotai","model":"kimi-k3"}}}}`)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	var revised, cancelled []string
	var reviseErr, cancelErr error
	h := handler(ro, "dev", Actions{
		Revise: func(runID, request string) (int, error) {
			revised = append(revised, runID+": "+request)
			return 2, reviseErr
		},
		Cancel: func(runID string) (string, error) {
			cancelled = append(cancelled, runID)
			return "running", cancelErr
		},
	}, Sources{})
	post := func(path, origin, contentType, body string) (int, map[string]any) {
		r := local("POST", path, strings.NewReader(body))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	send := func(path, body string) (int, map[string]any) {
		return post(path, "http://"+Addr, "application/json", body)
	}
	revise, cancel := "/api/runs/"+id+"/revisions", "/api/runs/"+id+"/cancel"

	for _, path := range []string{revise, cancel} {
		if code, _ := post(path, "", "application/json", `{"request":"x"}`); code != http.StatusForbidden {
			t.Errorf("%s without an Origin: %d", path, code)
		}
		if code, _ := post(path, "http://"+Addr, "text/plain", `{"request":"x"}`); code != http.StatusForbidden {
			t.Errorf("%s as a simple request: %d", path, code)
		}
	}
	if len(revised)+len(cancelled) != 0 {
		t.Fatal("a forbidden request reached an action")
	}
	if code, _ := send(revise, `{"request":"  "}`); code != http.StatusBadRequest {
		t.Errorf("empty request: %d", code)
	}
	if code, out := send(revise, `{"request":" louder still "}`); code != http.StatusCreated || out["iteration"] != float64(2) || out["run_id"] != id {
		t.Errorf("revise: %d %v", code, out)
	}
	if len(revised) != 1 || revised[0] != id+": louder still" {
		t.Errorf("revise called with %v", revised)
	}
	reviseErr = fmt.Errorf("%w: iteration 1 is still queued", trace.ErrNotRevisable)
	if code, out := send(revise, `{"request":"x"}`); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "still queued") {
		t.Errorf("not revisable: %d %v", code, out)
	}
	reviseErr = fmt.Errorf("%w: other_run", trace.ErrDuplicateBuild)
	if code, out := send(revise, `{"request":"x"}`); code != http.StatusConflict || out["run_id"] != "other_run" {
		t.Errorf("checkout held: %d %v", code, out)
	}
	reviseErr = errors.New("origin/feat/x is at 1234abcd")
	if code, out := send(revise, `{"request":"x"}`); code != http.StatusBadRequest || out["error"] != reviseErr.Error() {
		t.Errorf("checkout refusal: %d %v", code, out)
	}

	if code, out := send(cancel, `{}`); code != http.StatusOK || out["status"] != "running" || len(cancelled) != 1 {
		t.Errorf("cancel: %d %v %v", code, out, cancelled)
	}
	cancelErr = trace.ErrAlreadyDone
	if code, _ := send(cancel, `{}`); code != http.StatusConflict {
		t.Errorf("cancel after completion: %d", code)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, local("GET", "/api/runs/"+id, nil))
	var d struct {
		Run        trace.Row
		Iterations []struct {
			Iteration int               `json:"iteration"`
			Request   string            `json:"request"`
			Status    string            `json:"status"`
			Models    map[string]string `json:"models"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Run.Iteration != 1 || len(d.Iterations) != 2 || d.Iterations[1].Request != "greet louder" ||
		d.Iterations[1].Status != "queued" || d.Iterations[1].Models["planner"] != "moonshotai/kimi-k3" ||
		d.Iterations[0].Status != "ok" {
		t.Fatalf("run payload = %s", w.Body)
	}
	if strings.Contains(w.Body.String(), `"roster"`) {
		t.Fatal("the run payload resends the whole configuration snapshot")
	}

	// Without the actions, neither route exists.
	bare := handler(ro, "dev", Actions{}, Sources{})
	for _, path := range []string{revise, cancel} {
		r := local("POST", path, strings.NewReader(`{}`))
		r.Header.Set("Origin", "http://"+Addr)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		bare.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
			t.Errorf("%s on a read-only dashboard: %d", path, w.Code)
		}
	}
}

// The Agents page's two reads, against the shipped roster and prompts: the
// list is every agent then every routing phase with no bodies, and an agent's
// page carries the bodies as they are on disk.
func TestAgentRoutes(t *testing.T) {
	root := t.TempDir()
	if err := trace.Init(root); err != nil {
		t.Fatal(err)
	}
	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	assets := os.DirFS("..")
	h := handler(ro, "dev", Actions{}, Sources{Roster: func() (config.Config, error) { return config.Load(assets) }})

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", path, nil))
		return w
	}
	type prompt struct {
		Role       string   `json:"role"`
		Key        string   `json:"key"`
		Body       *string  `json:"body"`
		SharedWith []string `json:"shared_with"`
	}
	type agent struct {
		Name             string   `json:"name"`
		Kind             string   `json:"kind"`
		Routes           string   `json:"routes"`
		RepoInstructions bool     `json:"repo_instructions"`
		Workflows        []string `json:"workflows"`
		Prompts          []prompt `json:"prompts"`
	}

	w := get("/api/agents")
	if w.Code != 200 {
		t.Fatalf("GET /api/agents: %d %s", w.Code, w.Body)
	}
	var list []agent
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(assets)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range cfg.Agents {
		names = append(names, a.Name)
	}
	names = append(names, "route-plan", "route-build")
	var got []string
	for _, a := range list {
		got = append(got, a.Name)
	}
	if len(list) != 15 || !reflect.DeepEqual(got, names) {
		t.Fatalf("agents = %v; want %v", got, names)
	}
	if strings.Contains(w.Body.String(), `"body"`) {
		t.Error("the list carries prompt bodies")
	}

	byName := map[string]agent{}
	for _, a := range list {
		byName[a.Name] = a
		if a.Workflows == nil {
			t.Errorf("%s: workflows is null", a.Name)
		}
		for _, p := range a.Prompts {
			if p.SharedWith == nil {
				t.Errorf("%s %s: shared_with is null", a.Name, p.Role)
			}
		}
	}
	reviewers := []string{"code-review-general", "code-review-security", "code-review-slop", "code-review-adversarial"}
	general := byName["code-review-general"]
	if general.Kind != "agent" || !general.RepoInstructions || len(general.Prompts) != 2 ||
		!reflect.DeepEqual(general.Workflows, []string{"implement", "build", "revise"}) {
		t.Errorf("code-review-general = %+v", general)
	}
	for _, name := range reviewers {
		a := byName[name]
		if len(a.Prompts) != 2 || a.Prompts[0].Role != "system" || len(a.Prompts[0].SharedWith) != 0 {
			t.Errorf("%s prompts = %+v", name, a.Prompts)
			continue
		}
		user := a.Prompts[1]
		var others []string
		for _, r := range reviewers {
			if r != name {
				others = append(others, r)
			}
		}
		if user.Role != "user" || user.Key != "prompts/code-review/user.md" || !reflect.DeepEqual(user.SharedWith, others) {
			t.Errorf("%s user prompt = %+v; want shared with %v", name, user, others)
		}
	}
	if byName["brancher"].RepoInstructions {
		t.Error("brancher reports repo instructions on")
	}
	for name, want := range map[string]agent{
		"route-plan":  {Routes: "planner", Workflows: []string{"plan", "implement", "build", "revise"}},
		"route-build": {Routes: "builder", Workflows: []string{"implement", "build", "revise"}},
	} {
		a := byName[name]
		if a.Kind != "routing" || a.Routes != want.Routes || a.RepoInstructions || !reflect.DeepEqual(a.Workflows, want.Workflows) ||
			len(a.Prompts) != 1 || a.Prompts[0].Role != "instructions" || a.Prompts[0].Key != "prompts/routing/"+want.Routes+".md" {
			t.Errorf("%s = %+v", name, a)
		}
	}

	for _, name := range []string{"planner", "code-review-slop", "route-build"} {
		w := get("/api/agents/" + name)
		if w.Code != 200 {
			t.Fatalf("GET /api/agents/%s: %d %s", name, w.Code, w.Body)
		}
		var d agent
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		for i, p := range d.Prompts {
			b, err := os.ReadFile(filepath.Join("..", p.Key))
			if err != nil {
				t.Fatal(err)
			}
			if p.Body == nil || *p.Body != string(b) {
				t.Errorf("%s %s body does not match %s", name, p.Role, p.Key)
			}
			d.Prompts[i].Body = nil
		}
		if want := byName[name]; !reflect.DeepEqual(d, want) {
			t.Errorf("%s detail = %+v; want the list row %+v plus bodies", name, d, want)
		}
	}

	if w := get("/api/agents/nope"); w.Code != http.StatusNotFound {
		t.Errorf("unknown agent: %d, want 404", w.Code)
	}
	r := local("GET", "/api/agents", nil)
	r.Host = "evil.example:4700"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("foreign host: %d, want 403", w.Code)
	}

	broken := handler(ro, "dev", Actions{}, Sources{Roster: func() (config.Config, error) { return config.Config{}, errors.New("bad roster") }})
	for _, path := range []string{"/api/agents", "/api/agents/planner"} {
		w := httptest.NewRecorder()
		broken.ServeHTTP(w, local("GET", path, nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s with a roster error: %d, want 500", path, w.Code)
		}
	}

	bare := handler(ro, "dev", Actions{}, Sources{})
	for _, path := range []string{"/api/agents", "/api/agents/planner"} {
		w := httptest.NewRecorder()
		bare.ServeHTTP(w, local("GET", path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s without a roster: %d, want 404", path, w.Code)
		}
	}
}

// The Agents page's two writes, end to end through real overrides in a temp
// data root: a save or reset answers with the agent as its page now loads it,
// a stale hash is a conflict that writes nothing, and the key always comes
// from the roster.
func TestPromptWriteRoutes(t *testing.T) {
	root := t.TempDir()
	if err := trace.Init(root); err != nil {
		t.Fatal(err)
	}
	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	dir, assets := t.TempDir(), os.DirFS("..")
	roster := func() (config.Config, error) { return config.Load(config.Overlay(dir, assets)) }
	h := handler(ro, "dev", Actions{
		SavePrompt:  func(key, body, baseHash string) error { return config.SavePrompt(dir, assets, key, body, baseHash) },
		ResetPrompt: func(key string) error { return config.ResetPrompt(dir, key) },
	}, Sources{Roster: roster})

	type prompt struct {
		Role         string `json:"role"`
		Key          string `json:"key"`
		Body         string `json:"body"`
		Builtin      string `json:"builtin"`
		Hash         string `json:"hash"`
		OverridePath string `json:"override_path"`
		Overridden   bool   `json:"overridden"`
	}
	type agent struct {
		Name    string   `json:"name"`
		Prompts []prompt `json:"prompts"`
	}
	request := func(h http.Handler, method, path, host, origin, contentType, body string) *httptest.ResponseRecorder {
		r := local(method, path, strings.NewReader(body))
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	send := func(method, path, body string) (int, map[string]any, agent) {
		w := request(h, method, path, Addr, "http://"+Addr, "application/json", body)
		var out map[string]any
		var a agent
		json.Unmarshal(w.Body.Bytes(), &out)
		json.Unmarshal(w.Body.Bytes(), &a)
		return w.Code, out, a
	}
	save := func(path, body, hash string) (int, map[string]any, agent) {
		b, _ := json.Marshal(map[string]string{"body": body, "base_hash": hash})
		return send("PUT", path, string(b))
	}
	detail := func(name string) agent {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", "/api/agents/"+name, nil))
		var a agent
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || w.Code != 200 {
			t.Fatalf("GET /api/agents/%s: %d %s", name, w.Code, w.Body)
		}
		return a
	}
	overridePath := filepath.Join(dir, "prompts", "planner", "system.md")
	noOverride := func(when string) {
		t.Helper()
		if _, err := os.Stat(overridePath); !os.IsNotExist(err) {
			t.Fatalf("%s: override exists: %v", when, err)
		}
	}

	sys := detail("planner").Prompts[0]
	builtin := sys.Body
	if sys.Role != "system" || sys.Overridden || sys.OverridePath != overridePath || sys.Builtin != builtin || sys.Hash != config.PromptHash(builtin) {
		t.Fatalf("planner system before any save = %+v", sys)
	}
	path := "/api/agents/planner/prompts/system"

	for _, method := range []string{"PUT", "DELETE"} {
		body := `{"body":"x","base_hash":"` + sys.Hash + `"}`
		for _, c := range []struct{ host, origin, contentType string }{
			{Addr, "", "application/json"}, {Addr, "http://evil.example", "application/json"},
			{Addr, "http://" + Addr, "text/plain"}, {"evil.example:4700", "http://evil.example:4700", "application/json"},
		} {
			if w := request(h, method, path, c.host, c.origin, c.contentType, body); w.Code != http.StatusForbidden {
				t.Errorf("%s to %s from %q as %s: %d", method, c.host, c.origin, c.contentType, w.Code)
			}
		}
	}
	noOverride("forbidden writes")

	for _, c := range []struct {
		path, body, hash string
		code             int
		error            string
	}{
		{path, " \n\t", sys.Hash, http.StatusBadRequest, "a prompt can't be empty; reset it to go back to the built-in"},
		{path, "plan", "", http.StatusBadRequest, ""},
		{"/api/agents/planner/prompts/user", "no placeholder", sys.Hash, http.StatusBadRequest, "a user prompt has to contain {{request}}"},
		{"/api/agents/route-plan/prompts/user", "{{request}}", sys.Hash, http.StatusNotFound, ""},
		{"/api/agents/nope/prompts/system", "plan", sys.Hash, http.StatusNotFound, ""},
	} {
		code, out, _ := save(c.path, c.body, c.hash)
		if code != c.code || out["error"] == nil || (c.error != "" && out["error"] != c.error) {
			t.Errorf("PUT %s %q: %d %v", c.path, c.body, code, out)
		}
	}
	for _, p := range []string{"/api/agents/route-plan/prompts/user", "/api/agents/nope/prompts/system"} {
		if code, out, _ := send("DELETE", p, `{}`); code != http.StatusNotFound || out["error"] == nil {
			t.Errorf("DELETE %s: %d %v", p, code, out)
		}
	}
	noOverride("refused writes")

	// Saved verbatim, and the response is the agent page's own payload.
	edited := "  Plan carefully.\n\n"
	code, _, a := save(path, edited, sys.Hash)
	if code != http.StatusOK || !reflect.DeepEqual(a, detail("planner")) {
		t.Fatalf("save: %d %+v", code, a)
	}
	if got := a.Prompts[0]; !got.Overridden || got.Body != edited || got.Builtin != builtin || got.Hash != config.PromptHash(edited) {
		t.Errorf("planner system after save = %+v", got)
	}
	if b, err := os.ReadFile(overridePath); err != nil || string(b) != edited {
		t.Fatalf("override file = %q, %v", b, err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, local("GET", "/api/agents", nil))
	var list []agent
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 15 {
		t.Fatalf("list after save: %v %s", err, w.Body)
	}
	for _, a := range list {
		for _, p := range a.Prompts {
			if want := a.Name == "planner" && p.Role == "system"; p.Overridden != want || p.OverridePath != filepath.Join(dir, p.Key) {
				t.Errorf("list after save: %s %s = %+v", a.Name, p.Role, p)
			}
		}
	}
	for _, field := range []string{`"hash"`, `"builtin"`, `"body"`} {
		if strings.Contains(w.Body.String(), field) {
			t.Errorf("the list carries %s", field)
		}
	}

	// A save against the hash the page loaded before someone else's edit is
	// refused with what is there now.
	code, out, _ := save(path, "Plan something else.", sys.Hash)
	if code != http.StatusConflict || out["error"] != "this prompt changed since you loaded it" ||
		out["body"] != edited || out["hash"] != config.PromptHash(edited) || out["overridden"] != true {
		t.Errorf("stale save: %d %v", code, out)
	}
	if b, _ := os.ReadFile(overridePath); string(b) != edited {
		t.Errorf("a stale save wrote %q", b)
	}

	// Saving the built-in back removes the override rather than shadowing it.
	if code, _, a := save(path, builtin, config.PromptHash(edited)); code != http.StatusOK || a.Prompts[0].Overridden || a.Prompts[0].Body != builtin {
		t.Errorf("save equal to the built-in: %d %+v", code, a)
	}
	noOverride("saving the built-in")

	if code, _, _ := save(path, edited, sys.Hash); code != http.StatusOK {
		t.Fatalf("second save: %d", code)
	}
	for range 2 {
		if code, _, a := send("DELETE", path, `{}`); code != http.StatusOK || a.Prompts[0].Overridden || a.Prompts[0].Body != builtin {
			t.Errorf("reset: %d %+v", code, a)
		}
	}
	noOverride("reset")

	// Without the actions, neither route exists.
	bare := handler(ro, "dev", Actions{}, Sources{Roster: roster})
	for _, method := range []string{"PUT", "DELETE"} {
		w := request(bare, method, path, Addr, "http://"+Addr, "application/json", `{"body":"x","base_hash":"`+sys.Hash+`"}`)
		if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
			t.Errorf("%s on a read-only dashboard: %d", method, w.Code)
		}
	}
	noOverride("a read-only dashboard")
}
