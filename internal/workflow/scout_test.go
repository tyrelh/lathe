package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/agentconfig"
	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/pitest"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
)

type workflowHarness struct{ dataRoot, piBin string }

var harnesses sync.Map

func harness(t *testing.T) *workflowHarness {
	t.Helper()
	if state, ok := harnesses.Load(t); ok {
		return state.(*workflowHarness)
	}
	state := &workflowHarness{}
	actual, loaded := harnesses.LoadOrStore(t, state)
	if !loaded {
		t.Cleanup(func() { harnesses.Delete(t) })
	}
	return actual.(*workflowHarness)
}

func dataRoot(t *testing.T) string {
	t.Helper()
	root := harness(t).dataRoot
	if root == "" {
		t.Fatal("test did not load a data root")
	}
	return root
}

func piBin(t *testing.T) string {
	t.Helper()
	bin := harness(t).piBin
	if bin == "" {
		t.Fatal("test did not install pi")
	}
	return bin
}

// stubPi serves replies[n] on its nth invocation.
func stubPi(t *testing.T, replies ...string) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	write(t, count, "0\n")
	for i, r := range replies {
		write(t, filepath.Join(dir, fmt.Sprintf("reply%d.jsonl", i)), r)
	}
	installPi(t, dir, fmt.Sprintf("#!/bin/sh\nn=$(cat %q)\necho $((n+1)) > %q\ncat %q/reply$n.jsonl\n", count, count, dir))
}

// onPath installs pi and prepends its directory to PATH.
func onPath(t *testing.T, dir, script string) {
	t.Helper()
	installPi(t, dir, script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installPi(t *testing.T, dir, script string) {
	t.Helper()
	failed := filepath.Join(dir, "auth-fail")
	auth := fmt.Sprintf(`if [ "$1 $2" = "auth check" ]; then
  echo "$4" >> %q
  if [ -f %q ] && [ "$4" = "$(cat %q)" ]; then
    printf '{"provider":"%%s","status":"not_ready","reason":"credentials_not_configured"}\n' "$4"
    exit 1
  fi
  printf '{"provider":"%%s","status":"ready","authType":"api_key"}\n' "$4"
  exit 0
fi
`, filepath.Join(dir, "auth-providers"), failed, failed)
	script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\n"+auth, 1)
	bin := filepath.Join(dir, "pi")
	write(t, bin, script)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	harness(t).piBin = bin
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// reply is one Pi stream carrying a scout's whole answer.
func reply(t *testing.T, artifacts string) string {
	t.Helper()
	return piReply(t, "Had a look.\n\n```json\n"+
		`{"summary": "main.go dispatches subcommands", "findings": ["main.go: a switch on os.Args"], "artifacts": [`+artifacts+`]}`+
		"\n```\n")
}

// piReply wraps an agent's whole answer as the single assistant message a Pi
// stream would carry it in.
func piReply(t *testing.T, text string) string {
	t.Helper()
	return pitest.Message(t, text, 100)
}

// execute takes a request through the whole production path a workflow test
// cares about: submission records it, dispatch and claim hand it to a worker,
// and the graph runs against the claimed run. The attempt directory is left
// as the run directory, because where raw.jsonl sits is the worker's business
// and not this package's.
func execute(t *testing.T, cfg config.Config, name, repo, request string) int {
	t.Helper()
	return executeWith(t, cfg, name, repo, request, context.Background(), io.Discard)
}

func executeWith(t *testing.T, cfg config.Config, name, repo, request string, ctx context.Context, out io.Writer) int {
	t.Helper()
	code, _ := dispatch(t, cfg, name, repo, request, ctx, out, nil)
	return code
}

func dispatch(t *testing.T, cfg config.Config, name, repo, request string, ctx context.Context, out io.Writer, rec *run.Recovery) (int, string) {
	t.Helper()
	roster, err := cfg.Capture(Agents[name], config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	root := dataRoot(t)
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.Submit(trace.Request{Workflow: name, Repo: repo, Request: request, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	attempt := trace.NewAttemptID(id)
	if err := db.Reserve(id, attempt, ""); err != nil {
		t.Fatal(err)
	}
	token := trace.NewClaimToken()
	if _, err := db.Claim(attempt, token, "host", os.Getpid(), time.Minute); err != nil {
		t.Fatal(err)
	}
	r, err := run.Open(run.Options{
		ID: id, Workflow: name, Request: request, Repo: repo,
		Dir: filepath.Join(root, "runs", id), Snapshot: roster, DB: db,
		Out: out, Ctx: ctx, Attempt: attempt, Recovery: rec, PiBin: piBin(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		r.Retain(rec.Restored)
	}
	code := Graphs[name](r)
	if err := db.Complete(id, attempt, token, r.Status(), r.Reason()); err != nil {
		t.Fatal(err)
	}
	return code, r.Dir
}

func load(t *testing.T) (config.Config, string) {
	t.Helper()
	harness(t).dataRoot = filepath.Join(t.TempDir(), "lathe")
	seed, err := fs.Sub(os.DirFS(filepath.Join("..", "..")), "seed")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := agentconfig.Open(t.TempDir(), seed, "test")
	if err != nil {
		t.Fatal(err)
	}
	// A test that wants routing sets its own tiers.
	cfg.Tiers = nil
	return cfg, t.TempDir()
}

// A good scout run exits 0 and leaves raw.jsonl and result.json behind.
func TestScoutWritesResult(t *testing.T) {
	t.Parallel()
	stubPi(t, reply(t, ""))
	cfg, repo := load(t)

	if code := execute(t, cfg, "scout", repo, "what is here"); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}

	dir := latestRunDir(t)
	if fi, err := os.Stat(filepath.Join(dir, "raw.jsonl")); err != nil || fi.Size() == 0 {
		t.Fatalf("raw.jsonl: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got ScoutOutput
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Summary == nil || *got.Summary != "main.go dispatches subcommands" {
		t.Fatalf("result.json did not round-trip the envelope: %s", b)
	}
}

// A gate the agent never satisfies fails the phase, and the run exits 1.
func TestScoutFailsOnGateViolation(t *testing.T) {
	t.Parallel()
	stubPi(t, reply(t, `"invented.md"`), reply(t, `"invented.md"`), reply(t, `"invented.md"`))
	cfg, repo := load(t)

	if code := execute(t, cfg, "scout", repo, "what is here"); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "result.json")); err == nil {
		t.Fatal("result.json was written for a failed run")
	}
	if row := latestRun(t); row.Status != trace.StatusFail || !strings.Contains(row.Reason, "invented.md") {
		t.Fatalf("run = %s %q; want fail naming the gate violation", row.Status, row.Reason)
	}
}

func latestRunDir(t *testing.T) string {
	t.Helper()
	runs := filepath.Join(dataRoot(t), "runs")
	entries, err := os.ReadDir(runs)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want exactly one run directory under %s: %v", runs, err)
	}
	return filepath.Join(runs, entries[0].Name())
}

func TestNamesMatchGraphs(t *testing.T) {
	if len(Names) != len(Graphs) {
		t.Errorf("Names = %v; Graphs has %d", Names, len(Graphs))
	}
	for _, name := range Names {
		if Graphs[name] == nil {
			t.Errorf("Names lists %q, which is not a graph", name)
		}
	}
}
