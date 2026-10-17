package worker

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

	"github.com/tyrelh/lathe/internal/agentconfig"
	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/process"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workflow"
	"github.com/tyrelh/lathe/internal/workspace"
)

// The test binary is the supervisor helper writing runs launch through, as
// the lathe binary is in production.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == process.HelperArg {
		os.Exit(process.Helper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// scoutReply is one Pi stream carrying a complete scout report.
const scoutReply = `{"type":"message_end","message":{"role":"assistant","stopReason":"stop",` +
	`"usage":{"totalTokens":10,"cost":{"total":0.001}},"content":[{"type":"text","text":` +
	`"Looked.\n\n` + "```" + `json\n{\"summary\":\"main.go dispatches\",\"findings\":[],\"artifacts\":[]}\n` +
	"```" + `"}]}}`

// fakePi puts a stub agent on PATH. Production resolves pi the same way, so
// this is the only seam a worker test needs.
func fakePi(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat " + filepath.Join(dir, "reply.jsonl") + "\n"
	write(t, filepath.Join(dir, "reply.jsonl"), scoutReply+"\n")
	write(t, filepath.Join(dir, "pi"), script)
	if err := os.Chmod(filepath.Join(dir, "pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// repo is a committed checkout on a known branch.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	// These checkout tests do not exercise signing; isolate fixture commits
	// from host GPG settings and resources, including later commits.
	git(t, dir, "config", "--local", "commit.gpgsign", "false")
	write(t, filepath.Join(dir, "hello.txt"), "hello\n")
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "initial")
	return dir
}

// seeded loads the shipped seed from a fresh home, without routing tiers.
func seeded(t *testing.T) config.Config {
	t.Helper()
	seed, err := fs.Sub(os.DirFS(filepath.Join("..", "..")), "seed")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := agentconfig.Open(t.TempDir(), seed, "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tiers = nil
	return cfg
}

// queue records a run the way the CLI does and dispatches it the way the
// manager does, returning what the worker is launched with.
func queue(t *testing.T, dataRoot, dir, workflow string) (db *trace.DB, runID, attemptID string) {
	t.Helper()
	cfg := seeded(t)
	agents := map[string][]string{"scout": {"scout"}, "build": {"planner", "plan-reviewer", "builder", "tester"}}[workflow]
	roster, err := cfg.Capture(agents, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.Local(dir)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := workspace.Branch(ws.Path)
	if err != nil {
		t.Fatal(err)
	}
	var base string
	if writes(workflow) {
		base = strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	}
	spec, err := Spec{Version: SpecVersion, Workflow: workflow, Request: "what is here",
		Workspace: ws, Roster: roster, Base: base}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if db, err = trace.Open(dataRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if runID, err = db.Submit(trace.Request{Workflow: workflow, Repo: ws.Path,
		Request: "what is here", Branch: branch, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	attemptID = trace.NewAttemptID(runID)
	if err := db.Reserve(runID, attemptID, ""); err != nil {
		t.Fatal(err)
	}
	return db, runID, attemptID
}

// A worker claims its attempt, runs the workflow, records the commit it read
// and settles the run — and releases the checkout on the way out.
func TestWorkerExecutesAndSettles(t *testing.T) {
	fakePi(t)
	dir, dataRoot := repo(t), t.TempDir()
	db, runID, attemptID := queue(t, dataRoot, dir, "scout")

	if code := Execute(dataRoot, runID, attemptID, io.Discard); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}
	row, err := db.Get(runID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != trace.StatusOK {
		t.Fatalf("run = %+v", row)
	}
	if row.Commit != strings.TrimSpace(git(t, dir, "rev-parse", "HEAD")) {
		t.Fatalf("recorded commit %q", row.Commit)
	}
	if held, _ := db.HeldCheckouts(); len(held) != 0 {
		t.Fatalf("the worker kept its reservation: %v", held)
	}
	// Reports stay in the run directory; the raw stream is per attempt.
	if _, err := os.Stat(filepath.Join(dataRoot, "runs", runID, "result.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(AttemptDir(dataRoot, runID, attemptID), "raw.jsonl")); err != nil {
		t.Fatal(err)
	}
}

// A duplicate launch loses the claim and does no work at all.
func TestDuplicateLaunchLosesTheClaim(t *testing.T) {
	fakePi(t)
	dir, dataRoot := repo(t), t.TempDir()
	db, runID, attemptID := queue(t, dataRoot, dir, "scout")
	if code := Execute(dataRoot, runID, attemptID, io.Discard); code != 0 {
		t.Fatal(code)
	}
	if code := Execute(dataRoot, runID, attemptID, io.Discard); code != 1 {
		t.Fatalf("the second worker exited %d; want 1", code)
	}
	row, _ := db.Get(runID)
	if row.Status != trace.StatusOK {
		t.Fatalf("the duplicate overwrote the outcome: %+v", row)
	}
}

// The checkout checks run again when execution starts, because a person had
// the directory the whole time the run was queued.
func TestExecutionTimeCheckoutChecks(t *testing.T) {
	for _, tc := range []struct {
		name, workflow, want string
		change               func(t *testing.T, dir string)
		keepsCheckout        bool
		code                 int
	}{
		{
			name: "branch changed", workflow: "scout", want: "was submitted against main", code: 1,
			change: func(t *testing.T, dir string) { git(t, dir, "checkout", "-q", "-b", "other") },
		},
		{
			// The build runs in its own worktree, so the person's change is
			// neither refused nor touched. The stub agent cannot plan, so the
			// run itself fails.
			name: "build went dirty", workflow: "build", want: "", code: 1, keepsCheckout: true,
			change: func(t *testing.T, dir string) { write(t, filepath.Join(dir, "hello.txt"), "mine\n") },
		},
		{
			name: "newer commit on the branch", workflow: "scout", code: 0,
			change: func(t *testing.T, dir string) {
				write(t, filepath.Join(dir, "later.txt"), "later\n")
				git(t, dir, "add", ".")
				git(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "later")
			},
		},
		{
			name: "scout reads uncommitted files", workflow: "scout", code: 0,
			change: func(t *testing.T, dir string) { write(t, filepath.Join(dir, "hello.txt"), "mine\n") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakePi(t)
			dir, dataRoot := repo(t), t.TempDir()
			db, runID, attemptID := queue(t, dataRoot, dir, tc.workflow)
			tc.change(t, dir)

			if code := Execute(dataRoot, runID, attemptID, io.Discard); code != tc.code {
				t.Fatalf("exit code = %d; want %d", code, tc.code)
			}
			row, _ := db.Get(runID)
			if tc.code == 0 {
				if row.Status != trace.StatusOK {
					t.Fatalf("run = %+v", row)
				}
				return
			}
			if row.Status != trace.StatusFail || !strings.Contains(row.Reason, tc.want) {
				t.Fatalf("run = %+v; want a failure mentioning %q", row, tc.want)
			}
			// Whatever the person had in the checkout is still there.
			if b, err := os.ReadFile(filepath.Join(dir, "hello.txt")); err != nil ||
				(tc.keepsCheckout && string(b) != "mine\n") {
				t.Fatalf("the worker touched the checkout: %q %v", b, err)
			}
		})
	}
}

// A cancellation recorded before the worker finishes wins over the workflow's
// own result, and the reports it produced are kept.
func TestCancellationRecordsCancelled(t *testing.T) {
	fakePi(t)
	dir, dataRoot := repo(t), t.TempDir()
	db, runID, attemptID := queue(t, dataRoot, dir, "scout")
	// Finish prints after saving the report, but before the worker settles the
	// run. Cancelling here tests that ordering without racing the heartbeat.
	out := writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "lathe scout ") {
			if _, err := db.RequestCancel(runID); err != nil {
				t.Errorf("request cancellation: %v", err)
			}
		}
		return len(p), nil
	})
	if code := Execute(dataRoot, runID, attemptID, out); code != 130 {
		t.Fatalf("exit code = %d; want 130", code)
	}
	row, _ := db.Get(runID)
	if row.Status != trace.StatusCancelled {
		t.Fatalf("run = %+v", row)
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "runs", runID, "result.json")); err != nil {
		t.Fatalf("cancellation discarded the report it had already written: %v", err)
	}
}

// writes stands in for workflow.Writes in fixtures.
func writes(name string) bool { return workflow.Writes[name] }

// A writing run executes in its own worktree at the commit it was submitted
// at, whatever the checkout does meanwhile, and removes the worktree when it
// stops: the checkout's branch, index and files are as the person left them,
// and git lists no worktree but the checkout.
func TestWritingRunUsesCapturedCommitAndCleansUp(t *testing.T) {
	fakePi(t)
	dir, dataRoot := repo(t), t.TempDir()
	submitted := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	db, runID, attemptID := queue(t, dataRoot, dir, "build")

	git(t, dir, "checkout", "-q", "-b", "elsewhere")
	write(t, filepath.Join(dir, "later.txt"), "later\n")
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "later")
	write(t, filepath.Join(dir, "hello.txt"), "mine\n")
	git(t, dir, "add", "hello.txt")
	beforeHead := git(t, dir, "rev-parse", "HEAD")
	beforeStatus := git(t, dir, "status", "--porcelain")

	Execute(dataRoot, runID, attemptID, io.Discard)

	its, err := db.Iterations(runID)
	if err != nil || len(its) != 1 {
		t.Fatalf("iterations: %v %v", its, err)
	}
	it := its[0]
	if it.Base != submitted {
		t.Fatalf("base = %s; want the submitted commit %s", it.Base, submitted)
	}
	if it.Worktree != workspace.WorktreePath(dataRoot, runID, 0) || it.WorktreeState != trace.WorktreeRemoved {
		t.Fatalf("worktree %q state %q (%s)", it.Worktree, it.WorktreeState, it.CleanupError)
	}
	if _, err := os.Stat(it.Worktree); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still on disk: %v", err)
	}
	if list := git(t, dir, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("stale worktree entries:\n%s", list)
	}
	if got := strings.TrimSpace(git(t, dir, "branch", "--show-current")); got != "elsewhere" {
		t.Fatalf("the checkout moved to %q", got)
	}
	if git(t, dir, "rev-parse", "HEAD") != beforeHead || git(t, dir, "status", "--porcelain") != beforeStatus {
		t.Fatal("the checkout's HEAD, index or files changed")
	}
	if held, _ := db.HeldCheckouts(); len(held) != 0 {
		t.Fatalf("the worker kept its reservation: %v", held)
	}
}

// Every Pi a writing run starts is a child of the supervisor helper, and
// every launch it recorded has drained by the time the run stops.
func TestWritingRunSupervisesEveryLaunch(t *testing.T) {
	fakePi(t)
	stub := t.TempDir()
	parents := filepath.Join(stub, "parents")
	write(t, filepath.Join(stub, "pi"), "#!/bin/sh\nps -o command= -p $PPID >> "+parents+"\nexec "+
		filepath.Join(strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0], "pi")+" \"$@\"\n")
	if err := os.Chmod(filepath.Join(stub, "pi"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir, dataRoot := repo(t), t.TempDir()
	db, runID, attemptID := queue(t, dataRoot, dir, "build")

	Execute(dataRoot, runID, attemptID, io.Discard)

	b, err := os.ReadFile(parents)
	if err != nil {
		t.Fatalf("pi never ran: %v", err)
	}
	for _, parent := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.Contains(parent, process.HelperArg) {
			t.Fatalf("pi started outside the supervisor, by %q", parent)
		}
	}
	if open, err := db.OpenLaunches(runID); err != nil || len(open) != 0 {
		t.Fatalf("undrained launches: %v %v", open, err)
	}
}

// A writing run queued by the previous lathe is refused after its claim and
// before anything else: no reservation, no worktree, no agent. Its read-only
// counterpart still runs.
func TestQueuedLegacySpecifications(t *testing.T) {
	for _, tc := range []struct {
		workflow string
		code     int
	}{{"build", 1}, {"scout", 0}} {
		t.Run(tc.workflow, func(t *testing.T) {
			fakePi(t)
			dir, dataRoot := repo(t), t.TempDir()
			cfg := seeded(t)
			roster, err := cfg.Capture(map[string][]string{"scout": {"scout"}, "build": {"planner"}}[tc.workflow], config.Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			ws, _ := workspace.Local(dir)
			spec, _ := Spec{Version: legacyVersion, Workflow: tc.workflow, Request: "r", Workspace: ws, Roster: roster}.Marshal()
			if Mode(spec) != map[string]string{"build": trace.ModeReject, "scout": trace.ModeCheckout}[tc.workflow] {
				t.Fatalf("mode = %s", Mode(spec))
			}
			db, err := trace.Open(dataRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			runID, err := db.Submit(trace.Request{Workflow: tc.workflow, Repo: ws.Path, Request: "r", Branch: "main", Spec: spec})
			if err != nil {
				t.Fatal(err)
			}
			attemptID := trace.NewAttemptID(runID)
			if err := db.ReserveMode(runID, attemptID, "", Mode(spec)); err != nil {
				t.Fatal(err)
			}
			if code := Execute(dataRoot, runID, attemptID, io.Discard); code != tc.code {
				t.Fatalf("exit code = %d; want %d", code, tc.code)
			}
			if tc.code == 0 {
				return
			}
			row, _ := db.Get(runID)
			if row.Status != trace.StatusFail || !strings.Contains(row.Reason, "submit it again") {
				t.Fatalf("run = %+v", row)
			}
			if _, err := os.Stat(workspace.WorktreeRoot(dataRoot)); !os.IsNotExist(err) {
				t.Fatal("a refused run created a worktree")
			}
			if _, err := os.Stat(filepath.Join(AttemptDir(dataRoot, runID, attemptID), "raw.jsonl")); !os.IsNotExist(err) {
				t.Fatal("a refused run started an agent")
			}
		})
	}
}

// A worker that died between creating its worktree and activating it leaves
// a worktree recovery can settle: with the base recorded alongside ownership,
// changes made during setup are exported before removal; without one, the
// worktree is kept and cleanup stays pending rather than deleting it.
func TestSettleAfterCrashDuringSetup(t *testing.T) {
	dir, dataRoot := repo(t), t.TempDir()
	base := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	path, err := workspace.CreateWorktree(dataRoot, dir, "r1", 0, base)
	if err != nil {
		t.Fatal(err)
	}
	// A checkout hook, say, wrote during setup.
	write(t, filepath.Join(path, "setup.txt"), "from setup\n")

	w := trace.Worktree{RunID: "r1", Iteration: 0, Repo: dir, Path: path}
	if err := Settle(db, dataRoot, w, true); err == nil {
		t.Fatal("a worktree without a recorded base was settled")
	}
	if _, err := os.Stat(filepath.Join(path, "setup.txt")); err != nil {
		t.Fatal("the unexportable worktree was removed")
	}

	w.Base = base
	if err := Settle(db, dataRoot, w, true); err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(ReportDir(dataRoot, "r1", 0), "result.patch"))
	if err != nil || !strings.Contains(string(patch), "from setup") {
		t.Fatalf("patch = %q, %v", patch, err)
	}
	if fi, _ := os.Stat(filepath.Join(ReportDir(dataRoot, "r1", 0), "result.patch")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("patch mode = %v; want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the settled worktree is still on disk")
	}
	if list := git(t, dir, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("stale entries:\n%s", list)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A run recorded by an incompatible lathe fails with a reason rather than
// being executed against a specification nothing understands.
func TestUnknownSpecVersionFails(t *testing.T) {
	dataRoot := t.TempDir()
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spec, _ := json.Marshal(Spec{Version: SpecVersion + 1, Workflow: "scout"})
	runID, err := db.Submit(trace.Request{Workflow: "scout", Repo: "/repo", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := trace.NewAttemptID(runID)
	if err := db.Reserve(runID, attemptID, ""); err != nil {
		t.Fatal(err)
	}
	if code := Execute(dataRoot, runID, attemptID, io.Discard); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if row, _ := db.Get(runID); !strings.Contains(row.Reason, "incompatible lathe") {
		t.Fatalf("run = %+v", row)
	}
}

// A database the worker cannot reach is retried for a bounded time and then
// stops execution, well inside the lease it was granted, so partial work is
// kept rather than killed by a lease expiring under it.
func TestDatabaseOutageStopsExecution(t *testing.T) {
	oldBeat, oldGrace := Heartbeat, dbGrace
	t.Cleanup(func() { Heartbeat, dbGrace = oldBeat, oldGrace })
	Heartbeat, dbGrace = 5*time.Millisecond, 20*time.Millisecond

	dir, dataRoot := repo(t), t.TempDir()
	db, _, attemptID := queue(t, dataRoot, dir, "scout")
	token := trace.NewClaimToken()
	if _, err := db.Claim(attemptID, token, "host", os.Getpid(), Lease); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := beat(ctx, db, attemptID, token, stop, io.Discard)
	db.Close() // the outage

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the heartbeat never gave up on an unreachable database")
	}
	if ctx.Err() == nil {
		t.Fatal("execution was not stopped")
	}
}
