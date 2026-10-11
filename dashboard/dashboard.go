// Package dashboard serves a view of the trace database and the roster's
// prompts through an embedded page and JSON endpoints, plus routes that write
// through paths handed in by the CLI: queue a run for a project, revise a run,
// cancel one, save or reset a prompt, and edit an agent's model choice.
package dashboard

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workflow"
)

//go:embed index.html
var indexHTML []byte

// Addr is a literal rather than a setting: the server has no authentication
// and serves prompts and source. Remote viewing is an SSH tunnel.
const Addr = "127.0.0.1:4700"

// hosts are the names the dashboard answers to. Anything else in the Host
// header is a page on another origin reaching the loopback listener through
// DNS rebinding, and gets nothing — reads included, since they serve source.
var hosts = map[string]bool{Addr: true, "localhost:4700": true}

// Submit queues one run and returns its ID once the database has accepted it.
// The manager hands in the CLI's own submission path, which is what keeps
// configuration, checkout and issue handling out of this package.
type Submit func(workflow, repo, prompt, issue string) (string, error)

// Actions are the dashboard's writes, each the CLI's own path handed in by the
// manager: Submit queues a run, Revise appends an iteration to one and returns
// its number, Cancel asks one to stop and returns its status. SavePrompt writes
// the configuration file at key unless baseHash is stale, ResetPrompt restores
// it to its seed unless baseHash is stale, and SaveConfig sets an agent's provider, model and thinking
// unless baseHash is stale. A nil action registers no route, which is what
// keeps a read-only dashboard read-only.
type Actions struct {
	Submit      Submit
	Revise      func(runID, request string) (int, error)
	Cancel      func(runID string) (string, error)
	SavePrompt  func(key, body, baseHash string) error
	ResetPrompt func(key, baseHash string) error
	SaveConfig  func(agent, provider, model, thinking, baseHash string) error
}

// Sources are the dashboard's reads from outside the trace database, each
// called per request: Roster loads the agent roster and its prompts. A nil
// source registers no route.
type Sources struct {
	Roster func() (config.Config, error)
}

// maxSubmitBody bounds a POST body; a prompt is text someone typed.
const maxSubmitBody = 64 << 10

// Handler is the dashboard as something to mount: the manager serves it
// alongside the scheduler, so there is no second process and no second
// listener. The database is opened read-only — a UI bug cannot write — and
// created first if it is not there yet, so a fresh install serves an empty
// dashboard rather than an error telling you to go start a run. Submissions go
// through actions, never through this handle; sources serve what the database
// does not hold.
func Handler(dataRoot, version string, actions Actions, sources Sources) http.Handler {
	if err := trace.Init(dataRoot); err != nil {
		return failed(err)
	}
	db, err := trace.OpenRO(dataRoot)
	if err != nil {
		return failed(err)
	}
	return handler(db, version, actions, sources)
}

func failed(err error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	})
}

// handler is the whole server minus the listener, which is what lets a test
// exercise the routes without binding a port.
func handler(db *trace.DB, version string, actions Actions, sources Sources) http.Handler {
	mux := http.NewServeMux()
	if sources.Roster != nil {
		mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
			// The list leaves the bodies to the agent page.
			agents, err := agentViews(sources.Roster, false)
			writeJSON(w, agents, err)
		})
		mux.HandleFunc("GET /api/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
			a, err := findAgent(sources.Roster, r.PathValue("name"), true)
			if errors.Is(err, errNoAgent) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, a, err)
		})
		// One lock for every write: a save checks the hash it was loaded
		// against, and nothing else from this page may land in between.
		var mu sync.Mutex
		if actions.SavePrompt != nil {
			mux.HandleFunc("PUT /api/agents/{name}/prompts/{role}", savePrompt(&mu, sources.Roster, actions.SavePrompt))
		}
		if actions.ResetPrompt != nil {
			mux.HandleFunc("DELETE /api/agents/{name}/prompts/{role}", resetPrompt(&mu, sources.Roster, actions.ResetPrompt))
		}
		if actions.SaveConfig != nil {
			mux.HandleFunc("PUT /api/agents/{name}/config", saveConfig(&mu, sources.Roster, actions.SaveConfig))
		}
	}
	if actions.Submit != nil {
		mux.HandleFunc("POST /api/projects/runs", submitRun(db, actions.Submit))
	}
	if actions.Revise != nil {
		mux.HandleFunc("POST /api/runs/{id}/revisions", reviseRun(actions.Revise))
	}
	if actions.Cancel != nil {
		mux.HandleFunc("POST /api/runs/{id}/cancel", cancelRun(actions.Cancel))
	}
	mux.HandleFunc("GET /api/meta", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"version": version}, nil)
	})
	mux.HandleFunc("GET /api/overview", func(w http.ResponseWriter, r *http.Request) {
		o, err := db.Overview()
		writeJSON(w, o, err)
	})
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		projects, err := db.Projects()
		writeJSON(w, projects, err)
	})
	mux.HandleFunc("GET /api/projects/detail", func(w http.ResponseWriter, r *http.Request) {
		repo, ok := projectParam(w, r)
		if !ok {
			return
		}
		project, err := db.Project(repo)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "no such project", http.StatusNotFound)
			return
		}
		writeJSON(w, project, err)
	})
	mux.HandleFunc("GET /api/projects/runs", func(w http.ResponseWriter, r *http.Request) {
		repo, ok := projectParam(w, r)
		if !ok {
			return
		}
		beforeAt, beforeID := r.URL.Query().Get("before_at"), r.URL.Query().Get("before_id")
		_, hasAt := r.URL.Query()["before_at"]
		_, hasID := r.URL.Query()["before_id"]
		if hasAt != hasID || (hasID && beforeID == "") {
			http.Error(w, "incomplete run cursor", http.StatusBadRequest)
			return
		}
		limit := intParam(r, "n", 50)
		if limit < 1 || limit > 100 {
			limit = 50
		}
		runs, more, err := db.ProjectRuns(repo, beforeAt, beforeID, limit)
		writeJSON(w, map[string]any{"runs": runs, "more": more}, err)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Recent(intParam(r, "n", 50))
		// The total rides in a header rather than wrapping the array: the body
		// stays exactly the list every existing consumer already reads.
		if total, terr := db.Total(); terr == nil {
			w.Header().Set("X-Total-Runs", strconv.Itoa(total))
		}
		writeJSON(w, rows, err)
	})
	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		run, err := db.Get(id)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "no such run", http.StatusNotFound)
			return
		}
		if err != nil {
			writeJSON(w, nil, err)
			return
		}
		phases, err := db.Phases(id)
		if err != nil {
			writeJSON(w, nil, err)
			return
		}
		iterations, err := db.Iterations(id)
		if err != nil {
			writeJSON(w, nil, err)
			return
		}
		// The configuration each iteration ran with, as the models it gave each
		// agent: the whole snapshot carries every prompt, which is too much to
		// resend every poll.
		type iterationView struct {
			trace.Iteration
			Models map[string]string `json:"models"`
		}
		views := make([]iterationView, len(iterations))
		for i, it := range iterations {
			views[i] = iterationView{it, worker.Models(it.Spec)}
		}
		// The cursor bounds one poll; whatever is left arrives on the next one.
		// index.html knows this number: a full page is how it tells that a
		// settled run still has events to fetch before it stops polling.
		events, err := db.Events(id, int64(intParam(r, "after", 0)), 500)
		writeJSON(w, map[string]any{"run": run, "phases": phases, "events": events, "iterations": views}, err)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// agentView is one agent or routing phase as the Agents page shows it.
// Workflows is every workflow that spawns the agent, or that routes it.
type agentView struct {
	config.AgentPrompts
	Workflows []string `json:"workflows"`
}

// agentViews loads the roster and returns its agents and routing phases in
// Prompts order, with bodies when bodies is set.
func agentViews(roster func() (config.Config, error), bodies bool) ([]agentView, error) {
	c, err := roster()
	if err != nil {
		return nil, err
	}
	prompts, err := c.Prompts(bodies)
	if err != nil {
		return nil, err
	}
	views := make([]agentView, len(prompts))
	for i, p := range prompts {
		agent := p.Name
		if p.Kind == "routing" {
			agent = p.Routes
		}
		views[i] = agentView{AgentPrompts: p, Workflows: []string{}}
		for _, wf := range workflow.Names {
			if slices.Contains(workflow.Agents[wf], agent) {
				views[i].Workflows = append(views[i].Workflows, wf)
			}
		}
	}
	return views, nil
}

// errNoAgent is findAgent's answer for a name the roster does not have.
var errNoAgent = errors.New("no such agent")

// findAgent loads the roster and returns the named agent or routing phase,
// with bodies when bodies is set.
func findAgent(roster func() (config.Config, error), name string, bodies bool) (agentView, error) {
	agents, err := agentViews(roster, bodies)
	if err != nil {
		return agentView{}, err
	}
	i := slices.IndexFunc(agents, func(a agentView) bool { return a.Name == name })
	if i < 0 {
		return agentView{}, errNoAgent
	}
	return agents[i], nil
}

// submitRun queues a run against a recorded project. It is browser-only, as
// every write route is: see browserJSON.
func submitRun(db *trace.DB, submit Submit) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Repo     string `json:"repo"`
			Workflow string `json:"workflow"`
			Prompt   string `json:"prompt"`
			Issue    string `json:"issue"`
		}
		if !browserJSON(w, r, &body) {
			return
		}
		prompt, issue := strings.TrimSpace(body.Prompt), strings.TrimSpace(body.Issue)
		switch body.Workflow {
		case "scout", "plan", "implement", "build":
		default:
			submitError(w, http.StatusBadRequest, "unknown workflow "+strconv.Quote(body.Workflow), "")
			return
		}
		switch {
		case (prompt == "") == (issue == ""):
			submitError(w, http.StatusBadRequest, "enter a prompt or an issue reference, not both", "")
			return
		case body.Workflow == "scout" && issue != "":
			submitError(w, http.StatusBadRequest, "scout takes a prompt, not an issue", "")
			return
		}
		if _, err := db.Project(body.Repo); errors.Is(err, sql.ErrNoRows) {
			submitError(w, http.StatusBadRequest, "no recorded project at "+body.Repo, "")
			return
		} else if err != nil {
			submitError(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		id, err := submit(body.Workflow, body.Repo, prompt, issue)
		if duplicate(w, err) {
			return
		}
		// ponytail: every other failure is reported as the request's problem;
		// split out 500s if database errors turn up here in practice.
		if err != nil {
			submitError(w, http.StatusBadRequest, err.Error(), "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"run_id": id})
	}
}

// reviseRun appends a revision to a run, with the same protections as New
// run. A refusal is the run's state, not the request's shape, so it is a
// conflict; the page stays on the run and shows its queued state on success.
func reviseRun(revise func(runID, request string) (int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Request string `json:"request"`
		}
		if !browserJSON(w, r, &body) {
			return
		}
		request := strings.TrimSpace(body.Request)
		if request == "" {
			submitError(w, http.StatusBadRequest, "describe the change to make", "")
			return
		}
		id := r.PathValue("id")
		n, err := revise(id, request)
		if duplicate(w, err) {
			return
		}
		switch {
		case errors.Is(err, trace.ErrNotRevisable):
			submitError(w, http.StatusConflict, err.Error(), "")
			return
		case err != nil:
			submitError(w, http.StatusBadRequest, err.Error(), "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"run_id": id, "iteration": n})
	}
}

// cancelRun is lathe cancel for the run page: a queued iteration is cancelled
// outright, an active one stops when its worker notices.
func cancelRun(cancel func(runID string) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !browserJSON(w, r, &struct{}{}) {
			return
		}
		status, err := cancel(r.PathValue("id"))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			submitError(w, http.StatusNotFound, "no such run", "")
			return
		case errors.Is(err, trace.ErrAlreadyDone):
			submitError(w, http.StatusConflict, "already finished: "+status, "")
			return
		case err != nil:
			submitError(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	}
}

// browserJSON is every write route's gate and decoder. The Origin must be the
// page this server served, which a missing Origin is not, and the JSON content
// type keeps a cross-site form post from being a simple request. The CLI stays
// the way to script writes. It reports false once it has answered.
func browserJSON(w http.ResponseWriter, r *http.Request, body any) bool {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Header.Get("Origin") != "http://"+r.Host || mediaType != "application/json" {
		submitError(w, http.StatusForbidden, "forbidden", "")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubmitBody)).Decode(body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			submitError(w, http.StatusRequestEntityTooLarge, "request is larger than 64 KiB", "")
			return false
		}
		submitError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "")
		return false
	}
	return true
}

// duplicate answers a submission another run's checkout ownership refused,
// naming the holder, and reports whether it did.
func duplicate(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, trace.ErrDuplicateBuild) {
		return false
	}
	// Submit and Revise name the holder after the sentinel's text.
	holder, _ := strings.CutPrefix(err.Error(), trace.ErrDuplicateBuild.Error()+": ")
	submitError(w, http.StatusConflict, trace.ErrDuplicateBuild.Error(), holder)
	return true
}

func submitError(w http.ResponseWriter, code int, msg, runID string) {
	body := map[string]string{"error": msg}
	if runID != "" {
		body["run_id"] = runID
	}
	writeStatus(w, code, body)
}

// writeStatus answers code with v as JSON.
func writeStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func projectParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	values, ok := r.URL.Query()["repo"]
	if !ok || len(values) != 1 {
		http.Error(w, "repo is required", http.StatusBadRequest)
		return "", false
	}
	return values[0], true
}

func intParam(r *http.Request, name string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && n >= 0 {
		return n
	}
	return def
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
