// Package dashboard serves a read-only view of the trace database: one page
// from the binary, two JSON routes, and no router — three routes do not earn a
// dependency.
package dashboard

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/tyrelh/lathe/internal/trace"
)

//go:embed index.html
var indexHTML []byte

// Addr is a literal rather than a setting: the server has no authentication
// and serves prompts and source. Remote viewing is an SSH tunnel.
const Addr = "127.0.0.1:4700"

// Serve blocks until the process is interrupted. The database is opened
// read-only once and shared: SQLite in WAL mode lets this reader in while a
// run in another terminal is writing.
func Serve(dataRoot, version string, out io.Writer) error {
	// Migrations need a writer, and the serving connection is not one. Doing it
	// here in its own step means a database written by an older lathe — or no
	// database at all — opens as a dashboard rather than as an error telling
	// you to go start an agent run first.
	if err := trace.Init(dataRoot); err != nil {
		return err
	}
	db, err := trace.OpenRO(dataRoot)
	if err != nil {
		return err
	}
	defer db.Close()

	fmt.Fprintf(out, "lathe dash: http://%s  (Ctrl-C to stop)\n", Addr)
	return http.ListenAndServe(Addr, handler(db, version))
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
