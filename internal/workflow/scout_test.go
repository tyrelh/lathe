package workflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
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

	if code := Scout(cfg, config.Overrides{}, repo, "what is here"); code != 0 {
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

	if code := Scout(cfg, config.Overrides{}, repo, "what is here"); code != 1 {
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
