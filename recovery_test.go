package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/process"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workspace"
)

// A writing run is submitted from whatever state the checkout is in. It
// records the commit HEAD is on, and the checkout is left exactly as it was.
func TestWritingSubmissionFromADirtyCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	repo := gitRepo(t, "")
	head := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("mine\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o644)
	gitIn(t, repo, "add", "staged.txt")
	os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o644)
	before := gitIn(t, repo, "status", "--porcelain")

	for _, name := range []string{"implement", "build"} {
		sub, err := prepare(name, repo, "greet the world", "", config.Overrides{}, io.Discard)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var spec worker.Spec
		if err := json.Unmarshal(sub.Spec, &spec); err != nil {
			t.Fatal(err)
		}
		if sub.Base != head || spec.Base != head || spec.Version != worker.SpecVersion || sub.Branch != "main" {
			t.Fatalf("%s: base %s, spec base %s v%d, branch %q; want %s on main", name, sub.Base, spec.Base, spec.Version, sub.Branch, head)
		}
	}
	if got := gitIn(t, repo, "status", "--porcelain"); got != before {
		t.Fatalf("the checkout changed:\n%s\nwant:\n%s", got, before)
	}
}

// A worker killed anywhere in a supervised launch is settled by the next
// manager without a person: an intent that never registered is cancelled
// and admitted nothing, a registered supervisor that never got GO started
// nothing, and an admitted payload's writers are stopped and its work saved.
// Every case ends with the run lost and its worktree removed. The last case
// also has the first restart's cleanup fail, and the second finish it.
func TestCrashWindowsRecoverOnRestart(t *testing.T) {
	bin := buildLathe(t)
	for _, stage := range []string{"intent", "started", "registered", "writing"} {
		t.Run(stage, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			dataRoot, err := trace.DataRoot()
			if err != nil {
				t.Fatal(err)
			}
			repo := gitRepo(t, "")
			sub, err := prepare("implement", repo, "greet the world", "", config.Overrides{}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			db, err := trace.Open(dataRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			runID, err := db.Submit(sub.Request)
			if err != nil {
				t.Fatal(err)
			}
			attemptID := trace.NewAttemptID(runID)
			logPath := worker.LogPath(dataRoot, runID, attemptID)
			os.MkdirAll(filepath.Dir(logPath), 0o700)
			if err := db.ReserveMode(runID, attemptID, logPath, trace.ModeWorktree); err != nil {
				t.Fatal(err)
			}

			marks := t.TempDir()
			w := exec.Command(os.Args[0], "-test.run=^TestCrashingWorker$")
			w.Env = append(os.Environ(), "LATHE_CRASH_STAGE="+stage, "LATHE_CRASH_ATTEMPT="+attemptID,
				"LATHE_CRASH_MARKS="+marks, "LATHE_CRASH_HELPER="+bin)
			wlog, _ := os.Create(filepath.Join(marks, "worker.log"))
			w.Stdout, w.Stderr = wlog, wlog
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			if err := db.SetHandle(attemptID, strconv.Itoa(w.Process.Pid), w.Process.Pid); err != nil {
				t.Fatal(err)
			}
			eventually(t, 30*time.Second, "the worker reaching its barrier", func() bool {
				return exists(filepath.Join(marks, "barrier"))
			}, func() string { return readFile(filepath.Join(marks, "worker.log")) })
			w.Process.Kill()
			w.Wait()

			path := workspace.WorktreePath(dataRoot, runID, 0)
			if stage == "writing" {
				// The first restart cannot take the worktree's lock, so its
				// cleanup fails: the work stays recorded and on disk.
				lock, err := workspace.Acquire(dataRoot, path)
				if err != nil {
					t.Fatal(err)
				}
				out, stop := startManager(t, bin)
				eventually(t, 30*time.Second, "the manager reporting unresolved recovery", func() bool {
					return strings.Contains(out(), "recovery is unresolved in ")
				}, out)
				stop()
				lock.Release()
				its, _ := db.Iterations(runID)
				if !exists(path) || len(its) != 1 || its[0].WorktreeState == trace.WorktreeRemoved {
					t.Fatalf("the failed cleanup removed the worktree: %+v", its)
				}
				if state := its[0].WorktreeState; state != trace.WorktreePending || its[0].CleanupError == "" {
					t.Fatalf("after the failed cleanup the worktree is %q (%q); want %q with its error", state, its[0].CleanupError, trace.WorktreePending)
				}
			}

			out, _ := startManager(t, bin)
			eventually(t, 30*time.Second, "recovery removing the worktree", func() bool {
				its, err := db.Iterations(runID)
				return err == nil && len(its) == 1 && its[0].WorktreeState == trace.WorktreeRemoved
			}, out)

			row, err := db.Get(runID)
			if err != nil || row.Status != trace.StatusLost {
				t.Fatalf("run %s %q: %v", row.Status, row.Reason, err)
			}
			if open, err := db.OpenLaunches(runID); err != nil || len(open) != 0 {
				t.Fatalf("launches still open: %+v %v", open, err)
			}
			if exists(path) {
				t.Fatal("the worktree is still on disk")
			}
			if list := gitIn(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
				t.Fatalf("stale worktree entries:\n%s", list)
			}
			if want := stage != "intent"; exists(filepath.Join(marks, "helper")) != want {
				t.Fatalf("a helper was started: %v; want %v", !want, want)
			}
			for _, pidFile := range []string{"helper", "writer"} {
				if b, err := os.ReadFile(filepath.Join(marks, pidFile)); err == nil {
					pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
					eventually(t, 10*time.Second, pidFile+" "+strconv.Itoa(pid)+" stopping", func() bool {
						return syscall.Kill(pid, 0) == syscall.ESRCH
					}, out)
				}
			}

			its, _ := db.Iterations(runID)
			it := its[0]
			ran := exists(filepath.Join(marks, "payload-ran"))
			if stage != "writing" {
				if ran {
					t.Fatal("a payload ran without being admitted")
				}
				if it.Patch != "" || it.RecoveryRef != "" {
					t.Fatalf("nothing was written, yet patch %q ref %q", it.Patch, it.RecoveryRef)
				}
				return
			}
			if !ran {
				t.Fatal("the admitted payload never ran")
			}
			patch := readFile(it.Patch)
			for _, want := range []string{"partial.txt", "stray.txt"} {
				if !strings.Contains(patch, want) {
					t.Errorf("result.patch %s lacks %s:\n%s", it.Patch, want, patch)
				}
			}
			if it.RecoveryRef != workspace.RecoveryRef(runID, 0) {
				t.Fatalf("recovery ref %q", it.RecoveryRef)
			}
			if got := gitIn(t, repo, "show", it.RecoveryRef+":partial.txt"); got != "partial\n" {
				t.Fatalf("the kept commit holds %q", got)
			}
		})
	}
}

// TestCrashingWorker is not a test. It is the worker TestCrashWindowsRecoverOnRestart
// kills: it claims the attempt and creates the worktree as a worker does,
// then launches one supervised payload and holds at the stage it was given
// until it is killed.
func TestCrashingWorker(t *testing.T) {
	stage := os.Getenv("LATHE_CRASH_STAGE")
	if stage == "" {
		return
	}
	if err := crashingWorker(stage, os.Getenv("LATHE_CRASH_ATTEMPT"), os.Getenv("LATHE_CRASH_MARKS"), os.Getenv("LATHE_CRASH_HELPER")); err != nil {
		fmt.Fprintln(os.Stderr, "crashing worker:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "crashing worker: the payload finished before it was killed")
	os.Exit(1)
}

func crashingWorker(stage, attemptID, marks, helper string) error {
	dataRoot, err := trace.DataRoot()
	if err != nil {
		return err
	}
	db, err := trace.Open(dataRoot)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	row, err := db.Claim(attemptID, trace.NewClaimToken(), host, os.Getpid(), time.Minute)
	if err != nil {
		return err
	}
	var spec worker.Spec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return err
	}
	path := workspace.WorktreePath(dataRoot, row.ID, row.Iteration)
	if err := db.SetWorktree(row.ID, attemptID, path, spec.Base, trace.WorktreeCreating); err != nil {
		return err
	}
	if _, err := workspace.CreateWorktree(dataRoot, spec.Workspace.Path, row.ID, row.Iteration, spec.Base); err != nil {
		return err
	}
	if err := db.SetWorktree(row.ID, attemptID, path, spec.Base, trace.WorktreeActive); err != nil {
		return err
	}

	// The payload commits one file and leaves a descendant writing another,
	// so recovery has both a commit and uncommitted work to keep.
	cmd := exec.Command("sh", "-c", `
echo ran > "$MARKS/payload-ran"
printf 'partial\n' > partial.txt
git add partial.txt && git -c user.name=T -c user.email=t@example.com commit -qm partial
( while :; do echo stray >> stray.txt; sleep 0.05; done ) &
echo $! > "$MARKS/writer"
touch "$MARKS/barrier"
wait
`)
	cmd.Dir = path
	cmd.Env = append(os.Environ(), "MARKS="+marks)
	r := &process.Runner{Bin: helper, Registrar: crashRegistrar{db: db, runID: row.ID, attemptID: attemptID, stage: stage, marks: marks}}
	p, err := r.Start(context.Background(), cmd)
	if err != nil {
		return err
	}
	return p.Wait()
}

// crashRegistrar records launches as a worker does, and holds forever at
// its stage: after the intent, after the helper started but before it is
// registered, or after it is registered but before GO.
type crashRegistrar struct {
	db                             *trace.DB
	runID, attemptID, stage, marks string
}

func (c crashRegistrar) Intent(id string, argv []string) error {
	if err := c.db.LaunchIntent(c.runID, c.attemptID, id, argv); err != nil {
		return err
	}
	c.holdAt("intent")
	return nil
}

func (c crashRegistrar) Register(id string, reg process.Registration) error {
	os.WriteFile(filepath.Join(c.marks, "helper"), []byte(strconv.Itoa(reg.PID)), 0o644)
	c.holdAt("started")
	if err := c.db.LaunchRegistered(id, reg.Host, reg.PID, reg.PGID, reg.Endpoint, reg.Token); err != nil {
		return err
	}
	c.holdAt("registered")
	return nil
}

func (c crashRegistrar) Drained(id string) error { return c.db.LaunchDrainedAt(id) }

func (c crashRegistrar) holdAt(stage string) {
	if c.stage != stage {
		return
	}
	os.WriteFile(filepath.Join(c.marks, "barrier"), nil, 0o644)
	time.Sleep(time.Hour)
}

// Two builds of one repository run at once, each in its own worktree, and
// open two pull requests from two branches, though the brancher proposed the
// same name for both. The checkout they were submitted from, dirty and all,
// is exactly as it was.
func TestSimultaneousBuilds(t *testing.T) {
	bin := buildLathe(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("LATHE_CAPACITY", "2")
	repo := gitRepo(t, "")
	gitIn(t, repo, "config", "user.name", "T")
	gitIn(t, repo, "config", "user.email", "t@example.com")
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "--bare")
	gitIn(t, repo, "remote", "add", "origin", origin)

	os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("mine\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "staged.txt"), []byte("staged\n"), 0o644)
	gitIn(t, repo, "add", "staged.txt")
	os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o644)
	checkout := func() string {
		return gitIn(t, repo, "branch", "--show-current") + gitIn(t, repo, "rev-parse", "HEAD") +
			gitIn(t, repo, "status", "--porcelain") + gitIn(t, repo, "diff", "--cached") + gitIn(t, repo, "diff")
	}
	before := checkout()

	stub := buildStub(t)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))

	var ids []string
	for range 2 {
		out, code := lathe(t, bin, "build", "--detach", "--repo", repo, "--provider", "stub", "--model", "m", "greet the world")
		if code != 0 {
			t.Fatalf("submission exited %d:\n%s", code, out)
		}
		ids = append(ids, queuedID(out))
	}
	for _, id := range ids {
		if out, code := lathe(t, bin, "wait", id); code != 0 {
			shown, _ := lathe(t, bin, "show", id)
			t.Fatalf("build %s exited %d:\n%s\n%s\n%s", id, code, out, shown, managerLog(home))
		}
	}

	if exists(filepath.Join(stub, "serial")) {
		t.Fatal("the builders did not run at the same time")
	}
	dirs := strings.Fields(readFile(filepath.Join(stub, "builder-dirs")))
	if len(dirs) != 2 || dirs[0] == dirs[1] {
		t.Fatalf("builders ran in %v", dirs)
	}
	for _, id := range ids {
		if !strings.Contains(dirs[0]+dirs[1], filepath.Join("worktrees", id)) {
			t.Fatalf("no builder ran in %s's worktree: %v", id, dirs)
		}
	}
	heads := strings.Fields(readFile(filepath.Join(stub, "gh-heads")))
	if len(heads) != 2 || heads[0] == heads[1] {
		t.Fatalf("pull requests opened from %v", heads)
	}
	for _, head := range heads {
		if !strings.HasPrefix(head, "feat/greet-the-world-") {
			t.Fatalf("branch %q is not the proposal with a run suffix", head)
		}
		if got := gitIn(t, origin, "show", head+":hello.txt"); got != "hello world\n" {
			t.Fatalf("%s on origin holds %q", head, got)
		}
	}
	if got := checkout(); got != before {
		t.Fatalf("the checkout changed:\n%s\nwant:\n%s", got, before)
	}
	if list := gitIn(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("stale worktree entries:\n%s", list)
	}
}

// recoveryStub is buildStub without the builder barrier: each builder turn
// writes a distinct greeting, the tester's first turn reports an environment
// failure when tester-fails exists, and gh fails its first pr create when
// gh-fail-once exists. gh pr view reports the recorded head at origin's head.
func recoveryStub(t *testing.T) string {
	t.Helper()
	d := buildStub(t)
	os.WriteFile(filepath.Join(d, "tester-env"), []byte(agentReply(t, map[string]any{"summary": "discovered",
		"command": "missing-tool", "exit_code": 127, "output": "not found", "failures": []string{}, "coverage": "none",
		"artifacts": []string{}})), 0o644)
	pi := fmt.Sprintf(`#!/bin/sh
d=%q
if [ "$1 $2" = "auth check" ]; then printf '{"provider":"%%s","status":"ready","authType":"api_key"}\n' "$4"; exit 0; fi
a=unknown
for name in planner plan-reviewer brancher builder tester code-review-general code-review-security code-review-slop code-review-adversarial adjudicator committer pr-author; do
  case "$*" in *"You are the $name "*) a=$name; break ;; esac
done
if [ "$a" = builder ]; then
  n=0; [ ! -f "$d/builds" ] || n=$(cat "$d/builds"); echo $((n+1)) > "$d/builds"
  printf 'hello world %%s\n' "$n" > hello.txt
  cat "$d/builder"; exit 0
fi
if [ "$a" = tester ] && [ -f "$d/tester-fails" ] && [ ! -f "$d/tested" ]; then touch "$d/tested"; cat "$d/tester-env"; exit 0; fi
cat "$d/$a"
`, d)
	gh := fmt.Sprintf(`#!/bin/sh
d=%q
head=""; prev=""
for a in "$@"; do [ "$prev" = "--head" ] && head=$a; prev=$a; done
case "$1 $2" in
"pr list") echo '[]' ;;
"pr view") h=$(cat "$d/gh-head"); oid=$(git ls-remote origin "refs/heads/$h" | cut -f1)
  printf '{"url":"%%s","state":"OPEN","headRefName":"%%s","baseRefName":"main","isDraft":false,"headRefOid":"%%s","isCrossRepository":false}\n' "$3" "$h" "$oid" ;;
"pr create") echo "$head" > "$d/gh-head"
  if [ -f "$d/gh-fail-once" ]; then rm "$d/gh-fail-once"; echo "network down" >&2; exit 1; fi
  echo created >> "$d/gh-creates"; echo https://github.com/acme/widget/pull/7 ;;
*) exit 1 ;;
esac
`, d)
	os.WriteFile(filepath.Join(d, "pi"), []byte(pi), 0o755)
	os.WriteFile(filepath.Join(d, "gh"), []byte(gh), 0o755)
	return d
}

// recoveryRepo is a checkout with an origin that has main, and the lathe
// environment for one test.
func recoveryRepo(t *testing.T) (repo, origin, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	repo = gitRepo(t, "")
	gitIn(t, repo, "config", "user.name", "T")
	gitIn(t, repo, "config", "user.email", "t@example.com")
	origin = t.TempDir()
	gitIn(t, origin, "init", "-q", "--bare")
	gitIn(t, repo, "remote", "add", "origin", origin)
	gitIn(t, repo, "push", "-q", "origin", "HEAD:refs/heads/main")
	return repo, origin, home
}

// lathe resume continues a failed implement through the CLI: it waits for
// the iteration it submitted, restores the saved work and succeeds, and the
// failed iteration stays in the history.
func TestResumeCommandRecoversAFailedImplement(t *testing.T) {
	bin := buildLathe(t)
	repo, _, home := recoveryRepo(t)
	stub := recoveryStub(t)
	os.WriteFile(filepath.Join(stub, "tester-fails"), nil, 0o644)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, code := lathe(t, bin, "implement", "--detach", "--repo", repo, "--provider", "stub", "--model", "m", "greet the world")
	if code != 0 {
		t.Fatalf("submission exited %d:\n%s", code, out)
	}
	id := queuedID(out)
	if out, code := lathe(t, bin, "wait", id); code != 1 {
		t.Fatalf("first iteration exited %d:\n%s\n%s", code, out, managerLog(home))
	}
	if out, code := lathe(t, bin, "resume", id); code != 2 || !strings.Contains(out, "guidance") {
		t.Fatalf("resume without guidance exited %d:\n%s", code, out)
	}
	out, code = lathe(t, bin, "resume", "--provider", "stub", "--model", "m", id, "install the tool and keep the greeting")
	if code != 0 || !strings.Contains(out, "iteration 1 queued") {
		shown, _ := lathe(t, bin, "show", id)
		t.Fatalf("resume exited %d:\n%s\n%s\n%s", code, out, shown, managerLog(home))
	}
	shown, _ := lathe(t, bin, "show", "--json", id)
	var report struct {
		Iterations []struct {
			Status, Kind string
			Source       int `json:"source_iteration"`
		} `json:"iterations"`
	}
	if err := json.Unmarshal([]byte(shown), &report); err != nil {
		t.Fatalf("show --json: %v\n%s", err, shown)
	}
	if its := report.Iterations; len(its) != 2 || its[0].Status != "fail" || its[1].Status != "ok" || its[1].Kind != "recovery" || its[1].Source != 0 {
		t.Fatalf("iterations = %+v", report.Iterations)
	}
	if out, code := lathe(t, bin, "resume", id, "again"); code == 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("resuming a succeeded run exited %d:\n%s", code, out)
	}
}

// A build whose pull request creation failed after its push is resumed,
// continues on its branch, opens one pull request, and then takes a
// revision against that pull request.
func TestRecoverPublishRevise(t *testing.T) {
	bin := buildLathe(t)
	repo, origin, home := recoveryRepo(t)
	stub := recoveryStub(t)
	os.WriteFile(filepath.Join(stub, "gh-fail-once"), nil, 0o644)
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, code := lathe(t, bin, "build", "--detach", "--repo", repo, "--provider", "stub", "--model", "m", "greet the world")
	if code != 0 {
		t.Fatalf("submission exited %d:\n%s", code, out)
	}
	id := queuedID(out)
	if out, code := lathe(t, bin, "wait", id); code != 1 {
		t.Fatalf("build exited %d:\n%s\n%s", code, out, managerLog(home))
	}
	branch := strings.TrimSpace(readFile(filepath.Join(stub, "gh-head")))
	first := strings.TrimSpace(gitIn(t, origin, "rev-parse", branch))

	if out, code := lathe(t, bin, "resume", "--provider", "stub", "--model", "m", id, "open the pull request"); code != 0 {
		shown, _ := lathe(t, bin, "show", id)
		t.Fatalf("resume exited %d:\n%s\n%s\n%s", code, out, shown, managerLog(home))
	}
	if n := strings.Count(readFile(filepath.Join(stub, "gh-creates")), "created"); n != 1 {
		t.Fatalf("pull requests created: %d", n)
	}
	if got := strings.TrimSpace(readFile(filepath.Join(stub, "gh-head"))); got != branch {
		t.Fatalf("the recovery opened its pull request from %s, not %s", got, branch)
	}
	second := strings.TrimSpace(gitIn(t, origin, "rev-parse", branch))
	if second == first || strings.TrimSpace(gitIn(t, origin, "rev-parse", second+"^")) != first {
		t.Fatal("the recovery did not continue the published branch")
	}

	if out, code := lathe(t, bin, "revise", "--provider", "stub", "--model", "m", id, "greet again"); code != 0 {
		shown, _ := lathe(t, bin, "show", id)
		t.Fatalf("revise exited %d:\n%s\n%s\n%s", code, out, shown, managerLog(home))
	}
	if third := strings.TrimSpace(gitIn(t, origin, "rev-parse", branch)); strings.TrimSpace(gitIn(t, origin, "rev-parse", third+"^")) != second {
		t.Fatal("the revision did not extend the recovered pull request's branch")
	}
}

// buildStub is a directory holding a pi that answers every build agent and
// holds each builder until two are building, and a gh that records the
// branch each pull request is opened from.
func buildStub(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	replies := map[string]map[string]any{
		"planner": {"summary": "greet", "steps": []string{"edit hello.txt"}, "files": []string{"hello.txt"},
			"risks": []string{}, "artifacts": []string{}},
		"plan-reviewer": {"summary": "reviewed", "feedback": []string{}, "blocking": []string{}, "artifacts": []string{}},
		"brancher":      {"summary": "as the history does", "branch": "feat/greet-the-world", "artifacts": []string{}},
		"builder":       {"summary": "updated greeting", "changed": []string{"hello.txt"}, "needed": []string{}, "artifacts": []string{"hello.txt"}},
		"tester": {"summary": "discovered", "command": "true", "exit_code": 0, "output": "",
			"failures": []string{}, "coverage": "unchanged", "artifacts": []string{}},
		"adjudicator": {"summary": "adjudicated", "verdict": "accept", "decisions": []map[string]string{}, "changes": "",
			"scope": []map[string]string{}, "amendments": []map[string]string{}, "verification": []string{}, "artifacts": []string{}},
		"committer": {"summary": "conventional", "message": "feat: greet the world", "artifacts": []string{}},
		"pr-author": {"summary": "filled in", "title": "feat: greet the world", "body": "Greets the world.\n", "artifacts": []string{}},
	}
	for _, name := range []string{"code-review-general", "code-review-security", "code-review-slop", "code-review-adversarial"} {
		replies[name] = map[string]any{"summary": "reviewed", "findings": []string{}, "artifacts": []string{}}
	}
	for name, report := range replies {
		os.WriteFile(filepath.Join(d, name), []byte(agentReply(t, report)), 0o644)
	}
	pi := fmt.Sprintf(`#!/bin/sh
d=%q
if [ "$1 $2" = "auth check" ]; then printf '{"provider":"%%s","status":"ready","authType":"api_key"}\n' "$4"; exit 0; fi
a=unknown
for name in planner plan-reviewer brancher builder tester code-review-general code-review-security code-review-slop code-review-adversarial adjudicator committer pr-author; do
  case "$*" in *"You are the $name "*) a=$name; break ;; esac
done
if [ "$a" = builder ]; then
  pwd -P >> "$d/builder-dirs"
  printf 'hello world\n' > hello.txt
  touch "$d/in.$$"; i=0
  while [ "$(ls "$d" | grep -c '^in\.')" -lt 2 ] && [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done
  [ $i -lt 600 ] || touch "$d/serial"
fi
cat "$d/$a"
`, d)
	gh := fmt.Sprintf(`#!/bin/sh
d=%q
if [ "$1 $2" = "pr view" ]; then
  printf '{"url":"%%s","state":"OPEN","headRefName":"%%s","baseRefName":"main"}\n' "$3" "$(git branch --show-current)"
  exit 0
fi
git branch --show-current >> "$d/gh-heads"
echo "https://github.com/acme/widget/pull/$$"
`, d)
	os.WriteFile(filepath.Join(d, "pi"), []byte(pi), 0o755)
	os.WriteFile(filepath.Join(d, "gh"), []byte(gh), 0o755)
	return d
}

// agentReply is one agent answer as a Pi stream carries it: a fenced json
// block in a single assistant message.
func agentReply(t *testing.T, report map[string]any) string {
	t.Helper()
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{
		"role": "assistant", "stopReason": "stop",
		"usage":   map[string]any{"totalTokens": 10, "cost": map[string]any{"total": 0.001}},
		"content": []map[string]string{{"type": "text", "text": "```json\n" + string(b) + "\n```"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// buildLathe builds this binary for tests that run it as separate processes,
// and stops every process started from it when the test ends.
func buildLathe(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("spawns lathe processes")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the binary with")
	}
	bin := filepath.Join(t.TempDir(), "lathe")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building lathe: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("pkill", "-f", bin+" ").Run()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if exec.Command("pgrep", "-f", bin+" ").Run() != nil {
				return
			}
		}
		t.Errorf("lathe processes still running after cleanup")
	})
	return bin
}

// lathe runs bin with this process's environment and returns its combined
// output and exit code.
func lathe(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !asExit(err, &exit) {
		t.Fatalf("lathe %v: %v\n%s", args, err, out)
	}
	if exit != nil {
		return string(out), exit.ExitCode()
	}
	return string(out), 0
}

// startManager starts a manager that runs until stopped or the test ends,
// and returns a function reading its output so far and one stopping it.
func startManager(t *testing.T, bin string) (func() string, func()) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "manager.out")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "manager")
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cmd.Process.Signal(syscall.SIGTERM)
			cmd.Wait()
			f.Close()
		})
	}
	t.Cleanup(stop)
	return func() string { return readFile(log) }, stop
}

// eventually polls cond until it holds, failing with what and diag after
// limit.
func eventually(t *testing.T, limit time.Duration, what string, cond func() bool, diag func() string) {
	t.Helper()
	for deadline := time.Now().Add(limit); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s\n%s", what, diag())
		}
	}
}

// queuedID is the run ID from a submission's "<workflow> <id> queued" line.
func queuedID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[2] == "queued" {
			return f[1]
		}
	}
	return ""
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}
