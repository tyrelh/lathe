// Package dashboard serves a read-only view of the trace database through an
// embedded page and JSON endpoints.
package dashboard

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/tyrelh/lathe/internal/trace"
)

//go:embed index.html
var indexHTML []byte

// Addr is a literal rather than a setting: the server has no authentication
// and serves prompts and source. Remote viewing is an SSH tunnel.
const Addr = "127.0.0.1:4700"

// Handler is the dashboard as something to mount: the manager serves it
// alongside the scheduler, so there is no second process and no second
// listener. The database is opened read-only — a UI bug cannot write — and
// created first if it is not there yet, so a fresh install serves an empty
// dashboard rather than an error telling you to go start a run.
func Handler(dataRoot, version string) http.Handler {
	if err := trace.Init(dataRoot); err != nil {
		return failed(err)
	}
	db, err := trace.OpenRO(dataRoot)
	if err != nil {
		return failed(err)
	}
	return handler(db, version)
}

func failed(err error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	})
}

// handler is the whole server minus the listener, which is what lets a test
// exercise the routes without binding a port.
func handler(db *trace.DB, version string) http.Handler {
	mux := http.NewServeMux()
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
	return mux
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
