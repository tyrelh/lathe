package worker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workspace"
)

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
	write(t, filepath.Join(dir, "hello.txt"), "hello\n")
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "initial")
	return dir
}

// queue records a run the way the CLI does and dispatches it the way the
// manager does, returning what the worker is launched with.
func queue(t *testing.T, dataRoot, dir, workflow string) (db *trace.DB, runID, attemptID string) {
	t.Helper()
	cfg, err := config.Load(os.DirFS(filepath.Join("..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string][]string{"scout": {"scout"}, "build": {"planner", "builder", "tester"}}[workflow]
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
	spec, err := Spec{Version: SpecVersion, Workflow: workflow, Request: "what is here",
		Workspace: ws, Roster: roster}.Marshal()
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
		code                 int
	}{
		{
			name: "branch changed", workflow: "scout", want: "was submitted against main", code: 1,
			change: func(t *testing.T, dir string) { git(t, dir, "checkout", "-q", "-b", "other") },
		},
		{
			name: "build went dirty", workflow: "build", want: "uncommitted changes", code: 1,
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
				(tc.name == "build went dirty" && string(b) != "mine\n") {
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
	if _, err := db.RequestCancel(runID); err != nil {
		t.Fatal(err)
	}
	if code := Execute(dataRoot, runID, attemptID, io.Discard); code != 130 {
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
	Heartbeat, dbGrace = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { Heartbeat, dbGrace = 5*time.Second, 15*time.Second })

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

// The worker environment file is the worker's whole environment story: it is
// not the submitting shell's, and it is read when the worker starts.
func TestLoadEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.env")
	write(t, path, "# a comment\n\nexport MOONSHOT_API_KEY=\"secret\"\nGOFLAGS=-count=1\n")
	t.Setenv("MOONSHOT_API_KEY", "")
	if err := loadEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("MOONSHOT_API_KEY"); got != "secret" {
		t.Fatalf("MOONSHOT_API_KEY = %q", got)
	}
	if got := os.Getenv("GOFLAGS"); got != "-count=1" {
		t.Fatalf("GOFLAGS = %q", got)
	}
	if err := loadEnv(filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("a missing environment file was accepted")
	}
	if err := loadEnv(""); err != nil {
		t.Fatalf("an unset LATHE_WORKER_ENV is allowed: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "bad.env")
	write(t, bad, "NOT_AN_ASSIGNMENT\n")
	if err := loadEnv(bad); err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Fatalf("a malformed line: %v", err)
	}
}
