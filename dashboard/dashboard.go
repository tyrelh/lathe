// Package dashboard serves a view of the trace database through an embedded
// page and JSON endpoints, plus one route that queues a run for a project.
package dashboard

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/tyrelh/lathe/internal/trace"
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

// maxSubmitBody bounds a POST body; a prompt is text someone typed.
const maxSubmitBody = 64 << 10

// Handler is the dashboard as something to mount: the manager serves it
// alongside the scheduler, so there is no second process and no second
// listener. The database is opened read-only — a UI bug cannot write — and
// created first if it is not there yet, so a fresh install serves an empty
// dashboard rather than an error telling you to go start a run. Submissions go
// through submit, never through this handle; a nil submit registers no POST
// route at all.
func Handler(dataRoot, version string, submit Submit) http.Handler {
	if err := trace.Init(dataRoot); err != nil {
		return failed(err)
	}
	db, err := trace.OpenRO(dataRoot)
	if err != nil {
		return failed(err)
	}
	return handler(db, version, submit)
}

func failed(err error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	})
}

// handler is the whole server minus the listener, which is what lets a test
// exercise the routes without binding a port.
func handler(db *trace.DB, version string, submit Submit) http.Handler {
	mux := http.NewServeMux()
	if submit != nil {
		mux.HandleFunc("POST /api/projects/runs", submitRun(db, submit))
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
		// The cursor bounds one poll; whatever is left arrives on the next one.
		// index.html knows this number: a full page is how it tells that a
		// settled run still has events to fetch before it stops polling.
		events, err := db.Events(id, int64(intParam(r, "after", 0)), 500)
		writeJSON(w, map[string]any{"run": run, "phases": phases, "events": events}, err)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// submitRun queues a run against a recorded project. It is browser-only: the
// Origin must be the page this server served, which a missing Origin is not,
// and the JSON content type keeps a cross-site form post from being a simple
// request. The CLI stays the way to script submissions.
func submitRun(db *trace.DB, submit Submit) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if r.Header.Get("Origin") != "http://"+r.Host || mediaType != "application/json" {
			submitError(w, http.StatusForbidden, "forbidden", "")
			return
		}
		var body struct {
			Repo     string `json:"repo"`
			Workflow string `json:"workflow"`
			Prompt   string `json:"prompt"`
			Issue    string `json:"issue"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubmitBody)).Decode(&body); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				submitError(w, http.StatusRequestEntityTooLarge, "request is larger than 64 KiB", "")
				return
			}
			submitError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "")
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
		if errors.Is(err, trace.ErrDuplicateBuild) {
			// Submit names the holder after the sentinel's text.
			holder, _ := strings.CutPrefix(err.Error(), trace.ErrDuplicateBuild.Error()+": ")
			submitError(w, http.StatusConflict, trace.ErrDuplicateBuild.Error(), holder)
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

func submitError(w http.ResponseWriter, code int, msg, runID string) {
	body := map[string]string{"error": msg}
	if runID != "" {
		body["run_id"] = runID
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
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
