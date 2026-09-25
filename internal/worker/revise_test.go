package worker

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workspace"
)

// A run from before revisions can be revised when its records name the pull
// request, the branch and the commit; missing any of them is explained, and
// nothing is inferred to fill the gap.
func TestEvidence(t *testing.T) {
	dataRoot := t.TempDir()
	row := trace.Row{ID: "old", Workflow: "build", Branch: "feat/x", Commit: "abc"}
	if _, err := Evidence(dataRoot, row); err == nil || !strings.Contains(err.Error(), "no recorded pull request") {
		t.Fatalf("without pr.json: %v", err)
	}
	dir := ReportDir(dataRoot, "old", 0)
	os.MkdirAll(dir, 0o755)
	write(t, filepath.Join(dir, "pr.json"), `{"url":"https://github.com/o/r/pull/1"}`)
	ev, err := Evidence(dataRoot, row)
	if err != nil || ev != (workspace.Evidence{PR: "https://github.com/o/r/pull/1", Branch: "feat/x", Commit: "abc"}) {
		t.Fatalf("evidence = %+v, %v", ev, err)
	}
	row.Commit = ""
	if _, err := Evidence(dataRoot, row); err == nil || !strings.Contains(err.Error(), "does not record the branch and commit") {
		t.Fatalf("without a commit: %v", err)
	}
	row.Workflow = "implement"
	if _, err := Evidence(dataRoot, row); err == nil || !strings.Contains(err.Error(), "only a build opens a pull request") {
		t.Fatalf("an implement run: %v", err)
	}
	if got := ReportDir(dataRoot, "old", 2); got != filepath.Join(dataRoot, "runs", "old", "iteration-2") {
		t.Fatalf("report dir = %s", got)
	}
}

// A revision queued against a pull request that has since moved on fails
// when its worker starts, before any agent runs, and changes nothing.
func TestRevisionRechecksBeforeAnyAgent(t *testing.T) {
	stub := t.TempDir()
	write(t, filepath.Join(stub, "pi"), "#!/bin/sh\ntouch "+filepath.Join(stub, "spawned")+"\n")
	write(t, filepath.Join(stub, "gh"), `#!/bin/sh
echo '{"url":"u","state":"OPEN","headRefName":"feat/x","baseRefName":"main"}'
`)
	os.Chmod(filepath.Join(stub, "pi"), 0o755)
	os.Chmod(filepath.Join(stub, "gh"), 0o755)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir, dataRoot, origin := repo(t), t.TempDir(), t.TempDir()
	git(t, origin, "init", "-q", "--bare")
	git(t, dir, "checkout", "-qb", "feat/x")
	git(t, dir, "remote", "add", "origin", origin)
	git(t, dir, "push", "-q", "origin", "main", "feat/x")
	sha := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))

	cfg, err := config.Load(os.DirFS(filepath.Join("..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := cfg.Capture([]string{"planner"}, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := workspace.Local(dir)
	spec, _ := Spec{Version: SpecVersion, Workflow: "build", Request: "r", Workspace: ws, Roster: roster}.Marshal()
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, _ := db.Submit(trace.Request{Workflow: "build", Repo: ws.Path, Request: "r", Branch: "feat/x", Spec: spec, Exclusive: true})
	attempt := trace.NewAttemptID(id)
	db.Reserve(id, attempt, "")
	token := trace.NewClaimToken()
	db.Claim(attempt, token, "host", 1, Lease)
	db.SetShipped(id, attempt, "feat/x", sha)
	db.Complete(id, attempt, token, trace.StatusOK, "")
	os.MkdirAll(ReportDir(dataRoot, id, 0), 0o755)
	b, _ := json.Marshal(map[string]string{"url": "u"})
	write(t, filepath.Join(ReportDir(dataRoot, id, 0), "pr.json"), string(b))

	revision, _ := Spec{Version: SpecVersion, Workflow: "revise", Request: "more", Workspace: ws, Roster: roster}.Marshal()
	if _, err := db.Revise(id, 0, "more", revision); err != nil {
		t.Fatal(err)
	}
	// After queueing, someone commits to the pull request's branch by hand.
	write(t, filepath.Join(dir, "hello.txt"), "mine\n")
	git(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qam", "manual")
	manual := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))

	second := trace.NewAttemptID(id)
	if err := db.Reserve(id, second, ""); err != nil {
		t.Fatal(err)
	}
	if code := Execute(dataRoot, id, second, io.Discard); code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	if _, err := os.Stat(filepath.Join(stub, "spawned")); err == nil {
		t.Fatal("an agent ran against a pull request that had moved on")
	}
	row, _ := db.Get(id)
	its, _ := db.Iterations(id)
	if row.Status != trace.StatusFail || !strings.Contains(row.Reason, "changed outside lathe") ||
		!strings.Contains(row.Reason, "nothing was changed") || row.Commit != sha {
		t.Fatalf("run = %s %q @ %s", row.Status, row.Reason, row.Commit)
	}
	if its[0].Status != trace.StatusOK || its[1].Status != trace.StatusFail {
		t.Fatalf("iterations = %+v", its)
	}
	if head := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD")); head != manual {
		t.Fatal("the worker touched the person's commit")
	}
}
