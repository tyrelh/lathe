package main

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/gittest"
	"github.com/tyrelh/lathe/internal/pitest"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
)

func TestDashboardTaskWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	repo, err := filepath.EvalSymlinks(gitRepo(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	other, err := filepath.EvalSymlinks(gitRepo(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	_, db, err := openWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, project := range []string{repo, other} {
		if _, err := db.Submit(trace.Request{Workflow: "plan", Repo: project}); err != nil {
			t.Fatal(err)
		}
	}
	stub := t.TempDir()
	log := filepath.Join(stub, "fetches")
	gh := filepath.Join(stub, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\necho fetch >> '"+log+"'\nprintf '%s' '{\"number\":42,\"title\":\"title\",\"body\":\"body\",\"url\":\"https://github.com/o/r/issues/42\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	fetches := func() int {
		t.Helper()
		data, _ := os.ReadFile(log)
		return strings.Count(string(data), "fetch\n")
	}
	id, err := dashboardCreateTask(repo, "", "o/r#42")
	if err != nil {
		t.Fatal(err)
	}
	task, _ := db.GetTask(id)
	if task.Text != "GitHub issue https://github.com/o/r/issues/42" || task.IssueRef == nil || *task.IssueRef != "https://github.com/o/r/issues/42" || task.IssueTitle == nil || *task.IssueTitle != "title" || task.IssueBody == nil || *task.IssueBody != "body" || fetches() != 1 {
		t.Fatalf("issue task = %+v, fetches = %d", task, fetches())
	}
	if err := dashboardEditTask(id, repo, "", "https://github.com/o/r/issues/42"); err != nil {
		t.Fatal(err)
	}
	if fetches() != 1 {
		t.Fatal("unchanged reference fetched again")
	}
	if err := dashboardEditTask(id, repo, "", "42"); err != nil {
		t.Fatal(err)
	}
	if fetches() != 2 {
		t.Fatal("changed reference did not fetch")
	}
	if err := dashboardEditTask(id, other, "", "42"); err != nil {
		t.Fatal(err)
	}
	if fetches() != 3 {
		t.Fatal("repository change did not fetch numeric reference")
	}
	if err := dashboardEditTask(id, other, "plain text", ""); err != nil {
		t.Fatal(err)
	}
	task, _ = db.GetTask(id)
	if task.Text != "plain text" || task.IssueRef != nil || task.IssueURL != nil || task.IssueTitle != nil || task.IssueBody != nil || task.Tracker != nil {
		t.Fatalf("plain task = %+v", task)
	}
	before := task
	if err := os.WriteFile(gh, []byte("#!/bin/sh\necho failed >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dashboardEditTask(id, other, "", "43"); err == nil {
		t.Fatal("fetch failure accepted")
	}
	after, _ := db.GetTask(id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed fetch changed task")
	}
	tasks, _ := db.Tasks("")
	if _, err := dashboardCreateTask(repo, "", "43"); err == nil {
		t.Fatal("failed create accepted")
	}
	remaining, _ := db.Tasks("")
	if len(remaining) != len(tasks) {
		t.Fatal("failed create left task")
	}
	// Starting uses the stored reference, without fetching through gh here.
	issueID, err := db.CreateTask(trace.TaskInput{Repo: repo, Text: "GitHub issue 42", IssueRef: "42", IssueURL: "https://github.com/o/r/issues/42", IssueTitle: "stored", IssueBody: "stored body"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := dashboardStartTask(issueID)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := db.Get(runID)
	if row.TaskID == nil || *row.TaskID != issueID || row.Workflow != "build" || row.Request != "GitHub issue 42" {
		t.Fatalf("started run = %+v", row)
	}
	iterations, err := db.Iterations(runID)
	if err != nil || len(iterations) != 1 {
		t.Fatalf("iterations = %+v, %v", iterations, err)
	}
	var spec worker.Spec
	if err := json.Unmarshal(iterations[0].Spec, &spec); err != nil || spec.Issue != "42" {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	if _, err := dashboardStartTask(issueID); !errors.Is(err, trace.ErrTaskConflict) {
		t.Fatalf("duplicate start: %v", err)
	}
	if err := dashboardEditTask(issueID, repo, "no", ""); !errors.Is(err, trace.ErrTaskConflict) {
		t.Fatalf("edit started: %v", err)
	}
	plainRun, err := dashboardStartTask(id)
	if err != nil {
		t.Fatal(err)
	}
	row, _ = db.Get(plainRun)
	if row.TaskID == nil || *row.TaskID != id || row.Request != "plain text" || row.Workflow != "build" {
		t.Fatalf("plain run = %+v", row)
	}
}

func TestPrepareTaskAndSubmitUsesWriter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo, err := filepath.EvalSymlinks(gitRepo(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	db, err := trace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", blocked)
	id, err := db.CreateTask(trace.TaskInput{Repo: repo, Text: "GitHub issue 42", IssueRef: "42"})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := prepareTaskAndSubmit(db, "build", repo, "", "42", id)
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.Get(runID)
	if err != nil || row.TaskID == nil || *row.TaskID != id || row.Workflow != "build" {
		t.Fatalf("started run = %+v, %v", row, err)
	}
	var spec worker.Spec
	if err := json.Unmarshal(row.Spec, &spec); err != nil || spec.Issue != "42" {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	if _, err := prepareTaskAndSubmit(db, "build", repo, "", "42", id); !errors.Is(err, trace.ErrTaskConflict) {
		t.Fatalf("duplicate start: %v", err)
	}
	subdir := filepath.Join(repo, "subdir")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	moved, err := db.CreateTask(trace.TaskInput{Repo: subdir, Text: "change"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareTaskAndSubmit(db, "build", subdir, "change", "", moved); err == nil || !strings.Contains(err.Error(), "now resolves to") {
		t.Fatalf("checkout identity: %v", err)
	}
	runs, err := db.TaskRuns(moved)
	if err != nil || len(runs) != 0 {
		t.Fatalf("failed preparation left runs = %+v, %v", runs, err)
	}
}

func TestDashboardArchiveTask(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	_, db, err := openWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.Submit(trace.Request{Workflow: "build", Repo: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := db.Get(id)
	if err := dashboardArchiveTask(*row.TaskID); err != nil {
		t.Fatal(err)
	}
	task, _ := db.GetTask(*row.TaskID)
	cancelled, err := db.Cancelled(id)
	if task.ArchivedAt == nil || !cancelled || err != nil {
		t.Fatalf("archive = %+v, cancelled = %v, %v", task, cancelled, err)
	}
	if err := dashboardUnarchiveTask(task.ID); err != nil {
		t.Fatal(err)
	}
	task, _ = db.GetTask(task.ID)
	if task.ArchivedAt != nil {
		t.Fatal("still archived")
	}
}

func TestPreparePreservesTaskIssue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	repo := gitRepo(t, "")
	ref := "https://github.com/o/r/issues/42"
	req, err := prepare("build", repo, "issue request", ref, config.Overrides{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var spec worker.Spec
	if err := json.Unmarshal(req.Spec, &spec); err != nil || spec.Issue != ref {
		t.Fatalf("issue = %q, %v", spec.Issue, err)
	}
	root, err := trace.DataRoot()
	if err != nil {
		t.Fatal(err)
	}
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.Submit(req.Request)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := db.Get(id)
	if row.TaskID == nil {
		t.Fatal("prepared build has no task")
	}
	task, _ := db.GetTask(*row.TaskID)
	if task.IssueRef == nil || *task.IssueRef != ref {
		t.Fatalf("task = %+v", task)
	}
	id, err = prepareAndSubmit("build", req.Repo, "", "o/r#42")
	if err != nil {
		t.Fatal(err)
	}
	row, _ = db.Get(id)
	if row.TaskID == nil {
		t.Fatal("New Run build has no task")
	}
	task, _ = db.GetTask(*row.TaskID)
	if task.IssueRef == nil || *task.IssueRef != ref {
		t.Fatalf("New Run task = %+v", task)
	}
}

// init is gone with lathe.toml.
func TestInitIsRemoved(t *testing.T) {
	t.Chdir(t.TempDir())
	if code := dispatch([]string{"init"}); code != 2 {
		t.Fatalf("init exited %d", code)
	}
	if _, err := os.Stat("lathe.toml"); !os.IsNotExist(err) {
		t.Fatalf("lathe.toml was created: %v", err)
	}
}

// Capture seeds ~/.lathe and freezes what is there now. An edit there reaches
// the next capture and not an earlier one; a repository's lathe.toml and a
// data-root prompt override reach neither; a broken file refuses capture with
// the file named.
func TestCaptureReadsDotLathe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Chdir(gitRepo(t, "provider = \"anthropic\"\nmodel = \"from-lathe-toml\"\n[agents.builder]\nmodel = \"also-ignored\"\n"))
	override := filepath.Join(home, "data", "lathe", "prompts", "builder", "system.md")
	os.MkdirAll(filepath.Dir(override), 0o755)
	os.WriteFile(override, []byte("You build by override.\n"), 0o644)

	seeded, err := fs.ReadFile(Seed, "agents/builder/system.md")
	if err != nil {
		t.Fatal(err)
	}
	first, err := capture("build", config.Overrides{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b := first.Agents["builder"]
	if b.SystemPrompt != string(seeded) || b.Provider != "anthropic" || b.Model != "claude-opus-5-5" {
		t.Fatalf("builder = %s/%s %q", b.Provider, b.Model, b.SystemPrompt)
	}
	if len(first.Routing["builder"].Tiers) != 3 || len(first.Routing["planner"].Tiers) != 3 {
		t.Fatalf("routing = %+v", first.Routing)
	}

	system := filepath.Join(home, ".lathe", "agents", "builder", "system.md")
	if err := os.WriteFile(system, []byte("You build by edit.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := capture("build", config.Overrides{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Agents["builder"].SystemPrompt; got != "You build by edit.\n" {
		t.Fatalf("captured after the edit: %q", got)
	}
	if got := first.Agents["builder"].SystemPrompt; got != string(seeded) {
		t.Fatalf("an earlier capture changed: %q", got)
	}

	flagged, err := capture("build", config.Overrides{Model: "m"}, io.Discard)
	if err != nil || flagged.Agents["builder"].Model != "m" || !flagged.RoutingOff || len(flagged.Routing) != 0 {
		t.Fatalf("flagged capture: %+v %v", flagged.Agents["builder"], err)
	}

	for _, tc := range []struct{ file, body string }{
		{"system.md", " \n"},
		{"config.yaml", "provider: ["},
		{"config.yaml", "provider: moonshotai\nmodel: kimi-k2.7-code\nthinking: medium\ntimeout: 10m\n---\nmodel: hidden\n"},
	} {
		file, body := tc.file, tc.body
		path := filepath.Join(home, ".lathe", "agents", "builder", file)
		good, _ := os.ReadFile(path)
		os.WriteFile(path, []byte(body), 0o644)
		if _, err := capture("build", config.Overrides{}, io.Discard); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("broken %s: %v", file, err)
		}
		os.WriteFile(path, good, 0o644)
	}
}

// TestEndToEnd is the one test that uses real processes: a submitter that
// starts a manager, a manager that launches a worker, and a worker that runs
// a workflow against a real checkout. Everything else about dispatch is a
// database property and is tested as one — this is here to prove the launcher
// is not lying about any of it.
func TestEndToEnd(t *testing.T) {
	home := t.TempDir()
	bin := buildLathe(t)

	repo := gitRepo(t, "")
	// Submissions run from a repository with a lathe.toml of its own, which
	// lathe no longer reads.
	invokedFrom := gitRepo(t, `model = "from-the-invocation-directory"`)

	// A fake agent on PATH: the worker resolves pi the way production does,
	// and the manager passes PATH through to it.
	stub := t.TempDir()
	reply := pitest.Message(t, "Looked.\n\n```json\n{\"summary\":\"it dispatches\",\"findings\":[],\"artifacts\":[]}\n```", 10)
	os.WriteFile(filepath.Join(stub, "reply.jsonl"), []byte(reply), 0o644)
	os.WriteFile(filepath.Join(stub, "pi"), []byte("#!/bin/sh\ncat "+filepath.Join(stub, "reply.jsonl")+"\n"), 0o755)

	env := []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
	}

	// The submitter blocks: no manager is running, so it starts one, and the
	// run comes back finished.
	out, code := latheIn(t, bin, invokedFrom, env, 90*time.Second, "scout", "--repo", repo, "what is here")
	if code != 0 {
		t.Fatalf("lathe scout exited %d:\n%s\n%s", code, out, managerLog(home))
	}
	if !strings.Contains(out, "queued") || !strings.Contains(out, ": ok") {
		t.Fatalf("scout output:\n%s\n%s", out, managerLog(home))
	}
	if strings.Contains(out, "lathe.toml") {
		t.Fatalf("a repository with no lathe.toml was not silent about it:\n%s", out)
	}

	if list, _ := latheIn(t, bin, invokedFrom, env, 90*time.Second, "runs"); !strings.Contains(list, "scout") || !strings.Contains(list, "ok") {
		t.Fatalf("lathe runs:\n%s", list)
	}

	// --detach returns immediately with the ID, and wait picks the same run up
	// and reports its outcome.
	out, code = latheIn(t, bin, invokedFrom, env, 90*time.Second, "scout", "--detach", "--repo", repo, "what else is here")
	if code != 0 {
		t.Fatalf("detached submission exited %d:\n%s", code, out)
	}
	id := queuedID(out)
	if id == "" {
		t.Fatalf("detached output: %q", out)
	}
	if waited, code := latheIn(t, bin, invokedFrom, env, 90*time.Second, "wait", id); code != 0 || !strings.Contains(waited, ": ok") {
		t.Fatalf("lathe wait %s exited %d:\n%s\n%s", id, code, waited, managerLog(home))
	}
	if shown, code := latheIn(t, bin, invokedFrom, env, 90*time.Second, "show", "--json", id); code != 0 || !strings.Contains(shown, `"status": "ok"`) {
		t.Fatalf("lathe show exited %d:\n%s", code, shown)
	}

	// Issue input survives submission, the manager and worker, then reaches
	// both planning agents. A lookup failure must never invoke an agent.
	for _, args := range [][]string{
		{"plan", "--issue", ""}, {"plan", "--issue", "--web"},
		{"plan", "--issue", "41", "also a prompt"}, {"scout", "--issue", "41"},
	} {
		if out, code := latheIn(t, bin, invokedFrom, env, 90*time.Second, args...); code != 2 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
	}
	issueJSON := `{"number":41,"title":"Issue task title","body":"Issue task body","url":"https://github.com/tyrelh/lathe/issues/41"}`
	ghScript := "#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$@\" > " + filepath.Join(stub, "gh-args") + "\necho '" + issueJSON + "'\n"
	if err := os.WriteFile(filepath.Join(stub, "gh"), []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	planJSON := "```json\n" + `{"summary":"planned","steps":["edit hello.txt"],"files":["hello.txt"],"risks":[],"feedback":[],"blocking":[],"artifacts":[]}` + "\n```"
	stream, _ := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []map[string]string{{"type": "text", "text": planJSON}}}})
	if err := os.WriteFile(filepath.Join(stub, "reply.jsonl"), append(stream, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	piScript := "#!/bin/sh\nif [ \"$1 $2\" = 'auth check' ]; then printf '{\"provider\":\"%s\",\"status\":\"ready\",\"authType\":\"api_key\"}\\n' \"$4\"; exit 0; fi\nprintf '%s\\n' \"$@\" >> " + filepath.Join(stub, "pi-args") + "\ncat " + filepath.Join(stub, "reply.jsonl") + "\n"
	if err := os.WriteFile(filepath.Join(stub, "pi"), []byte(piScript), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code = latheIn(t, bin, invokedFrom, env, 90*time.Second, "plan", "--repo", repo, "--issue", "41")
	if code != 0 {
		t.Fatalf("issue plan: %d %s", code, out)
	}
	ghArgs, _ := os.ReadFile(filepath.Join(stub, "gh-args"))
	realRepo, _ := filepath.EvalSymlinks(repo)
	if string(ghArgs) != realRepo+"\nissue\nview\n41\n--json\nnumber,title,body,url\n" {
		t.Fatalf("gh invocation: %s", ghArgs)
	}
	piArgs, _ := os.ReadFile(filepath.Join(stub, "pi-args"))
	if strings.Count(string(piArgs), "Issue task body") != 2 || !strings.Contains(string(piArgs), "Issue task title") {
		t.Fatalf("issue handoff: %s", piArgs)
	}
	db, err := trace.Open(filepath.Join(home, "data", "lathe"))
	if err != nil {
		t.Fatal(err)
	}
	issueRunID := queuedID(out)
	phases, err := db.Phases(issueRunID)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	// The seeded planner has tiers, so the plan is routed: without a Typesafe
	// key it lands on the default tier, and the routing phase is recorded.
	if len(phases) != 6 || phases[1].Name != "auth" || phases[2].Name != "issue" || phases[2].Status != "success" {
		t.Fatalf("phases: %+v", phases)
	}
	if _, err := os.Stat(filepath.Join(home, "data", "lathe", "runs", issueRunID, "issue.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stub, "gh"), []byte("#!/bin/sh\necho 'issue not found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, workflow := range []string{"plan", "implement", "build"} {
		out, code = latheIn(t, bin, invokedFrom, env, 90*time.Second, workflow, "--repo", repo, "--issue", "41")
		if code != 1 || !strings.Contains(out, "issue not found") {
			t.Fatalf("%s lookup failure: %d %s", workflow, code, out)
		}
	}
	after, _ := os.ReadFile(filepath.Join(stub, "pi-args"))
	if string(after) != string(piArgs) {
		t.Fatal("planner ran after failed lookup")
	}

	// A repository's lathe.toml has no effect; a flag still applies.
	configured := gitRepo(t, "provider = \"anthropic\"\n[agents.scout]\nmodel = \"claude-sonnet-5\"\n")
	out, code = latheIn(t, bin, invokedFrom, env, 90*time.Second, "scout", "--detach", "--repo", configured, "--thinking", "high", "what is configured")
	if code != 0 || strings.Contains(out, "lathe.toml") {
		t.Fatalf("configured submission exited %d:\n%s", code, out)
	}
	if a := capturedScout(t, home, out); a.Provider != "openai-codex" || a.Model != "gpt-6-sol" || a.Thinking != "high" {
		t.Fatalf("captured scout: %+v", a)
	}

	// An edit in ~/.lathe is frozen into runs submitted after it is written,
	// and a run captured before keeps the seed.
	seeded, err := fs.ReadFile(Seed, "agents/scout/system.md")
	if err != nil {
		t.Fatal(err)
	}
	before := out
	edited := filepath.Join(home, ".lathe", "agents", "scout", "system.md")
	if err := os.WriteFile(edited, []byte("You scout by edit.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = latheIn(t, bin, invokedFrom, env, 90*time.Second, "scout", "--detach", "--repo", repo, "what is edited")
	if code != 0 {
		t.Fatalf("edited submission exited %d:\n%s", code, out)
	}
	if a := capturedScout(t, home, out); a.SystemPrompt != "You scout by edit.\n" {
		t.Fatalf("captured scout system prompt: %q", a.SystemPrompt)
	}
	if a := capturedScout(t, home, before); a.SystemPrompt != string(seeded) {
		t.Fatalf("a run captured before the edit has %q", a.SystemPrompt)
	}
}

// gitRepo makes a committed repository, with project as its lathe.toml unless
// that is empty.
func gitRepo(t *testing.T, project string) string {
	t.Helper()
	files := map[string]string{"hello.txt": "hello\n"}
	if project != "" {
		files["lathe.toml"] = project
	}
	repo, _ := gittest.Repo(t, files)
	return repo
}

// capturedScout reads back the scout a detached submission froze into its
// specification, from the run ID in the submission's output.
func capturedScout(t *testing.T, home, out string) config.Resolved {
	t.Helper()
	id := queuedID(out)
	db, err := trace.Open(filepath.Join(home, "data", "lathe"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row, err := db.Get(id)
	if err != nil {
		t.Fatalf("run %q: %v", id, err)
	}
	var spec worker.Spec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	return spec.Roster.Agents["scout"]
}

// managerLog is what makes a failure here diagnosable: the submitter's own
// output says nothing about why a queue was never drained.
func managerLog(home string) string {
	b, err := os.ReadFile(filepath.Join(home, "data", "lathe", "manager.log"))
	if err != nil {
		return "manager.log: " + err.Error()
	}
	return "manager.log:\n" + string(b)
}
