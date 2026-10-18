package dashboard

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/tyrelh/lathe/internal/worker"
)

func TestFreshTaskRunRoute(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  int
	}{{"blocked", 201}, {"Backlog", 409}, {"In progress", 409}, {"Ready for review", 409}, {"Done", 409}, {"archived", 409}, {"missing", 404}} {
		t.Run(tc.state, func(t *testing.T) {
			state := tc.state
			var writer *trace.DB
			var taskID, old string
			ro := readOnly(t, func(db *trace.DB, root string) {
				var err error
				writer, err = trace.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { writer.Close() })
				taskID, err = db.CreateTask(trace.TaskInput{Repo: repo, Text: "original"})
				if err != nil {
					t.Fatal(err)
				}
				if state == "Backlog" {
					return
				}
				old, err = db.Submit(trace.Request{TaskID: taskID, Repo: repo, Workflow: "build", Request: "original"})
				if err != nil {
					t.Fatal(err)
				}
				if state != "In progress" {
					if _, err := db.RequestCancel(old); err != nil {
						t.Fatal(err)
					}
				}
				s, err := sql.Open("sqlite", filepath.Join(root, "lathe.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if state == "Ready for review" || state == "Done" {
					pr := "open"
					if state == "Done" {
						pr = "merged"
					}
					if _, err := s.Exec(`UPDATE runs SET status='ok', pr_state=? WHERE run_id=?`, pr, old); err != nil {
						t.Fatal(err)
					}
				}
				if state == "archived" {
					if err := db.ArchiveTask(taskID); err != nil {
						t.Fatal(err)
					}
				}
			})
			if state == "missing" {
				taskID = "missing"
			}
			action := func(id, request string) (string, error) {
				return writer.FreshTaskRun(trace.Request{TaskID: id, Workflow: "build", Repo: repo, Request: request}, func(trace.Row) bool { return true })
			}
			h := handler(ro, "test", Actions{FreshTaskRun: action}, Sources{PRURL: func(trace.Row) string { return "https://github.com/o/r/pull/1" }})
			path := "/api/tasks/" + taskID + "/runs"
			w := browserWrite(h, "POST", path, Addr, "http://"+Addr, "application/json", `{"request":"  fresh request  "}`)
			if w.Code != tc.want {
				t.Fatalf("%d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			if tc.want == 201 {
				var reply map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
					t.Fatal(err)
				}
				w = httptest.NewRecorder()
				h.ServeHTTP(w, local("GET", "/api/tasks", nil))
				var tasks []taskView
				if err := json.Unmarshal(w.Body.Bytes(), &tasks); err != nil {
					t.Fatal(err)
				}
				if len(tasks) != 1 || tasks[0].CurrentRun.ID != reply["run_id"] || !reflect.DeepEqual(tasks[0].Runs, []string{old}) {
					t.Fatalf("tasks = %+v", tasks)
				}
				row, _ := ro.Get(reply["run_id"])
				if row.Request != "fresh request" {
					t.Fatalf("request = %q", row.Request)
				}
			}
		})
	}
	ro := readOnly(t, nil)
	assertNoWriteRoute(t, handler(ro, "test", Actions{}, Sources{}), "POST", "/api/tasks/id/runs")
	h := handler(ro, "test", Actions{FreshTaskRun: func(string, string) (string, error) { return "", nil }}, Sources{})
	for _, tc := range []struct {
		origin, content, body string
		code                  int
	}{
		{"", "application/json", "{}", 403}, {"http://evil.example", "application/json", "{}", 403},
		{"http://" + Addr, "text/plain", "{}", 403}, {"http://" + Addr, "application/json", "{", 400},
	} {
		w := browserWrite(h, "POST", "/api/tasks/id/runs", Addr, tc.origin, tc.content, tc.body)
		if w.Code != tc.code {
			t.Fatalf("protection: %d", w.Code)
		}
	}
	for _, request := range []string{`{}`, `{"request":"  "}`} {
		called := false
		h := handler(ro, "test", Actions{FreshTaskRun: func(id, text string) (string, error) {
			called = true
			if text != "" {
				t.Fatalf("not trimmed: %q", text)
			}
			return "", errors.New("preparation failed")
		}}, Sources{})
		w := browserWrite(h, "POST", "/api/tasks/id/runs", Addr, "http://"+Addr, "application/json", request)
		if !called || w.Code != 400 {
			t.Fatalf("action error: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestTaskWriteRoutes(t *testing.T) {
	var writer *trace.DB
	ro := readOnly(t, func(db *trace.DB, root string) {
		if _, err := db.Submit(trace.Request{Workflow: "plan", Repo: repo}); err != nil {
			t.Fatal(err)
		}
		var err error
		writer, err = trace.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { writer.Close() })
	})
	actions := Actions{
		CreateTask: func(repo, text, issue string) (string, error) {
			return writer.CreateTask(trace.TaskInput{Repo: repo, Text: text, IssueRef: issue})
		},
		EditTask: func(id, repo, text, issue string) error {
			return writer.EditTask(id, trace.TaskInput{Repo: repo, Text: text, IssueRef: issue})
		},
		StartTask: func(id string) (string, error) {
			task, err := writer.GetTask(id)
			if err != nil {
				return "", err
			}
			return writer.StartTask(trace.Request{TaskID: id, Workflow: "build", Repo: task.Repo, Request: task.Text})
		},
	}
	h := handler(ro, "test", actions, Sources{})
	write := func(method, path, body string, want int) string {
		t.Helper()
		w := browserWrite(h, method, path, Addr, "http://"+Addr, "application/json", body)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body.String(), want)
		}
		var result map[string]string
		json.Unmarshal(w.Body.Bytes(), &result)
		if result["id"] != "" {
			return result["id"]
		}
		return result["run_id"]
	}
	payload := `{"repo":` + strconv.Quote(repo) + `,"text":"  request  "}`
	id := write("POST", "/api/tasks", payload, 201)
	task, _ := ro.GetTask(id)
	if task.Text != "request" {
		t.Fatalf("text not trimmed: %+v", task)
	}
	write("PATCH", "/api/tasks/"+id, payload, 200)
	issueID := write("POST", "/api/tasks", `{"repo":`+strconv.Quote(repo)+`,"issue":" 42 "}`, 201)
	task, _ = ro.GetTask(issueID)
	if task.IssueRef == nil || *task.IssueRef != "42" {
		t.Fatalf("issue not trimmed: %+v", task)
	}
	for _, state := range []string{"queued", "terminal", "archived"} {
		taskID := write("POST", "/api/tasks", payload, 201)
		if state == "archived" {
			if err := writer.ArchiveTask(taskID); err != nil {
				t.Fatal(err)
			}
		} else {
			runID := write("POST", "/api/tasks/"+taskID+"/start", "{}", 201)
			row, _ := ro.Get(runID)
			if row.TaskID == nil || *row.TaskID != taskID {
				t.Fatalf("wrong task: %+v", row)
			}
			if state == "terminal" {
				if _, err := writer.RequestCancel(runID); err != nil {
					t.Fatal(err)
				}
			}
		}
		write("PATCH", "/api/tasks/"+taskID, payload, 409)
		write("POST", "/api/tasks/"+taskID+"/start", "{}", 409)
	}
	write("PATCH", "/api/tasks/missing", payload, 404)
	write("POST", "/api/tasks/missing/start", "{}", 404)
	for _, route := range []struct{ method, path string }{{"POST", "/api/tasks"}, {"PATCH", "/api/tasks/" + id}, {"POST", "/api/tasks/" + id + "/start"}} {
		assertNoWriteRoute(t, handler(ro, "test", Actions{}, Sources{}), route.method, route.path)
		for _, tc := range []struct {
			host, origin, content, body string
			code                        int
		}{
			{Addr, "", "application/json", "{}", 403},
			{Addr, "http://evil.example", "application/json", "{}", 403},
			{"evil.example", "http://" + Addr, "application/json", "{}", 403},
			{Addr, "http://" + Addr, "text/plain", "{}", 403},
			{Addr, "http://" + Addr, "application/json", "{", 400},
			{Addr, "http://" + Addr, "application/json", `{"text":"` + strings.Repeat("x", maxWriteBody) + `"}`, 413},
		} {
			w := browserWrite(h, route.method, route.path, tc.host, tc.origin, tc.content, tc.body)
			if w.Code != tc.code {
				t.Fatalf("protection %s: %d %s", route.path, w.Code, w.Body.String())
			}
		}
	}
	for _, method := range []string{"POST", "PATCH"} {
		path := "/api/tasks"
		if method == "PATCH" {
			path += "/" + id
		}
		for _, body := range []string{`{}`, `{"repo":"/missing","text":"hi"}`, `{"repo":` + strconv.Quote(repo) + `,"text":" ","issue":" "}`, `{"repo":` + strconv.Quote(repo) + `,"text":"hi","issue":"42"}`} {
			write(method, path, body, 400)
		}
	}
}

func TestTaskActionsIndependent(t *testing.T) {
	ro := readOnly(t, func(db *trace.DB, _ string) {
		if _, err := db.Submit(trace.Request{Workflow: "plan", Repo: repo}); err != nil {
			t.Fatal(err)
		}
	})
	for i := range 3 {
		actions := Actions{}
		failure := errors.New("write failed")
		method, path, body := "POST", "/api/tasks", `{"repo":`+strconv.Quote(repo)+`,"text":"request"}`
		switch i {
		case 0:
			actions.CreateTask = func(string, string, string) (string, error) { return "", failure }
		case 1:
			method, path = "PATCH", "/api/tasks/id"
			actions.EditTask = func(string, string, string, string) error { return failure }
		case 2:
			path, body = "/api/tasks/id/start", "{}"
			actions.StartTask = func(string) (string, error) { return "", failure }
		}
		h := handler(ro, "test", actions, Sources{})
		w := browserWrite(h, method, path, Addr, "http://"+Addr, "application/json", body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "write failed") {
			t.Fatalf("failure: %d %s", w.Code, w.Body.String())
		}
		if i != 0 {
			assertNoWriteRoute(t, h, "POST", "/api/tasks")
		}
		if i != 1 {
			assertNoWriteRoute(t, h, "PATCH", "/api/tasks/id")
		}
		if i != 2 {
			assertNoWriteRoute(t, h, "POST", "/api/tasks/id/start")
		}
	}
}

func TestTaskArchiveRoutes(t *testing.T) {
	for _, status := range []string{"backlog", trace.StatusQueued, trace.StatusStarting, trace.StatusRunning, trace.StatusOK} {
		t.Run(status, func(t *testing.T) {
			var writer *trace.DB
			var taskID, runID string
			ro := readOnly(t, func(db *trace.DB, root string) {
				var err error
				runID, err = db.Submit(trace.Request{Workflow: "build", Repo: repo})
				if err != nil {
					t.Fatal(err)
				}
				row, _ := db.Get(runID)
				taskID = *row.TaskID
				s, err := sql.Open("sqlite", filepath.Join(root, "lathe.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if status == "backlog" {
					_, err = s.Exec(`UPDATE runs SET task_id = NULL WHERE run_id = ?`, runID)
				} else {
					for _, table := range []string{"runs", "iterations"} {
						if _, err = s.Exec(`UPDATE `+table+` SET status = ? WHERE run_id = ?`, status, runID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				writer, err = trace.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { writer.Close() })
			})
			h := handler(ro, "test", Actions{ArchiveTask: writer.ArchiveTask, UnarchiveTask: writer.UnarchiveTask}, Sources{})
			list := func(query string, count int) []taskView {
				t.Helper()
				w := httptest.NewRecorder()
				h.ServeHTTP(w, local("GET", "/api/tasks"+query, nil))
				var views []taskView
				if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil || w.Code != 200 || len(views) != count {
					t.Fatalf("list %s: %d %s", query, w.Code, w.Body.String())
				}
				return views
			}
			before := list("", 1)[0].Lane
			for _, action := range []string{"archive", "unarchive"} {
				path := "/api/tasks/" + taskID + "/" + action
				for _, tc := range []struct {
					origin, content, body string
					code                  int
				}{
					{"", "application/json", "{}", 403},
					{"http://" + Addr, "text/plain", "{}", 403},
					{"http://" + Addr, "application/json", "{", 400},
				} {
					if w := browserWrite(h, "POST", path, Addr, tc.origin, tc.content, tc.body); w.Code != tc.code {
						t.Fatalf("write protection: %d %s", w.Code, w.Body.String())
					}
				}
				w := browserWrite(h, "POST", path, Addr, "http://"+Addr, "application/json", "{}")
				if w.Code != 200 {
					t.Fatalf("%s: %d %s", action, w.Code, w.Body.String())
				}
				if action == "archive" {
					list("", 0)
					list("?archived=1", 1)
					list("?archived=1&repo="+url.QueryEscape(repo), 1)
					list("?archived=1&repo=/missing", 0)
					cancelled, err := ro.Cancelled(runID)
					live := status == trace.StatusQueued || status == trace.StatusStarting || status == trace.StatusRunning
					if err != nil || cancelled != live {
						t.Fatalf("cancelled = %v, %v", cancelled, err)
					}
				} else {
					list("?archived=1", 0)
					want := before
					if status == trace.StatusQueued {
						want = trace.LaneBlocked
					}
					if got := list("", 1)[0].Lane; got != want {
						t.Fatalf("restored lane = %s, want %s", got, want)
					}
				}
				missing := browserWrite(h, "POST", "/api/tasks/missing/"+action, Addr, "http://"+Addr, "application/json", "{}")
				if missing.Code != 404 {
					t.Fatalf("missing = %d", missing.Code)
				}
				assertNoWriteRoute(t, handler(ro, "test", Actions{}, Sources{}), "POST", path)
			}
		})
	}
}

func TestTaskArchiveActionFailures(t *testing.T) {
	ro := readOnly(t, nil)
	for _, archive := range []bool{false, true} {
		actions := Actions{}
		action := func(string) error { return errors.New("write failed") }
		path, absent := "/api/tasks/id/unarchive", "/api/tasks/id/archive"
		if archive {
			actions.ArchiveTask = action
			path, absent = absent, path
		} else {
			actions.UnarchiveTask = action
		}
		h := handler(ro, "test", actions, Sources{})
		w := browserWrite(h, "POST", path, Addr, "http://"+Addr, "application/json", "{}")
		if w.Code != 500 || !strings.Contains(w.Body.String(), "write failed") {
			t.Fatalf("failure: %d %s", w.Code, w.Body.String())
		}
		assertNoWriteRoute(t, h, "POST", absent)
	}
}

func TestTasksAPI(t *testing.T) {
	var current, earlier, taskID string
	db := readOnly(t, func(db *trace.DB, root string) {
		var err error
		earlier, err = db.Submit(trace.Request{Workflow: "build", Repo: repo, Request: "change"})
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.Get(earlier)
		if err != nil {
			t.Fatal(err)
		}
		taskID = *row.TaskID
		current, err = db.Submit(trace.Request{Workflow: "build", Repo: repo, TaskID: taskID})
		if err != nil {
			t.Fatal(err)
		}
		s, err := sql.Open("sqlite", filepath.Join(root, "lathe.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		for _, q := range []string{
			`INSERT INTO tasks (id,repo,text,created_at,updated_at) VALUES ('backlog','/other','later','2030','2030')`,
			`INSERT INTO tasks (id,repo,text,created_at,updated_at,archived_at) VALUES ('archived','/other','hidden','2040','2040','2040')`,
		} {
			if _, err := s.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Exec(`UPDATE runs SET submitted_at='2020', activity_at='2040', iteration=5 WHERE run_id=?`, earlier); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Exec(`UPDATE runs SET submitted_at='2021', activity_at='2029', iteration=2, status='ok', pr_state='open' WHERE run_id=?`, current); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Exec(`UPDATE tasks SET tracker='github', issue_ref='42', issue_url='https://github.com/o/r/issues/42', issue_title='Issue', issue_body='Body' WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
	})
	h := handler(db, "test", Actions{}, Sources{PRURL: func(r trace.Row) string {
		if r.ID != current {
			return ""
		}
		return "https://github.com/o/r/pull/1"
	}})
	for _, tc := range []struct {
		query string
		count int
	}{{"", 2}, {"?repo=" + url.QueryEscape(repo), 1}, {"?repo=/rep", 0}, {"?repo=/unknown", 0}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", "/api/tasks"+tc.query, nil))
		var got []taskView
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || got == nil || len(got) != tc.count {
			t.Fatalf("tasks %s: %d %s", tc.query, w.Code, w.Body.String())
		}
		for _, v := range got {
			if v.ID == "backlog" {
				if v.Lane != trace.LaneBacklog || v.Activity != "2030" || v.CurrentRun != nil || v.Issue != nil || v.Runs == nil {
					t.Fatalf("backlog: %+v", v)
				}
			} else if v.ID != taskID || v.Lane != trace.LaneReadyForReview || v.Activity != "2029" || v.CurrentRun.ID != current || v.CurrentRun.Iteration != 2 || v.CurrentRun.Status != "ok" || *v.CurrentRun.PRState != "open" || v.CurrentRun.PRURL == "" || !reflect.DeepEqual(v.Runs, []string{earlier}) || *v.Issue.Title != "Issue" || *v.Issue.Body != "Body" {
				t.Fatalf("task: %+v", v)
			}
		}
		if tc.query == "" && got[0].ID != "backlog" {
			t.Fatal("not activity ordered")
		}
	}
	for _, path := range []string{"/api/tasks", "/api/tasks/" + taskID, "/api/tasks/" + taskID + "/start"} {
		assertNoWriteRoute(t, h, "POST", path)
	}
	w := httptest.NewRecorder()
	r := local("GET", "/api/tasks", nil)
	r.Host = "evil.example"
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("unprotected tasks")
	}
	for _, id := range []string{current, earlier} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", "/api/runs/"+id, nil))
		var detail struct {
			Run trace.Row `json:"run"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Run.TaskID == nil || *detail.Run.TaskID != taskID {
			t.Fatal("run task link missing")
		}
	}
}

func TestTasksEmpty(t *testing.T) {
	h := handler(readOnly(t, nil), "test", Actions{}, Sources{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, local("GET", "/api/tasks", nil))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty tasks: %d %s", w.Code, w.Body.String())
	}
}

func TestRunPRObservation(t *testing.T) {
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.Submit(trace.Request{Workflow: "build", Repo: repo, Request: "x"})
	if err != nil {
		t.Fatal(err)
	}
	dir := worker.ReportDir(root, id, 0)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr.json"), []byte(`{"url":"https://github.com/o/r/pull/1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := handler(db, "test", Actions{}, Sources{PRURL: func(r trace.Row) string { return worker.RecordedPRURL(root, r) }})
	for _, state := range []string{"", "open", "merged"} {
		if state != "" {
			if err := db.ObservePR(id, state); err != nil {
				t.Fatal(err)
			}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", "/api/runs/"+id, nil))
		var got struct {
			Run     trace.Row `json:"run"`
			URL     string    `json:"pr_url"`
			Pending bool      `json:"pr_pending"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.URL != "https://github.com/o/r/pull/1" {
			t.Fatalf("PR URL = %q", got.URL)
		}
		if got.Pending != (state != "merged") {
			t.Fatalf("%q pending = %v", state, got.Pending)
		}
		if state == "" {
			if got.Run.PRState != nil {
				t.Fatal("unchecked state is not null")
			}
		} else if got.Run.PRState == nil || *got.Run.PRState != state {
			t.Fatalf("state = %+v", got.Run.PRState)
		}
	}
}

// repo is the checkout path the seeded runs record.
const repo = "/repos/alpha"

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

// browserWrite serves a method request to path with the given Host, Origin,
// Content-Type and body; an empty origin sends no Origin header.
func browserWrite(h http.Handler, method, path, host, origin, contentType, body string) *httptest.ResponseRecorder {
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

// assertNoWriteRoute fails unless h answers a valid browser write of method to
// path with 404 or 405.
func assertNoWriteRoute(t *testing.T, h http.Handler, method, path string) {
	t.Helper()
	w := browserWrite(h, method, path, Addr, "http://"+Addr, "application/json", `{}`)
	if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
		t.Errorf("%s %s without its action: %d", method, path, w.Code)
	}
}

// readOnly returns a read-only handle, closed at cleanup, on a fresh trace
// database at root after seed, when non-nil, has written to it.
func readOnly(t *testing.T, seed func(db *trace.DB, root string)) *trace.DB {
	t.Helper()
	root := t.TempDir()
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if seed != nil {
		seed(db, root)
	}
	db.Close()
	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	return ro
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
	var runID string
	ro := readOnly(t, func(db *trace.DB, _ string) {
		runID, _, _ = claimed(t, db, "scout", repo, "where is the parser")
		completed := trace.NewPhase(runID, 0, "review", "plan-reviewer")
		completed.Finish(trace.PhaseRequestedChanges, "")
		if err := db.PhaseUpsert(completed); err != nil {
			t.Fatal(err)
		}
		p := trace.NewPhase(runID, 1, "scout", "scout")
		if err := db.PhaseUpsert(p); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"read", "grep"} {
			if err := db.Event(runID, p.ID, "tool_call", name, map[string]string{"path": "main.go"}); err != nil {
				t.Fatal(err)
			}
		}
	})
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
	if d.Run.Status != "running" || len(d.Phases) != 2 || len(d.Events) != 2 {
		t.Fatalf("run %+v, %d phases, %d events", d.Run, len(d.Phases), len(d.Events))
	}
	if d.Phases[0].Status != trace.PhaseRequestedChanges || d.Phases[0].Error != "" || d.Phases[0].End == "" {
		t.Errorf("requested changes phase = %+v", d.Phases[0])
	}
	if d.Phases[1].Status != "fail" {
		t.Errorf("an unfinished phase should read 'fail', got %q", d.Phases[1].Status)
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
	ro := readOnly(t, func(db *trace.DB, _ string) {
		for i := 0; i < 3; i++ {
			// Finished each time round: a second outstanding build for one
			// checkout is rejected at submission, which is the point of the rule.
			id, attempt, token := claimed(t, db, "build", repo, "add retries")
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
	})
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
	if o.TopProjects[0].Repo != repo || o.TopProjects[0].Runs != 3 {
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
	if err := json.Unmarshal(do("/api/projects/detail?repo="+url.QueryEscape(repo)).Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	if project.Runs != 3 || project.Cost != o.Cost || project.Tokens != o.Tokens {
		t.Fatalf("project totals = %+v, overview = %+v", project, o)
	}
	var page struct {
		Runs []trace.Row `json:"runs"`
		More bool        `json:"more"`
	}
	if err := json.Unmarshal(do("/api/projects/runs?repo="+url.QueryEscape(repo)+"&n=2").Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 2 || !page.More {
		t.Fatalf("first project runs page = %+v", page)
	}
	next := "/api/projects/runs?repo=" + url.QueryEscape(repo) + "&n=2&before_at=" +
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
	w := httptest.NewRecorder()
	handler(readOnly(t, nil), "dev", Actions{}, Sources{}).ServeHTTP(w, local("GET", "/api/overview", nil))
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
	ro := readOnly(t, func(db *trace.DB, _ string) {
		if _, err := db.Submit(trace.Request{Workflow: "scout", Repo: "/repos/alpha", Request: "look", Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	})

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
		w := browserWrite(h, "POST", "/api/projects/runs", host, origin, contentType, body)
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

// The recovery route keeps the browser protections, requires guidance,
// passes it to the shared submission and maps refusals to a conflict.
func TestResumeRoute(t *testing.T) {
	var id string
	ro := readOnly(t, func(db *trace.DB, _ string) {
		var attempt, token string
		id, attempt, token = claimed(t, db, "build", "/repos/alpha", "greet")
		if err := db.Complete(id, attempt, token, trace.StatusFail, "push failed"); err != nil {
			t.Fatal(err)
		}
	})
	var resumed []string
	var resumeErr error
	h := handler(ro, "dev", Actions{Resume: func(runID, guidance string) (int, error) {
		resumed = append(resumed, runID+": "+guidance)
		return 1, resumeErr
	}}, Sources{})
	post := func(origin, body string) (int, string) {
		w := browserWrite(h, "POST", "/api/runs/"+id+"/recoveries", Addr, origin, "application/json", body)
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
	assertNoWriteRoute(t, handler(ro, "dev", Actions{}, Sources{}), "POST", "/api/runs/"+id+"/recoveries")
}

// The run page's two writes: revise and cancel. They carry New run's
// protections, go through the actions handed in, and map refusals to
// statuses the page can explain. The run payload carries the iterations and
// the models each ran with.
func TestReviseAndCancelRoutes(t *testing.T) {
	var id string
	ro := readOnly(t, func(db *trace.DB, _ string) {
		var attempt, token string
		id, attempt, token = claimed(t, db, "build", "/repos/alpha", "greet")
		if err := db.Complete(id, attempt, token, trace.StatusOK, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Revise(id, 0, "greet louder",
			[]byte(`{"roster":{"agents":{"planner":{"provider":"moonshotai","model":"kimi-k3"}}}}`)); err != nil {
			t.Fatal(err)
		}
	})

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
		w := browserWrite(h, "POST", path, Addr, origin, contentType, body)
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
		assertNoWriteRoute(t, bare, "POST", path)
	}
}

// The run payload says whether the run page may offer revise and resume, by
// workflow and latest status.
func TestRunEligibility(t *testing.T) {
	cases := []struct {
		workflow, status       string
		revisable, recoverable bool
	}{
		{"build", trace.StatusOK, true, false},
		{"scout", trace.StatusOK, false, false},
		{"build", trace.StatusRunning, false, false},
		{"build", trace.StatusFail, false, true},
		{"build", trace.StatusCancelled, false, true},
		{"plan", trace.StatusLost, false, true},
	}
	ids := make([]string, len(cases))
	ro := readOnly(t, func(db *trace.DB, _ string) {
		for i, c := range cases {
			id, attempt, token := claimed(t, db, c.workflow, fmt.Sprintf("/repos/%d", i), "greet")
			if c.status != trace.StatusRunning {
				if err := db.Complete(id, attempt, token, c.status, ""); err != nil {
					t.Fatal(err)
				}
			}
			ids[i] = id
		}
	})
	h := handler(ro, "dev", Actions{}, Sources{})
	for i, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", "/api/runs/"+ids[i], nil))
		var d struct{ Revisable, Recoverable bool }
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		if d.Revisable != c.revisable || d.Recoverable != c.recoverable {
			t.Errorf("%s %s: revisable %v, recoverable %v", c.workflow, c.status, d.Revisable, d.Recoverable)
		}
	}
}

// The run payload says where a writing iteration ran and where its result is
// kept, and the run still counts toward its original repository rather than
// the worktree it ran in.
func TestWorktreeReporting(t *testing.T) {
	var id, tree, patch, ref string
	ro := readOnly(t, func(db *trace.DB, root string) {
		var attempt, token string
		id, attempt, token = claimed(t, db, "build", "/repos/alpha", "greet")
		tree = filepath.Join(root, "worktrees", id, "iteration-0")
		if err := db.SetWorktree(id, attempt, tree, "0123456789abcdef", "active"); err != nil {
			t.Fatal(err)
		}
		if err := db.Complete(id, attempt, token, trace.StatusFail, "validation unresolved"); err != nil {
			t.Fatal(err)
		}
		patch = filepath.Join(root, "runs", id, "result.patch")
		ref = "refs/lathe/runs/" + id + "/iteration-0"
		if err := db.SetPatch(id, 0, patch); err != nil {
			t.Fatal(err)
		}
		if err := db.SetRecoveryRef(id, 0, ref); err != nil {
			t.Fatal(err)
		}
		if err := db.SetWorktreeState(id, 0, "pending", "removing the worktree: busy"); err != nil {
			t.Fatal(err)
		}
	})
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
	ro := readOnly(t, nil)
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
	if !reflect.DeepEqual(got, names) {
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
