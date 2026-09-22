package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// shipStub serves one reply per spawn in the order a build's phases run, and
// puts a `gh` beside `pi` that records what it was asked and prints a URL.
func shipStub(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "plan"), planReply(t, `"hello.txt"`))
	write(t, filepath.Join(dir, "review"), reviewOK(t))
	write(t, filepath.Join(dir, "branch"), jsonReply(t, map[string]any{
		"summary": "the history prefixes with feat/", "branch": branch, "artifacts": []string{}}))
	write(t, filepath.Join(dir, "implement"), jsonReply(t, map[string]any{
		"summary": "updated greeting", "changed": []string{"hello.txt"},
		"needed": []string{}, "artifacts": []string{"hello.txt"}}))
	write(t, filepath.Join(dir, "test"), jsonReply(t, map[string]any{
		"summary": "discovered", "command": "true", "failures": []string{}, "artifacts": []string{}}))
	write(t, filepath.Join(dir, "commit"), jsonReply(t, map[string]any{
		"summary":   "conventional commits, as the history does",
		"message":   "feat: greet the world\n\nThe greeting now names its audience.",
		"artifacts": []string{}}))
	write(t, filepath.Join(dir, "pr"), jsonReply(t, map[string]any{
		"summary": "filled in the template", "title": "feat: greet the world",
		"body": "## What\n\nGreets the world.\n", "artifacts": []string{}}))

	gh := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %[1]q/gh-args
for a in "$@"; do [ ! -f "$a" ] || cp "$a" %[1]q/gh-body; done
if [ -f %[1]q/gh-fail ]; then echo "no git remote found" >&2; exit 1; fi
echo "Creating pull request for feat/x into main"
echo %[2]q
`, dir, shipURL)
	write(t, filepath.Join(dir, "gh"), gh)
	if err := os.Chmod(filepath.Join(dir, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Dispatch on which agent is being spawned rather than on the spawn index:
	// a correction round is a second spawn of the same phase, and an index would
	// serve it the next phase's answer.
	onPath(t, dir, fmt.Sprintf(`#!/bin/sh
n=0
[ ! -f %[1]q/count ] || n=$(cat %[1]q/count)
echo $((n+1)) > %[1]q/count
printf '%%s\n' "$@" > %[1]q/args$n
case "$*" in
*"You are the planner"*)       cat %[1]q/plan ;;
*"You are the plan-reviewer"*) cat %[1]q/review ;;
*"You are the brancher"*)      cat %[1]q/branch ;;
*"You are the builder"*)       printf 'hello world\n' > hello.txt; cat %[1]q/implement ;;
*"You are the tester"*)        cat %[1]q/test ;;
*"You are the committer"*)     cat %[1]q/commit ;;
*)                             cat %[1]q/pr ;;
esac
`, dir))
	return dir
}

func spawns(t *testing.T, stub string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(stub, "count"))
	return strings.TrimSpace(string(b))
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

// The whole workflow: the branch exists and is checked out, one commit carries
// the change, the branch reached origin, and pr.json holds the URL gh printed.
func TestBuildShipsTheChange(t *testing.T) {
	cfg, repo, origin := shipRepo(t)
	stub := shipStub(t, "feat/greet-the-world")

	if code := execute(t, cfg, "build", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if got := spawns(t, stub); got != "7" {
		t.Fatalf("spawn count = %s; want 7", got)
	}
	if got := strings.TrimSpace(gitBuild(t, repo, "branch", "--show-current")); got != "feat/greet-the-world" {
		t.Fatalf("checked out %q", got)
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
	if got := strings.TrimSpace(gitBuild(t, origin, "rev-parse", "feat/greet-the-world")); got != strings.TrimSpace(gitBuild(t, repo, "rev-parse", "HEAD")) {
		t.Fatalf("origin is not at the pushed commit: %s", got)
	}

	args, _ := os.ReadFile(filepath.Join(stub, "gh-args"))
	for _, want := range []string{"pr", "create", "--title", "feat: greet the world", "--body-file"} {
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
	assertShipped(t, repo)
}

// assertShipped checks the run row names the branch and commit the work landed
// on, not the ones the run was submitted against.
func assertShipped(t *testing.T, repo string) {
	t.Helper()
	row := latestRun(t)
	head := strings.TrimSpace(gitBuild(t, repo, "rev-parse", "HEAD"))
	if row.Branch != "feat/greet-the-world" || row.Commit != head {
		t.Fatalf("run records %s @ %s; want feat/greet-the-world @ %s", row.Branch, row.Commit, head)
	}
}

// A name that is already taken fails before a file has been written, which is
// the cheapest place for it to fail: the brancher is corrected twice, the run
// ends, and the checkout is exactly as it was.
func TestBuildRefusesATakenBranchName(t *testing.T) {
	cfg, repo, _ := shipRepo(t)
	start := strings.TrimSpace(gitBuild(t, repo, "branch", "--show-current"))
	head := strings.TrimSpace(gitBuild(t, repo, "rev-parse", "HEAD"))
	stub := shipStub(t, start)

	if code := execute(t, cfg, "build", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	// Two plan phases plus three brancher attempts: nothing reached the builder.
	if got := spawns(t, stub); got != "5" {
		t.Fatalf("spawn count = %s; want 5", got)
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
	if got := spawns(t, stub); got != "7" {
		t.Fatalf("spawn count = %s; want 7", got)
	}
	if got := gitBuild(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("tree left dirty: %q", got)
	}
	if got := gitBuild(t, repo, "log", "--oneline"); !strings.Contains(got, "feat: greet the world") {
		t.Fatalf("the commit was lost: %s", got)
	}
	if reason := latestRun(t).Reason; !strings.Contains(reason, "gh pr create") ||
		!strings.Contains(reason, "committed on feat/greet-the-world") {
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
		{"current branch", current, "already checked out"},
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
