package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
)

// stubPi puts a fake `pi` on PATH that serves replies[n] on its nth
// invocation. Scout resolves the binary the way production does, so this is
// the only seam a workflow test needs — no provider, no key, no network.
func stubPi(t *testing.T, replies ...string) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	write(t, count, "0\n")
	for i, r := range replies {
		write(t, filepath.Join(dir, fmt.Sprintf("reply%d.jsonl", i)), r)
	}
	onPath(t, dir, fmt.Sprintf("#!/bin/sh\nn=$(cat %q)\necho $((n+1)) > %q\ncat %q/reply$n.jsonl\n", count, count, dir))
}

// onPath installs script as an executable `pi` in dir and puts dir first on
// PATH, which is the whole seam a workflow test needs. Every stub differs only
// in the script, so only the script belongs in one.
func onPath(t *testing.T, dir, script string) {
	t.Helper()
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
	b, err := json.Marshal(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role": "assistant", "stopReason": "stop",
			"usage":   map[string]any{"totalTokens": 100, "cost": map[string]any{"total": 0.001}},
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
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
	roster, err := cfg.Capture(Agents[name], config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe")
	db, err := trace.Open(dataRoot)
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
		Dir: filepath.Join(dataRoot, "runs", id), Snapshot: roster, DB: db,
		Out: out, Ctx: ctx, Attempt: attempt,
	})
	if err != nil {
		t.Fatal(err)
	}
	code := Graphs[name](r)
	if err := db.Complete(id, attempt, token, r.Status(), r.Reason()); err != nil {
		t.Fatal(err)
	}
	return code
}

func load(t *testing.T) (config.Config, string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg, err := config.Load(os.DirFS(filepath.Join("..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, t.TempDir()
}

// Phase 5's done-when: a good run exits 0 and leaves both files behind.
func TestScoutWritesResult(t *testing.T) {
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
	stubPi(t, reply(t, `"invented.md"`), reply(t, `"invented.md"`), reply(t, `"invented.md"`))
	cfg, repo := load(t)

	if code := execute(t, cfg, "scout", repo, "what is here"); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "result.json")); err == nil {
		t.Fatal("result.json was written for a failed run")
	}
}

func latestRunDir(t *testing.T) string {
	t.Helper()
	runs := filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe", "runs")
	entries, err := os.ReadDir(runs)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want exactly one run directory under %s: %v", runs, err)
	}
	return filepath.Join(runs, entries[0].Name())
}
