package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
)

const shipURL = "https://github.com/acme/widget/pull/7"

// shipRepo is buildRepo plus a bare repository as its origin, which is what
// lets the pr phase's push be a real push rather than a mock.
func shipRepo(t *testing.T) (config.Config, string, string) {
	t.Helper()
	cfg, repo := buildRepo(t)
	origin := t.TempDir()
	gitBuild(t, origin, "init", "-q", "--bare")
	gitBuild(t, repo, "remote", "add", "origin", origin)
	return cfg, repo, origin
}

// jsonReply is one agent answer: a fenced json block inside the single
// assistant message a Pi stream would carry it in. Built through Marshal so a
// multi-paragraph commit message escapes the way a real one would.
func jsonReply(t *testing.T, report map[string]any) string {
	t.Helper()
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return piReply(t, "```json\n"+string(b)+"\n```")
}

// shipStub is codeStub with the brancher proposing branch.
func shipStub(t *testing.T, branch string) string {
	t.Helper()
	dir := codeStub(t)
	write(t, filepath.Join(dir, "brancher"), jsonReply(t, map[string]any{
		"summary": "the history prefixes with feat/", "branch": branch, "artifacts": []string{}}))
	return dir
}

// latestRun is the one run this test recorded, for its status and reason.
func latestRun(t *testing.T) trace.Row {
	t.Helper()
	db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Recent(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("runs: %v %v", rows, err)
	}
	return rows[0]
}

func cancelOnFile(t *testing.T, path string, limit time.Duration, before func()) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		timer := time.NewTimer(limit)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				cancel()
				return
			case <-ticker.C:
				if _, err := os.Stat(path); err == nil {
					if before != nil {
						before()
					}
					cancel()
					return
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return ctx, cancel
}

func TestBuildAuthPreflight(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			cfg, repo, _ := shipRepo(t)
			preflightProviders(&cfg, "pr-author")
			stub := codeStub(t)
			want := 0
			if fail {
				t.Setenv("LATHE_TEST_AUTH_FAIL", "later-auth")
				want = 1
			}
			if got := execute(t, cfg, "build", repo, "greet the world"); got != want {
				t.Fatalf("exit = %d, want %d", got, want)
			}
			assertPreflight(t, stub, 0, fail)
			if fail && totalSpawns(t, stub) != 0 {
				t.Fatal("auth failure consumed agent turns")
			}
		})
	}
}

func TestBuildEnvironmentFailurePublishesNothing(t *testing.T) {
	for _, exit := range []int{126, 127} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			cfg, repo, origin := shipRepo(t)
			stub := codeStub(t)
			lyingGit(t, stub, origin)
			head := rev(t, repo, "HEAD")
			command := fmt.Sprintf("echo ENV-TAIL; exit %d", exit)
			if exit == 127 {
				write(t, filepath.Join(stub, "tester.sh"), "echo artifact > test-output.tmp\n")
			}
			write(t, filepath.Join(stub, "tester"), testerReply(t, command, exit, "ENV-TAIL\n"))
			if got := execute(t, cfg, "build", repo, "greet the world"); got != 1 {
				t.Fatalf("exit = %d", got)
			}
			assertEnvironmentFailure(t, stub, latestRunDir(t), command, exit, 0)
			if rev(t, repo, "HEAD") != head || gitBuild(t, origin, "for-each-ref", "--format=%(refname)", "refs/heads") != "" {
				t.Fatal("environment failure committed or pushed")
			}
			if _, err := os.Stat(filepath.Join(stub, "pushed")); !os.IsNotExist(err) {
				t.Fatal("environment failure attempted a push")
			}
			if _, err := os.Stat(filepath.Join(repo, "test-output.tmp")); !os.IsNotExist(err) {
				t.Fatalf("test artifact survived: %v", err)
			}
			if _, err := os.Stat(filepath.Join(latestRunDir(t), "pr.json")); !os.IsNotExist(err) {
				t.Fatal("environment failure published a PR")
			}
		})
	}
}

// The branch, commit and pull request carry the accepted change.
func TestBuildShipsTheChange(t *testing.T) {
	cfg, repo, origin := shipRepo(t)
	stub := shipStub(t, "feat/greet-the-world")

	if code := execute(t, cfg, "build", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if got, want := totalSpawns(t, stub), len(Agents["build"]); got != want {
		t.Fatalf("spawn count = %d; want %d", got, want)
	}
	branch := builtBranch(t)
	if !strings.HasPrefix(branch, "feat/greet-the-world-") {
		t.Fatalf("branch %q does not carry the run's suffix", branch)
	}
	if got := strings.TrimSpace(gitBuild(t, repo, "branch", "--show-current")); got != branch {
		t.Fatalf("checked out %q; want %q", got, branch)
	}
	if got := gitBuild(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("tree left dirty: %q", got)
	}
	if got := gitBuild(t, repo, "log", "--oneline"); strings.Count(got, "\n") != 2 {
		t.Fatalf("want exactly one commit on top of the initial one: %s", got)
	}
	show := gitBuild(t, repo, "show", "--stat", "--format=%s%n%n%b", "HEAD")
	for _, want := range []string{"feat: greet the world", "The greeting now names its audience.", "hello.txt"} {
		if !strings.Contains(show, want) {
			t.Errorf("commit missing %q: %s", want, show)
		}
	}
	if got := gitBuild(t, repo, "show", "HEAD:hello.txt"); got != "hello world\n" {
		t.Fatalf("committed content = %q", got)
	}
	if got := strings.TrimSpace(gitBuild(t, origin, "rev-parse", branch)); got != strings.TrimSpace(gitBuild(t, repo, "rev-parse", "HEAD")) {
		t.Fatalf("origin is not at the pushed commit: %s", got)
	}
	if got, _ := exec.Command("git", "-C", repo, "config", "--get", "branch."+branch+".remote").Output(); len(got) != 0 {
		t.Fatalf("the push set an upstream in the shared configuration: %s", got)
	}

	args, _ := os.ReadFile(filepath.Join(stub, "gh-args"))
	for _, want := range []string{"pr", "create", "--head\n" + branch, "--title", "feat: greet the world", "--body-file"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("gh arguments missing %q: %s", want, args)
		}
	}
	if body, _ := os.ReadFile(filepath.Join(stub, "gh-body")); string(body) != "## What\n\nGreets the world.\n" {
		t.Errorf("gh body = %q", body)
	}

	b, err := os.ReadFile(filepath.Join(latestRunDir(t), "pr.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		URL, Title string
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.URL != shipURL || got.Title != "feat: greet the world" {
		t.Fatalf("pr.json = %s", b)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "implement.json")); err != nil {
		t.Fatal(err)
	}
	if args, _ := os.ReadFile(filepath.Join(stub, "gh-args")); strings.Contains(string(args), "--draft") {
		t.Fatal("an accepted change opened a draft")
	}
	assertShipped(t, repo)
}

// assertShipped checks the run row names the branch and commit the work landed
// on, not the ones the run was submitted against.
func assertShipped(t *testing.T, repo string) {
	t.Helper()
	row := latestRun(t)
	head := strings.TrimSpace(gitBuild(t, repo, "rev-parse", "HEAD"))
	if want := builtBranch(t); row.Branch != want || row.Commit != head {
		t.Fatalf("run records %s @ %s; want %s @ %s", row.Branch, row.Commit, want, head)
	}
}

// builtBranch is the branch the latest build creates from the stub's proposal.
func builtBranch(t *testing.T) string {
	t.Helper()
	return RunBranch(&run.Run{ID: latestRun(t).ID}, "feat/greet-the-world")
}

// A name git will not accept fails before a file has been written, which is
// the cheapest place for it to fail: the brancher is corrected twice, the run
// ends, and the checkout is exactly as it was.
func TestBuildRefusesAnUnusableBranchName(t *testing.T) {
	cfg, repo, _ := shipRepo(t)
	start := strings.TrimSpace(gitBuild(t, repo, "branch", "--show-current"))
	head := strings.TrimSpace(gitBuild(t, repo, "rev-parse", "HEAD"))
	stub := shipStub(t, "feat//retry")

	if code := execute(t, cfg, "build", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	// Two plan phases plus three brancher attempts: nothing reached the builder.
	if got := totalSpawns(t, stub); got != 5 {
		t.Fatalf("spawn count = %d; want 5", got)
	}
	if got := strings.TrimSpace(gitBuild(t, repo, "branch", "--show-current")); got != start {
		t.Fatalf("branch = %q, want %q", got, start)
	}
	if got := strings.TrimSpace(gitBuild(t, repo, "rev-parse", "HEAD")); got != head {
		t.Fatal("HEAD moved")
	}
	if got := gitBuild(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("tree touched: %q", got)
	}
}

// A failure after the commit keeps the commit: the run fails saying where the
// work is rather than leaving the engineer to find it.
func TestBuildKeepsTheCommitWhenGhFails(t *testing.T) {
	cfg, repo, _ := shipRepo(t)
	stub := shipStub(t, "feat/greet-the-world")
	write(t, filepath.Join(stub, "gh-fail"), "")

	if code := execute(t, cfg, "build", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got, want := totalSpawns(t, stub), len(Agents["build"]); got != want {
		t.Fatalf("spawn count = %d; want %d", got, want)
	}
	if got := gitBuild(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("tree left dirty: %q", got)
	}
	if got := gitBuild(t, repo, "log", "--oneline"); !strings.Contains(got, "feat: greet the world") {
		t.Fatalf("the commit was lost: %s", got)
	}
	if reason := latestRun(t).Reason; !strings.Contains(reason, "gh pr create") ||
		!strings.Contains(reason, "committed on "+builtBranch(t)) {
		t.Fatalf("reason = %q", reason)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "pr.json")); !os.IsNotExist(err) {
		t.Fatal("pr.json was written for a pull request that was never opened")
	}
	assertShipped(t, repo)
}

func TestBranchUsable(t *testing.T) {
	_, repo := buildRepo(t)
	current := strings.TrimSpace(gitBuild(t, repo, "branch", "--show-current"))
	gitBuild(t, repo, "tag", "v1.0.0")
	gitBuild(t, repo, "branch", "feat/taken")
	// Only origin has it: the plain name does not resolve, but Push would collide.
	gitBuild(t, repo, "update-ref", "refs/remotes/origin/feat/theirs", "HEAD")
	r := &run.Run{Repo: repo}

	for _, tc := range []struct {
		name, branch, want string
	}{
		{"usable", "feat/retry-backoff", ""},
		{"trimmed", "  feat/retry-backoff\n", ""},
		{"current branch", current, "already exists"},
		{"existing branch", "feat/taken", "already exists"},
		{"existing tag", "v1.0.0", "already exists"},
		{"branch on origin", "feat/theirs", "already exists"},
		{"invalid", "feat//retry", "will not accept"},
		{"space", "feat/retry backoff", "will not accept"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := BranchUsable(&BranchOutput{Branch: &tc.branch}, r)
			switch {
			case tc.want == "" && len(v) != 0:
				t.Fatalf("%q was rejected: %v", tc.branch, v)
			case tc.want != "" && (len(v) != 1 || !strings.Contains(v[0], tc.want)):
				t.Fatalf("%q: %v, want %q", tc.branch, v, tc.want)
			}
		})
	}
	// A different envelope type is another phase's business.
	if v := BranchUsable(&CommitOutput{}, r); v != nil {
		t.Fatalf("foreign envelope: %v", v)
	}
}

// Two builds proposing the same name get distinct branches: the gate checks
// the run's own name, which is the one checkout creates, so the second is
// accepted after the first has created its branch.
func TestRunBranchesAreDistinct(t *testing.T) {
	_, repo := buildRepo(t)
	proposal := "feat/retry-backoff"
	first := &run.Run{Repo: repo, ID: "20260101T000000Z_build_aaaa1111"}
	second := &run.Run{Repo: repo, ID: "20260101T000000Z_build_bbbb2222"}
	a, b := RunBranch(first, proposal), RunBranch(second, proposal)
	if a == b || a != proposal+"-aaaa1111" || RunBranch(first, proposal) != a {
		t.Fatalf("branches %q and %q", a, b)
	}
	gitBuild(t, repo, "branch", a)
	if v := BranchUsable(&BranchOutput{Branch: &proposal}, second); len(v) != 0 {
		t.Fatalf("the second build was refused: %v", v)
	}
	if v := BranchUsable(&BranchOutput{Branch: &proposal}, first); len(v) != 1 || !strings.Contains(v[0], "already exists") {
		t.Fatalf("the gate did not check the created name: %v", v)
	}
}

func TestShipEnvelopesRequireEveryKey(t *testing.T) {
	blank, empty := "  ", []string{}
	for _, tc := range []struct {
		name string
		zero run.Envelope
		want int
	}{
		{"branch", &BranchOutput{}, 3},
		{"branch blank", &BranchOutput{Summary: &blank, Branch: &blank, Wrote: &empty}, 2},
		{"commit", &CommitOutput{}, 3},
		{"pr", &PROutput{}, 4},
	} {
		if v := tc.zero.Validate(); len(v) != tc.want {
			t.Fatalf("%s: %v, want %d violations", tc.name, v, tc.want)
		}
	}
}
