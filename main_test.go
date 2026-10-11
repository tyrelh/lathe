package main

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
)

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
	repo := gitRepo(t, "provider = \"anthropic\"\nmodel = \"from-lathe-toml\"\n[agents.builder]\nmodel = \"also-ignored\"\n")
	override := filepath.Join(home, "data", "lathe", "prompts", "builder", "system.md")
	os.MkdirAll(filepath.Dir(override), 0o755)
	os.WriteFile(override, []byte("You build by override.\n"), 0o644)

	seeded, err := fs.ReadFile(Seed, "agents/builder/system.md")
	if err != nil {
		t.Fatal(err)
	}
	first, err := capture("build", repo, config.Overrides{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b := first.Agents["builder"]
	if b.SystemPrompt != string(seeded) || b.Provider != "moonshotai" || b.Model != "kimi-k2.7-code" {
		t.Fatalf("builder = %s/%s %q", b.Provider, b.Model, b.SystemPrompt)
	}
	if len(first.Routing["builder"].Tiers) != 3 || len(first.Routing["planner"].Tiers) != 3 {
		t.Fatalf("routing = %+v", first.Routing)
	}

	system := filepath.Join(home, ".lathe", "agents", "builder", "system.md")
	if err := os.WriteFile(system, []byte("You build by edit.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := capture("build", repo, config.Overrides{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Agents["builder"].SystemPrompt; got != "You build by edit.\n" {
		t.Fatalf("captured after the edit: %q", got)
	}
	if got := first.Agents["builder"].SystemPrompt; got != string(seeded) {
		t.Fatalf("an earlier capture changed: %q", got)
	}

	flagged, err := capture("build", repo, config.Overrides{Model: "m"}, io.Discard)
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
		if _, err := capture("build", repo, config.Overrides{}, io.Discard); err == nil || !strings.Contains(err.Error(), path) {
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
	if testing.Short() {
		t.Skip("spawns a manager and a worker")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the binary with")
	}

	home := t.TempDir()
	bin := filepath.Join(home, "lathe")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building lathe: %v\n%s", err, out)
	}
	// The manager outlives the submitter by design, so the test is what has to
	// stop it. The binary path is unique to this test, which is what makes
	// this safe to match on.
	t.Cleanup(func() { exec.Command("pkill", "-f", bin+" manager").Run() })

	repo := gitRepo(t, "")
	// Submissions run from a repository with a lathe.toml of its own, which
	// lathe no longer reads.
	invokedFrom := gitRepo(t, `model = "from-the-invocation-directory"`)

	// A fake agent on PATH: the worker resolves pi the way production does,
	// and the manager passes PATH through to it.
	stub := t.TempDir()
	reply := `{"type":"message_end","message":{"role":"assistant","stopReason":"stop",` +
		`"usage":{"totalTokens":10,"cost":{"total":0.001}},"content":[{"type":"text","text":` +
		"\"Looked.\\n\\n```json\\n{\\\"summary\\\":\\\"it dispatches\\\",\\\"findings\\\":[],\\\"artifacts\\\":[]}\\n```\"}]}}"
	os.WriteFile(filepath.Join(stub, "reply.jsonl"), []byte(reply+"\n"), 0o644)
	os.WriteFile(filepath.Join(stub, "pi"), []byte("#!/bin/sh\ncat "+filepath.Join(stub, "reply.jsonl")+"\n"), 0o755)

	env := []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
	}
	lathe := func(t *testing.T, args ...string) (string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = invokedFrom
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		var exit *exec.ExitError
		if err != nil {
			if !asExit(err, &exit) {
				t.Fatalf("lathe %v: %v\n%s", args, err, out)
			}
			code = exit.ExitCode()
		}
		return string(out), code
	}

	// The submitter blocks: no manager is running, so it starts one, and the
	// run comes back finished.
	out, code := lathe(t, "scout", "--repo", repo, "what is here")
	if code != 0 {
		t.Fatalf("lathe scout exited %d:\n%s\n%s", code, out, managerLog(home))
	}
	if !strings.Contains(out, "queued") || !strings.Contains(out, ": ok") {
		t.Fatalf("scout output:\n%s\n%s", out, managerLog(home))
	}
	if strings.Contains(out, "lathe.toml") {
		t.Fatalf("a repository with no lathe.toml was not silent about it:\n%s", out)
	}

	if list, _ := lathe(t, "runs"); !strings.Contains(list, "scout") || !strings.Contains(list, "ok") {
		t.Fatalf("lathe runs:\n%s", list)
	}

	// --detach returns immediately with the ID, and wait picks the same run up
	// and reports its outcome.
	out, code = lathe(t, "scout", "--detach", "--repo", repo, "what else is here")
	if code != 0 {
		t.Fatalf("detached submission exited %d:\n%s", code, out)
	}
	fields := strings.Fields(out)
	id := fields[1]
	if !strings.HasSuffix(fields[2], "queued") {
		t.Fatalf("detached output: %q", out)
	}
	if waited, code := lathe(t, "wait", id); code != 0 || !strings.Contains(waited, ": ok") {
		t.Fatalf("lathe wait %s exited %d:\n%s\n%s", id, code, waited, managerLog(home))
	}
	if shown, code := lathe(t, "show", "--json", id); code != 0 || !strings.Contains(shown, `"status": "ok"`) {
		t.Fatalf("lathe show exited %d:\n%s", code, shown)
	}

	// Issue input survives submission, the manager and worker, then reaches
	// both planning agents. A lookup failure must never invoke an agent.
	for _, args := range [][]string{
		{"plan", "--issue", ""}, {"plan", "--issue", "--web"},
		{"plan", "--issue", "41", "also a prompt"}, {"scout", "--issue", "41"},
	} {
		if out, code := lathe(t, args...); code != 2 {
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
	out, code = lathe(t, "plan", "--repo", repo, "--issue", "41")
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
	issueRunID := strings.Fields(out)[1]
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
		out, code = lathe(t, workflow, "--repo", repo, "--issue", "41")
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
	out, code = lathe(t, "scout", "--detach", "--repo", configured, "--thinking", "high", "what is configured")
	if code != 0 || strings.Contains(out, "lathe.toml") {
		t.Fatalf("configured submission exited %d:\n%s", code, out)
	}
	if a := capturedScout(t, home, out); a.Provider != "moonshotai" || a.Model != "kimi-k2.7-code" || a.Thinking != "high" {
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
	out, code = lathe(t, "scout", "--detach", "--repo", repo, "what is edited")
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
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hello\n"), 0o644)
	if project != "" {
		os.WriteFile(filepath.Join(repo, "lathe.toml"), []byte(project), 0o644)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo
}

// capturedScout reads back the scout a detached submission froze into its
// specification, from the run ID in the submission's output.
func capturedScout(t *testing.T, home, out string) config.Resolved {
	t.Helper()
	var id string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[2] == "queued" {
			id = f[1]
		}
	}
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

func asExit(err error, out **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*out = e
	}
	return ok
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
