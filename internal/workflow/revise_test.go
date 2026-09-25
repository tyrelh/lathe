package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workspace"
)

const prBranch = "feat/greet-the-world"

// revisable is a finished build with an open pull request: shipRepo, with the
// base branch on origin so a revision has a branch diff to show, a successful
// build, and a git on PATH that a test can make lie about a push. It returns
// the configuration, the repository, its origin and the stub directory.
func revisable(t *testing.T) (config.Config, string, string, string) {
	t.Helper()
	cfg, repo, origin := shipRepo(t)
	base := strings.TrimSpace(gitBuild(t, repo, "branch", "--show-current"))
	gitBuild(t, repo, "push", "-q", "origin", "HEAD")
	gitBuild(t, repo, "fetch", "-q", "origin")
	stub := codeStub(t)
	write(t, filepath.Join(stub, "gh-base"), base)
	lyingGit(t, stub, origin)
	if code := execute(t, cfg, "build", repo, "greet the world"); code != 0 {
		t.Fatalf("build: code = %d", code)
	}
	os.Remove(filepath.Join(stub, "pushed"))
	return cfg, repo, origin, stub
}

// lyingGit wraps the real git. With push-race in the stub directory it moves
// origin's pull request branch back a commit just before the push, as a
// concurrent update would; push-refuse fails the push without pushing;
// push-lie pushes and then reports failure, as a lost acknowledgement would;
// ls-fail makes origin unreadable once anything has been pushed.
func lyingGit(t *testing.T, stub, origin string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "git"), fmt.Sprintf(`#!/bin/sh
real=%[1]q; d=%[2]q; origin=%[3]q; ref=refs/heads/%[4]s
case " $* " in
  *" push "*)
    if [ -f "$d/push-race" ]; then "$real" --git-dir="$origin" update-ref $ref "$("$real" --git-dir="$origin" rev-parse $ref^)"; fi
    if [ -f "$d/push-refuse" ]; then echo "fatal: the remote end hung up" >&2; exit 1; fi
    "$real" "$@"; code=$?
    touch "$d/pushed"
    if [ -f "$d/push-lie" ]; then echo "fatal: the remote end hung up unexpectedly" >&2; exit 1; fi
    exit $code ;;
  *" ls-remote "*)
    if [ -f "$d/pushed" ] && [ -f "$d/ls-fail" ]; then echo "fatal: unable to access origin" >&2; exit 128; fi ;;
esac
exec "$real" "$@"
`, real, stub, origin, prBranch))
	if err := os.Chmod(filepath.Join(dir, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// reviseWith takes the run through a revision the way lathe does: the
// submitter's checks and acceptance, dispatch and claim, the worker's recheck
// under its claim, the graph, and settlement — cancelled if a cancellation was
// recorded, as the worker settles it. The worker's own wiring is in its
// package; this is the same sequence against the workflow alone.
func reviseWith(t *testing.T, cfg config.Config, repo, request string, ctx context.Context, out io.Writer) int {
	t.Helper()
	roster, err := cfg.Capture(Agents["revise"], config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe")
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row := latestRun(t)
	var pr prResult
	b, err := os.ReadFile(filepath.Join(dataRoot, "runs", row.ID, "pr.json"))
	if err != nil || json.Unmarshal(b, &pr) != nil {
		t.Fatalf("pr.json: %v", err)
	}
	ev := workspace.Evidence{PR: pr.URL, Branch: row.Branch, Commit: row.Commit}
	if err := trace.Revisable(row); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.CheckRevisable(repo, ev); err != nil {
		t.Fatalf("submission: %v", err)
	}
	n, err := db.Revise(row.ID, row.Iteration, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	attempt, token := trace.NewAttemptID(row.ID), trace.NewClaimToken()
	if err := db.Reserve(row.ID, attempt, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Claim(attempt, token, "host", os.Getpid(), time.Minute); err != nil {
		t.Fatal(err)
	}
	head, err := workspace.CheckRevisable(repo, ev)
	if err != nil {
		t.Fatalf("worker recheck: %v", err)
	}
	its, err := db.Iterations(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	rev := &run.Revision{PR: ev.PR, Branch: ev.Branch, Base: head.Base, Head: ev.Commit}
	for _, it := range its[:n] {
		dir := filepath.Join(dataRoot, "runs", row.ID)
		if it.N > 0 {
			dir = filepath.Join(dir, fmt.Sprintf("iteration-%d", it.N))
		}
		rev.Prior = append(rev.Prior, run.Prior{Iteration: it.N, Request: it.Request, Dir: dir})
	}
	if err := db.SetCommit(row.ID, attempt, row.Commit); err != nil {
		t.Fatal(err)
	}
	r, err := run.Open(run.Options{
		ID: row.ID, Workflow: "revise", Request: request, Repo: repo,
		Dir:      filepath.Join(dataRoot, "runs", row.ID, fmt.Sprintf("iteration-%d", n)),
		Snapshot: roster, DB: db, Out: out, Ctx: ctx,
		Iteration: n, Attempt: attempt, Revision: rev,
	})
	if err != nil {
		t.Fatal(err)
	}
	code := Graphs["revise"](r)
	status := r.Status()
	if cancelled, _ := db.Cancelled(row.ID); cancelled {
		status = trace.StatusCancelled
	}
	if err := db.Complete(row.ID, attempt, token, status, r.Reason()); err != nil {
		t.Fatal(err)
	}
	return code
}

func revise(t *testing.T, cfg config.Config, repo, request string) int {
	t.Helper()
	return reviseWith(t, cfg, repo, request, context.Background(), io.Discard)
}

func iterations(t *testing.T) []trace.Iteration {
	t.Helper()
	db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	its, err := db.Iterations(latestRun(t).ID)
	if err != nil {
		t.Fatal(err)
	}
	return its
}

func rev(t *testing.T, repo, ref string) string {
	t.Helper()
	return strings.TrimSpace(gitBuild(t, repo, "rev-parse", ref))
}

// A build and two revisions: one run and one pull request, three preserved
// iterations, phases numbered straight through, every report kept, and the
// run's spend cumulative in the row, the completion banner and the usage.
func TestRevisionsExtendTheRun(t *testing.T) {
	cfg, repo, origin, stub := revisable(t)
	built := rev(t, repo, "HEAD")

	write(t, filepath.Join(stub, "builder.sh"), "printf 'hello again\\n' > hello.txt\n")
	if code := revise(t, cfg, repo, "say hello again"); code != 0 {
		t.Fatalf("first revision: code = %d: %s", code, latestRun(t).Reason)
	}
	first := rev(t, repo, "HEAD")
	write(t, filepath.Join(stub, "builder.sh"), "printf 'hello thrice\\n' > hello.txt\n")
	var out strings.Builder
	if code := reviseWith(t, cfg, repo, "say it a third time", context.Background(), &out); code != 0 {
		t.Fatalf("second revision: code = %d: %s", code, latestRun(t).Reason)
	}
	head := rev(t, repo, "HEAD")

	db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n, _ := db.Total(); n != 1 {
		t.Fatalf("%d runs; a revision extends the one it revises", n)
	}
	if got := strings.Count(stubFile(t, stub, "gh-creates"), "created"); got != 1 {
		t.Fatalf("gh pr create ran %d times; the revisions reuse the pull request", got)
	}
	row := latestRun(t)
	if row.Status != trace.StatusOK || row.Iteration != 2 || row.Branch != prBranch || row.Commit != head {
		t.Fatalf("run = %+v", row)
	}
	its := iterations(t)
	if len(its) != 3 {
		t.Fatalf("iterations = %+v", its)
	}
	for i, want := range []struct{ request, base, commit string }{
		{"greet the world", "", built}, {"say hello again", built, first}, {"say it a third time", first, head},
	} {
		it := its[i]
		if it.Status != trace.StatusOK || it.Request != want.request || it.Commit != want.commit ||
			(i > 0 && (it.Base != want.base || it.Remote != want.commit)) || it.Started == "" || it.Ended == "" {
			t.Errorf("iteration %d = %+v", i, it)
		}
	}
	if row.Started != its[0].Started || row.Submitted != its[0].Submitted {
		t.Errorf("the run lost its original creation or first start: %+v vs %+v", row, its[0])
	}

	// The branch on origin carries all three commits, in order, one on another.
	if rev(t, origin, prBranch) != head {
		t.Fatal("origin is not at the last revision")
	}
	gitBuild(t, repo, "merge-base", "--is-ancestor", built, first)
	gitBuild(t, repo, "merge-base", "--is-ancestor", first, head)
	if got := gitBuild(t, repo, "show", "HEAD:hello.txt"); got != "hello thrice\n" {
		t.Fatalf("published content = %q", got)
	}

	phases, err := db.Phases(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen, last := map[int]bool{}, map[int]int{}
	for i, p := range phases {
		if p.Seq != i+1 {
			t.Fatalf("phase %d has seq %d; numbering must continue across iterations", i, p.Seq)
		}
		if p.Iteration < 2 && last[p.Iteration+1] > 0 {
			t.Fatalf("phase %s of iteration %d follows a later iteration's", p.ID, p.Iteration)
		}
		seen[p.Iteration], last[p.Iteration] = true, p.Seq
	}
	if len(seen) != 3 {
		t.Fatalf("phases span iterations %v", seen)
	}

	dir := latestRunDir(t)
	for _, f := range []string{"plan.json", "validation.json", "pr.json",
		"iteration-1/plan.json", "iteration-1/validation.json", "iteration-2/plan.json", "iteration-2/implement.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("report %s: %v", f, err)
		}
	}

	spawns := totalSpawns(t, stub)
	if row.Tokens != 100*spawns || fmt.Sprintf("%.5f", row.Cost) != fmt.Sprintf("%.5f", 0.001*float64(spawns)) {
		t.Fatalf("run totals %d tokens $%.5f over %d responses", row.Tokens, row.Cost, spawns)
	}
	if !strings.Contains(out.String(), fmt.Sprintf(" %d tokens ", 100*spawns)) {
		t.Fatalf("the completion banner is not cumulative: %s", out.String())
	}

	// The planner saw the history: the original request, the earlier plan and
	// the diff the pull request carries; the next one, the earlier revision.
	brief := stubFile(t, stub, "args.planner.1")
	for _, want := range []string{"say hello again", "greet the world", "Previous plan", "Branch diff against", "+hello world"} {
		if !strings.Contains(brief, want) {
			t.Errorf("revision planner brief missing %q", want)
		}
	}
	if brief := stubFile(t, stub, "args.planner.2"); !strings.Contains(brief, "revision 1: say hello again") {
		t.Errorf("second revision's brief does not name the first: %s", brief)
	}
	if n := spawnsOf(t, stub, "brancher") + spawnsOf(t, stub, "pr-author"); n != 2 {
		t.Fatalf("a revision named a branch or wrote a pull request: %d spawns", n)
	}
}

// A request that needs no code change is validated as the branch stands,
// succeeds without a commit, and leaves the run open to another revision.
func TestRevisionWithNoChanges(t *testing.T) {
	cfg, repo, origin, stub := revisable(t)
	built := rev(t, repo, "HEAD")
	write(t, filepath.Join(stub, "planner.1"), planReply(t, ""))

	if code := revise(t, cfg, repo, "confirm the greeting needs nothing else"); code != 0 {
		t.Fatalf("code = %d: %s", code, latestRun(t).Reason)
	}
	if n := spawnsOf(t, stub, "builder"); n != 1 {
		t.Fatalf("builder spawned %d times; a plan with no files has nothing to build", n)
	}
	if n := spawnsOf(t, stub, "committer"); n != 1 {
		t.Fatalf("committer spawned %d times; nothing to commit", n)
	}
	if n := spawnsOf(t, stub, "tester"); n != 2 {
		t.Fatalf("tester spawned %d times; the unchanged branch is still validated", n)
	}
	row := latestRun(t)
	if row.Status != trace.StatusOK || row.Commit != built || rev(t, repo, "HEAD") != built || rev(t, origin, prBranch) != built {
		t.Fatalf("run = %+v", row)
	}
	if !strings.Contains(row.Reason, "no code changes") {
		t.Fatalf("reason = %q", row.Reason)
	}
	if err := trace.Revisable(row); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.CheckRevisable(repo, workspace.Evidence{PR: shipURL, Branch: prBranch, Commit: built}); err != nil {
		t.Fatalf("no longer revisable: %v", err)
	}
}

// Unaccepted and cancelled revisions keep their work and reports locally,
// publish nothing, and end the run's revisions.
func TestUnfinishedRevisionsPublishNothing(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason string
		setup                func(t *testing.T, stub string) (context.Context, func())
	}{
		{"validation incomplete", trace.StatusFail, "the changes are left uncommitted", func(t *testing.T, stub string) (context.Context, func()) {
			write(t, filepath.Join(stub, "code-review-general"), piReply(t, "no json here"))
			return context.Background(), func() {}
		}},
		{"cancelled", trace.StatusCancelled, "", func(t *testing.T, stub string) (context.Context, func()) {
			write(t, filepath.Join(stub, "code-review-slop.sh"), "touch "+filepath.Join(stub, "reviewing")+"; sleep 30\n")
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				for i := 0; i < 1000; i++ {
					if _, err := os.Stat(filepath.Join(stub, "reviewing")); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
				if err == nil {
					db.RequestCancel(latestRun(t).ID)
					db.Close()
				}
				cancel()
			}()
			return ctx, cancel
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo, origin, stub := revisable(t)
			built := rev(t, repo, "HEAD")
			write(t, filepath.Join(stub, "builder.sh"), "printf 'hello again\\n' > hello.txt\n")
			ctx, cancel := tc.setup(t, stub)
			defer cancel()

			if code := reviseWith(t, cfg, repo, "say hello again", ctx, io.Discard); code == 0 {
				t.Fatal("an unfinished revision succeeded")
			}
			if b, _ := os.ReadFile(filepath.Join(repo, "hello.txt")); string(b) != "hello again\n" {
				t.Fatalf("the revision's work was not kept: %q", b)
			}
			if rev(t, repo, "HEAD") != built || rev(t, origin, prBranch) != built {
				t.Fatal("an unfinished revision committed or pushed")
			}
			if n := spawnsOf(t, stub, "committer"); n != 1 {
				t.Fatalf("committer spawned %d times", n)
			}
			row := latestRun(t)
			if row.Status != tc.status || row.Commit != built || !strings.Contains(row.Reason, tc.reason) {
				t.Fatalf("run = %s %q @ %s", row.Status, row.Reason, row.Commit)
			}
			if _, err := os.Stat(filepath.Join(latestRunDir(t), "iteration-1", "plan.json")); err != nil {
				t.Fatalf("the revision's reports were not kept: %v", err)
			}
			if err := trace.Revisable(row); err == nil {
				t.Fatal("a run whose latest revision did not succeed still takes another")
			}
		})
	}
}

// Publication against a branch or pull request that changes under it: the
// lease refuses to overwrite a concurrent update, a lost acknowledgement is
// settled by asking origin, and a pull request closed around the push fails
// the iteration saying whether the commit went out.
func TestRevisionPublication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		markers []string
		states  string // gh pr view answers, one per view: submission, worker recheck, before and after the push
		code    int
		reason  []string
		// arrived is whether the commit reached origin; shipped, whether the
		// run records it as published, which needs origin to confirm it.
		arrived, shipped bool
	}{
		{name: "delivered", code: 0, arrived: true, shipped: true},
		{name: "lost acknowledgement", markers: []string{"push-lie"}, code: 0, arrived: true, shipped: true},
		{name: "push refused", markers: []string{"push-refuse"}, code: 1, reason: []string{"not published", "git push failed"}},
		{name: "concurrent update", markers: []string{"push-race"}, code: 1, reason: []string{"not published", "committed locally"}},
		{name: "origin unreadable after the push", markers: []string{"ls-fail"}, code: 1, arrived: true,
			reason: []string{"publication is uncertain", "last verified at"}},
		{name: "closed before the push", states: "OPEN\nOPEN\nCLOSED\n", code: 1, reason: []string{"is closed; nothing was pushed"}},
		{name: "merged during the push", states: "OPEN\nOPEN\nOPEN\nMERGED\n", code: 1, arrived: true, shipped: true,
			reason: []string{"pushed", "is now merged"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo, origin, stub := revisable(t)
			built := rev(t, repo, "HEAD")
			write(t, filepath.Join(stub, "builder.sh"), "printf 'hello again\\n' > hello.txt\n")
			for _, m := range tc.markers {
				write(t, filepath.Join(stub, m), "")
			}
			if tc.states != "" {
				write(t, filepath.Join(stub, "gh-states"), tc.states)
			}

			if code := revise(t, cfg, repo, "say hello again"); code != tc.code {
				t.Fatalf("code = %d: %s", code, latestRun(t).Reason)
			}
			local := rev(t, repo, "HEAD")
			if local == built {
				t.Fatal("the accepted change was not committed")
			}
			row, its := latestRun(t), iterations(t)
			for _, want := range tc.reason {
				if !strings.Contains(row.Reason, want) {
					t.Errorf("reason %q missing %q", row.Reason, want)
				}
			}
			if its[1].Commit != local {
				t.Errorf("the iteration does not record its local commit: %+v", its[1])
			}
			remote, want := rev(t, origin, prBranch), built
			switch {
			case tc.arrived:
				want = local
			case tc.name == "concurrent update":
				want = rev(t, repo, built+"^") // the update lathe must not overwrite
			}
			if remote != want {
				t.Fatalf("origin at %s; want %s", remote, want)
			}
			if shipped := row.Commit == local; shipped != tc.shipped || (!shipped && row.Commit != built) {
				t.Fatalf("the run records %s as shipped (local %s, built %s)", row.Commit, local, built)
			}
		})
	}
}
