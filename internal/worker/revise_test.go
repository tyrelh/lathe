package worker

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workspace"
)

func TestRecordedPRURL(t *testing.T) {
	root := t.TempDir()
	row := trace.Row{ID: "history", Iteration: 3}
	if got := RecordedPRURL(root, row); got != "" {
		t.Fatalf("missing record: %q", got)
	}
	for _, n := range []int{0, 2, 3} {
		dir := ReportDir(root, row.ID, n)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(ReportDir(root, row.ID, 0), "pr.json"), `{"url":"root"}`)
	if got := RecordedPRURL(root, row); got != "root" {
		t.Fatalf("root fallback: %q", got)
	}
	write(t, filepath.Join(ReportDir(root, row.ID, 2), "pr.json"), `{"url":"later"}`)
	write(t, filepath.Join(ReportDir(root, row.ID, 3), "pr.json"), `{broken`)
	if got := RecordedPRURL(root, row); got != "later" {
		t.Fatalf("latest readable: %q", got)
	}
}

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

type shippedFixture struct {
	stub, dir, dataRoot, origin, sha, id string
	db                                   *trace.DB
	roster                               config.Snapshot
	ws                                   workspace.Desc
}

func shippedBuild(t *testing.T, withBase bool, ghBase string) shippedFixture {
	t.Helper()
	f := shippedFixture{stub: t.TempDir(), dir: repo(t), dataRoot: t.TempDir(), origin: t.TempDir()}
	stubBin(t, f.stub, "pi", "#!/bin/sh\ntouch "+filepath.Join(f.stub, "spawned")+"\n")
	stubBin(t, f.stub, "gh", "#!/bin/sh\necho '{\"url\":\"u\",\"state\":\"OPEN\",\"headRefName\":\"feat/x\",\"baseRefName\":\""+ghBase+"\"}'\n")
	t.Setenv("PATH", f.stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	git(t, f.origin, "init", "-q", "--bare")
	git(t, f.dir, "checkout", "-qb", "feat/x")
	git(t, f.dir, "remote", "add", "origin", f.origin)
	git(t, f.dir, "push", "-q", "origin", "main", "feat/x")
	f.sha = strings.TrimSpace(git(t, f.dir, "rev-parse", "HEAD"))
	var err error
	f.roster, err = seeded(t).Capture([]string{"planner"}, config.Overrides{})
	must(t, err)
	f.ws, err = workspace.Local(f.dir)
	must(t, err)
	base := ""
	if withBase {
		base = f.sha
	}
	spec, err := (Spec{Version: SpecVersion, Workflow: "build", Request: "r", Workspace: f.ws, Roster: f.roster, Base: base}).Marshal()
	must(t, err)
	f.db, err = trace.Open(f.dataRoot)
	must(t, err)
	t.Cleanup(func() { _ = f.db.Close() })
	f.id, err = f.db.Submit(trace.Request{Workflow: "build", Repo: f.ws.Path, Request: "r", Branch: "feat/x", Spec: spec})
	must(t, err)
	attempt, token := trace.NewAttemptID(f.id), trace.NewClaimToken()
	must(t, f.db.Reserve(f.id, attempt, ""))
	_, err = f.db.Claim(attempt, token, "host", 1, Lease)
	must(t, err)
	must(t, f.db.SetShipped(f.id, attempt, "feat/x", f.sha))
	must(t, f.db.Complete(f.id, attempt, token, trace.StatusOK, ""))
	must(t, os.MkdirAll(ReportDir(f.dataRoot, f.id, 0), 0o700))
	write(t, filepath.Join(ReportDir(f.dataRoot, f.id, 0), "pr.json"), `{"url":"u"}`)
	return f
}

// A revision queued against a pull request that has since moved on fails
// when its worker starts, before any agent runs, and changes nothing.
func TestRevisionRechecksBeforeAnyAgent(t *testing.T) {
	f := shippedBuild(t, false, "main")
	revision, err := (Spec{Version: SpecVersion, Workflow: "revise", Request: "more", Workspace: f.ws, Roster: f.roster, Base: f.sha}).Marshal()
	must(t, err)
	if _, err := f.db.Revise(f.id, 0, "more", revision); err != nil {
		t.Fatal(err)
	}
	// After queueing, someone pushes to the pull request's branch by hand, and
	// leaves the checkout dirty, which on its own would not matter.
	write(t, filepath.Join(f.dir, "hello.txt"), "mine\n")
	git(t, f.dir, "commit", "-qam", "manual")
	git(t, f.dir, "push", "-q", "origin", "feat/x")
	write(t, filepath.Join(f.dir, "hello.txt"), "dirty\n")
	manual := strings.TrimSpace(git(t, f.dir, "rev-parse", "HEAD"))

	second := trace.NewAttemptID(f.id)
	if err := f.db.Reserve(f.id, second, ""); err != nil {
		t.Fatal(err)
	}
	if code := Execute(f.dataRoot, f.id, second, io.Discard); code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	if _, err := os.Stat(filepath.Join(f.stub, "spawned")); err == nil {
		t.Fatal("an agent ran against a pull request that had moved on")
	}
	row, _ := f.db.Get(f.id)
	its, _ := f.db.Iterations(f.id)
	if row.Status != trace.StatusFail || !strings.Contains(row.Reason, "changed outside lathe") ||
		!strings.Contains(row.Reason, "nothing was changed") || row.Commit != f.sha {
		t.Fatalf("run = %s %q @ %s", row.Status, row.Reason, row.Commit)
	}
	if its[0].Status != trace.StatusOK || its[1].Status != trace.StatusFail {
		t.Fatalf("iterations = %+v", its)
	}
	if head := strings.TrimSpace(git(t, f.dir, "rev-parse", "HEAD")); head != manual {
		t.Fatal("the worker touched the person's commit")
	}
	if _, err := os.Stat(workspace.WorktreeRoot(f.dataRoot)); !os.IsNotExist(err) {
		t.Fatal("a refused revision created a worktree")
	}
}

// A pull request base that cannot be fetched fails the revision before any
// agent or worktree: the planner's diff would describe the wrong change.
func TestRevisionNeedsItsBase(t *testing.T) {
	f := shippedBuild(t, true, "no-such-base")
	revision, err := (Spec{Version: SpecVersion, Workflow: "revise", Request: "more", Workspace: f.ws, Roster: f.roster, Base: f.sha}).Marshal()
	must(t, err)
	if _, err := f.db.Revise(f.id, 0, "more", revision); err != nil {
		t.Fatal(err)
	}
	second := trace.NewAttemptID(f.id)
	if err := f.db.ReserveMode(f.id, second, "", Mode(revision)); err != nil {
		t.Fatal(err)
	}
	if code := Execute(f.dataRoot, f.id, second, io.Discard); code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	row, _ := f.db.Get(f.id)
	if row.Status != trace.StatusFail || !strings.Contains(row.Reason, "the pull request's base") {
		t.Fatalf("run = %s %q", row.Status, row.Reason)
	}
	if _, err := os.Stat(filepath.Join(f.stub, "spawned")); err == nil {
		t.Fatal("an agent ran without the pull request's base")
	}
	if _, err := os.Stat(workspace.WorktreeRoot(f.dataRoot)); !os.IsNotExist(err) {
		t.Fatal("a revision without its base created a worktree")
	}
}
