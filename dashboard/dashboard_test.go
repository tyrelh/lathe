package dashboard

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
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

	"github.com/tyrelh/lathe/internal/agentconfig"
	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
)

// seededRoster returns a fresh home seeded from the shipped seed, the seed, and
// a roster that loads the home the way the manager does.
func seededRoster(t *testing.T) (string, fs.FS, func() (config.Config, error)) {
	t.Helper()
	seed, err := fs.Sub(os.DirFS(".."), "seed")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	return home, seed, func() (config.Config, error) { return agentconfig.Open(home, seed, "v1") }
}

// local is a request as the browser on this machine sends it: to the address
// the dashboard listens on.
func local(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, path, body)
	r.Host = Addr
	return r
}

func TestFavicon(t *testing.T) {
	h := Handler(t.TempDir(), "test", Actions{}, Sources{})
	page := httptest.NewRecorder()
	h.ServeHTTP(page, local("GET", "/", nil))
	if !strings.Contains(page.Body.String(), `<link rel="icon" href="/favicon.ico?v=1"`) {
		t.Fatal("page does not reference favicon")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, local("GET", "/favicon.ico?v=1", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/vnd.microsoft.icon" {
		t.Fatalf("favicon response: %d %v", w.Code, w.Header())
	}
	b := w.Body.Bytes()
	if len(b) < 54 || binary.LittleEndian.Uint16(b[0:2]) != 0 || binary.LittleEndian.Uint16(b[2:4]) != 1 || binary.LittleEndian.Uint16(b[4:6]) != 3 {
		t.Fatal("favicon must be an ICO with three sizes")
	}
	for i, size := range []int{16, 32, 48} {
		entry := b[6+i*16 : 6+(i+1)*16]
		length, offset := int(binary.LittleEndian.Uint32(entry[8:12])), int(binary.LittleEndian.Uint32(entry[12:16]))
		if offset < 54 || offset > len(b) || length > len(b)-offset {
			t.Fatalf("invalid ICO entry for %d", size)
		}
		img, err := png.Decode(bytes.NewReader(b[offset : offset+length]))
		if err != nil {
			t.Fatal(err)
		}
		if int(entry[0]) != size || int(entry[1]) != size || img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			t.Fatalf("incorrect dimensions for %d", size)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Fatalf("favicon %d lacks transparent padding", size)
		}
	}
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local(method, "/favicon.ico", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("reload %s: %d", method, w.Code)
		}
	}
	r := local("GET", "/favicon.ico", nil)
	r.Host = "evil.example"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("favicon bypassed host guard: %d", w.Code)
	}
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

// TestRoutes checks that a run written by the writer is readable through the
// read-only handles, and that ?after= returns only what the caller has not
// seen.
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

// TestEventPage pins index.html's PAGE to the server's per-poll event cap.
func TestEventPage(t *testing.T) {
	want := fmt.Sprintf("const PAGE = %d;", eventPage)
	if !bytes.Contains(indexHTML, []byte(want)) {
		t.Fatalf("index.html does not declare %q", want)
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

	result = errors.New("checkout has uncommitted changes")
	if code, out := send(build); code != http.StatusBadRequest || out["error"] != result.Error() {
		t.Errorf("checkout problem: %d %v, want 400 with the reason", code, out)
	}
}

// The run page's two writes: revise and cancel. They carry New run's
// protections, go through the actions handed in, and map refusals to
// statuses the page can explain. The run payload carries the iterations and
// the models each ran with.
// The recovery route keeps the browser protections, requires guidance,
// passes it to the shared submission and maps refusals to a conflict.
func TestResumeRoute(t *testing.T) {
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	id, attempt, token := claimed(t, db, "build", "/repos/alpha", "greet")
	if err := db.Complete(id, attempt, token, trace.StatusFail, "push failed"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var resumed []string
	var resumeErr error
	h := handler(ro, "dev", Actions{Resume: func(runID, guidance string) (int, error) {
		resumed = append(resumed, runID+": "+guidance)
		return 1, resumeErr
	}}, Sources{})
	post := func(origin, body string) (int, string) {
		r := local("POST", "/api/runs/"+id+"/recoveries", strings.NewReader(body))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, _ := post("", `{"guidance":"x"}`); code != http.StatusForbidden {
		t.Errorf("no origin: %d", code)
	}
	if code, _ := post("http://"+Addr, `{"guidance":"  "}`); code != http.StatusBadRequest {
		t.Errorf("empty guidance: %d", code)
	}
	if code, body := post("http://"+Addr, `{"guidance":" include the tests "}`); code != http.StatusCreated || !strings.Contains(body, `"iteration":1`) {
		t.Errorf("resume: %d %s", code, body)
	}
	if len(resumed) != 1 || resumed[0] != id+": include the tests" {
		t.Fatalf("resumed = %v", resumed)
	}
	resumeErr = fmt.Errorf("%w: still running", trace.ErrNotRecoverable)
	if code, _ := post("http://"+Addr, `{"guidance":"again"}`); code != http.StatusConflict {
		t.Errorf("refusal: %d", code)
	}
	r := local("POST", "/api/runs/"+id+"/recoveries", strings.NewReader(`{}`))
	r.Header.Set("Origin", "http://"+Addr)
	w := httptest.NewRecorder()
	handler(ro, "dev", Actions{}, Sources{}).ServeHTTP(w, r)
	if w.Code == http.StatusCreated {
		t.Error("the route exists without the action")
	}
}

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

// The run payload says where a writing iteration ran and where its result is
// kept, and the run still counts toward its original repository rather than
// the worktree it ran in.
func TestWorktreeReporting(t *testing.T) {
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	id, attempt, token := claimed(t, db, "build", "/repos/alpha", "greet")
	tree := filepath.Join(root, "worktrees", id, "iteration-0")
	if err := db.SetWorktree(id, attempt, tree, "0123456789abcdef", "active"); err != nil {
		t.Fatal(err)
	}
	if err := db.Complete(id, attempt, token, trace.StatusFail, "validation unresolved"); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(root, "runs", id, "result.patch")
	ref := "refs/lathe/runs/" + id + "/iteration-0"
	if err := db.SetPatch(id, 0, patch); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRecoveryRef(id, 0, ref); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorktreeState(id, 0, "pending", "removing the worktree: busy"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	h := handler(ro, "dev", Actions{}, Sources{})
	get := func(path string, v any) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", path, nil))
		if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
			t.Fatalf("%s: %v: %s", path, err, w.Body)
		}
	}

	var d struct {
		Iterations []trace.Iteration
	}
	get("/api/runs/"+id, &d)
	want := trace.Iteration{Worktree: tree, WorktreeState: "pending", CleanupError: "removing the worktree: busy",
		Patch: patch, RecoveryRef: ref}
	if len(d.Iterations) != 1 {
		t.Fatalf("iterations = %+v", d.Iterations)
	}
	if got := d.Iterations[0]; got.Worktree != want.Worktree || got.WorktreeState != want.WorktreeState ||
		got.CleanupError != want.CleanupError || got.Patch != want.Patch || got.RecoveryRef != want.RecoveryRef {
		t.Fatalf("iteration = %+v; want %+v", got, want)
	}

	var projects []trace.ProjectSpend
	get("/api/projects", &projects)
	if len(projects) != 1 || projects[0].Repo != "/repos/alpha" || projects[0].Runs != 1 {
		t.Fatalf("projects = %+v", projects)
	}
}

// The Agents page's two reads, against a home seeded from the shipped seed:
// the list is every agent then every routing phase with no bodies, and an
// agent's page carries the bodies as they are in the home.
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
	home, _, roster := seededRoster(t)
	h := handler(ro, "dev", Actions{}, Sources{Roster: roster})

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", path, nil))
		return w
	}
	type prompt struct {
		Role   string  `json:"role"`
		Key    string  `json:"key"`
		Path   string  `json:"path"`
		Body   *string `json:"body"`
		Seed   *string `json:"seed"`
		Hash   *string `json:"hash"`
		Edited bool    `json:"edited"`
		Drift  bool    `json:"drift"`
	}
	type settings struct {
		Provider string  `json:"provider"`
		Model    string  `json:"model"`
		Thinking string  `json:"thinking"`
		Timeout  string  `json:"timeout"`
		Key      string  `json:"key"`
		Body     *string `json:"body"`
		Seed     *string `json:"seed"`
		Hash     *string `json:"hash"`
	}
	type agent struct {
		Name             string    `json:"name"`
		Kind             string    `json:"kind"`
		Routes           string    `json:"routes"`
		RepoInstructions bool      `json:"repo_instructions"`
		Workflows        []string  `json:"workflows"`
		Config           *settings `json:"config"`
		Prompts          []prompt  `json:"prompts"`
	}

	w := get("/api/agents")
	if w.Code != 200 {
		t.Fatalf("GET /api/agents: %d %s", w.Code, w.Body)
	}
	var list []agent
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	cfg, err := roster()
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
	for _, field := range []string{`"body"`, `"seed"`, `"hash"`, `shared_with`} {
		if strings.Contains(w.Body.String(), field) {
			t.Errorf("the list carries %s", field)
		}
	}

	byName := map[string]agent{}
	for _, a := range list {
		byName[a.Name] = a
		if a.Workflows == nil {
			t.Errorf("%s: workflows is null", a.Name)
		}
		for _, p := range a.Prompts {
			if p.Edited || p.Drift || p.Path != filepath.Join(home, filepath.FromSlash(p.Key)) {
				t.Errorf("%s %s = %+v", a.Name, p.Role, p)
			}
		}
		if (a.Kind == "agent") != (a.Config != nil) {
			t.Errorf("%s config = %+v", a.Name, a.Config)
		}
	}
	if c := byName["planner"].Config; c == nil || c.Provider != "anthropic" || c.Model != "claude-opus-5-5" || c.Thinking != "medium" ||
		c.Timeout != "10m" || c.Key != "agents/planner/config.yaml" {
		t.Errorf("planner config = %+v", c)
	}
	reviewers := []string{"code-review-general", "code-review-security", "code-review-slop", "code-review-adversarial"}
	general := byName["code-review-general"]
	if general.Kind != "agent" || !general.RepoInstructions || len(general.Prompts) != 2 ||
		!reflect.DeepEqual(general.Workflows, []string{"implement", "build", "revise"}) {
		t.Errorf("code-review-general = %+v", general)
	}
	for _, name := range reviewers {
		a := byName[name]
		// Each reviewer has its own user prompt; none is shared.
		if len(a.Prompts) != 2 || a.Prompts[0].Role != "system" || a.Prompts[1].Role != "user" ||
			a.Prompts[1].Key != "agents/"+name+"/user.md" {
			t.Errorf("%s prompts = %+v", name, a.Prompts)
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
			len(a.Prompts) != 1 || a.Prompts[0].Role != "instructions" || a.Prompts[0].Key != "routing/"+want.Routes+".md" {
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
			b, err := os.ReadFile(filepath.Join(home, p.Key))
			if err != nil {
				t.Fatal(err)
			}
			if p.Body == nil || *p.Body != string(b) || p.Seed == nil || *p.Seed != string(b) || p.Hash == nil || *p.Hash != config.Hash(string(b)) {
				t.Errorf("%s %s body does not match %s", name, p.Role, p.Key)
			}
			d.Prompts[i].Body, d.Prompts[i].Seed, d.Prompts[i].Hash = nil, nil, nil
		}
		if d.Config != nil {
			if d.Config.Body == nil || d.Config.Hash == nil || *d.Config.Hash != config.Hash(*d.Config.Body) {
				t.Errorf("%s config = %+v", name, d.Config)
			}
			d.Config.Body, d.Config.Seed, d.Config.Hash = nil, nil, nil
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
