package pi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/trace"
)

func openSample(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// The captured run: 8 message_end events, 4 tool calls, and totals that only
// come out right if per-message usage is summed rather than read as cumulative.
func TestScanSample(t *testing.T) {
	var raw bytes.Buffer
	var tools []Event
	res, err := Scan(openSample(t, "pi-sample.jsonl"), &raw, func(ev Event) {
		if ev.Type == "tool_execution_end" {
			tools = append(tools, ev)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 356 || res.Malformed != 0 {
		t.Fatalf("events = %d, malformed = %d; want 356, 0", res.Events, res.Malformed)
	}
	if res.Tokens != 1094+1188+2925 {
		t.Fatalf("tokens = %d; want %d (the sum, not the last value)", res.Tokens, 1094+1188+2925)
	}
	if want := 0.00080573 + 0.00054861 + 0.00268676; res.Cost < want-1e-9 || res.Cost > want+1e-9 {
		t.Fatalf("cost = %v; want %v", res.Cost, want)
	}
	if !strings.Contains(res.Text, "Go files") {
		t.Fatalf("final text = %q", res.Text)
	}
	if len(tools) != 4 {
		t.Fatalf("tool ends = %d; want 4", len(tools))
	}
	// Args live on the start event; pairing on toolCallId is what puts them here.
	for _, ev := range tools {
		if len(ev.Args) == 0 {
			t.Fatalf("%s: no args backfilled from its start event", ev.ToolCallID)
		}
	}
	// The raw record is the whole stream, byte for byte.
	want, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw.Bytes(), want) {
		t.Fatalf("raw record = %d bytes; want %d", raw.Len(), len(want))
	}
}

// A failed tool comes back in-band, and the run carries on.
func TestScanToolError(t *testing.T) {
	var failed int
	if _, err := Scan(openSample(t, "pi-sample-toolerror.jsonl"), nil, func(ev Event) {
		if ev.Type == "tool_execution_end" && ev.IsError {
			failed++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("failed tool calls = %d; want 1", failed)
	}
}

// One tool result bigger than Scanner's 64KB default, delivered down a real
// pipe: the line must survive whole, not end the stream with ErrTooLong.
func TestRunHugeToolResult(t *testing.T) {
	dir := t.TempDir()
	big, err := json.Marshal(map[string]any{
		"type": "tool_execution_end", "toolCallId": "read_0_big", "toolName": "read",
		"result": map[string]any{"content": strings.Repeat("x", 1<<20)}, "isError": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	line := filepath.Join(dir, "big.jsonl")
	if err := os.WriteFile(line, append(big, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat "+line+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var got int
	res, err := Run(context.Background(), Options{Bin: stub, Dir: dir}, "read it",
		func(ev Event) { got = len(ev.Result) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 || got < 1<<20 {
		t.Fatalf("events = %d, result = %d bytes; want 1 and > 1MB", res.Events, got)
	}
}

// Phase 3's done-when, minus the second terminal: the sample's tool calls
// arrive in the global DB as tool_call rows while the stream is being read.
func TestScanStreamsToolCallsToDB(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "data")
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const runID = "20260916T120000Z_scout"
	if err := db.RunStart(runID, "scout", "/Users/tyrel/Projects/lathe", "what is here"); err != nil {
		t.Fatal(err)
	}
	ph := trace.NewPhase(runID, 1, "scout", "agent", "scout")
	if err := db.PhaseUpsert(ph); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(openSample(t, "pi-sample.jsonl"), nil, func(ev Event) {
		if ev.Type != "tool_execution_end" {
			return
		}
		if err := db.Event(runID, ph.ID, "tool_call", ev.ToolName,
			map[string]any{"id": ev.ToolCallID, "args": ev.Args, "isError": ev.IsError}); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	ph.Finish("success", "")
	if err := db.PhaseUpsert(ph); err != nil {
		t.Fatal(err)
	}
	if err := db.RunFinish(runID, "ok", res.Tokens, res.Cost); err != nil {
		t.Fatal(err)
	}

	ro, err := sql.Open("sqlite", "file:"+filepath.Join(dataRoot, "runs.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var names string
	if err := ro.QueryRow(
		`SELECT group_concat(name) FROM (SELECT name FROM events WHERE type='tool_call' ORDER BY event_id)`,
	).Scan(&names); err != nil {
		t.Fatal(err)
	}
	if names != "find,read,read,read" {
		t.Fatalf("tool_call rows = %q; want find,read,read,read", names)
	}
}

// Run's own logic is the command line it builds; a stub on the Bin path lets
// that be checked without a provider, a key, or the network.
func TestRunBuildsCommandAndStreams(t *testing.T) {
	dir := t.TempDir()
	sample, err := filepath.Abs(filepath.Join("..", "..", "testdata", "pi-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "fake-pi")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\npwd >> %q\ncat %q\n",
		filepath.Join(dir, "args"), filepath.Join(dir, "args"), sample)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var pid int
	res, err := Run(context.Background(), Options{
		Bin: stub, Dir: dir, Provider: "moonshotai", Model: "kimi-k2.7-code",
		Tools: []string{"read", "grep"}, OnStart: func(p int) { pid = p },
	}, "-not-a-flag", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pid == 0 {
		t.Fatal("OnStart never saw a pid")
	}
	if res.Events != 356 {
		t.Fatalf("events = %d; want 356", res.Events)
	}

	got, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-p\n--mode\njson\n", "--provider\nmoonshotai\n", "--tools\nread,grep\n", "--\n-not-a-flag\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("args missing %q:\n%s", want, got)
		}
	}
	// cmd.Dir is how a global binary acts on the repo you invoked it from.
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	ranIn, _ := filepath.EvalSymlinks(lines[len(lines)-1])
	wantDir, _ := filepath.EvalSymlinks(dir)
	if ranIn != wantDir {
		t.Fatalf("pi ran in the wrong directory:\n%s", got)
	}
}

// Hand-run: LATHE_PI_LIVE=1 go test ./internal/pi -run Live -v spends real
// tokens against the real provider and writes to the real data root, so a
// second terminal can watch rows arrive with sqlite3.
func TestLiveScout(t *testing.T) {
	if os.Getenv("LATHE_PI_LIVE") == "" {
		t.Skip("set LATHE_PI_LIVE=1 to spawn the real agent")
	}
	root, err := trace.DataRoot()
	if err != nil {
		t.Fatal(err)
	}
	db, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	runID := "live_" + time.Now().UTC().Format("20060102T150405Z")
	if err := db.RunStart(runID, "scout", repo, "live phase 3 check"); err != nil {
		t.Fatal(err)
	}
	ph := trace.NewPhase(runID, 1, "scout", "agent", "scout")
	if err := db.PhaseUpsert(ph); err != nil {
		t.Fatal(err)
	}

	runDir := filepath.Join(root, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rawFile, err := os.Create(filepath.Join(runDir, "raw.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rawFile.Close()

	res, err := Run(context.Background(), Options{
		Dir: repo, Provider: "moonshotai", Model: "kimi-k2.7-code",
		Tools: []string{"read", "grep", "find", "ls"},
		Raw:   rawFile, SessionID: runID,
		OnStart: func(pid int) { db.Event(runID, ph.ID, "log", "pi_pid", map[string]int{"pid": pid}) },
	}, "Read every .go file under internal/ and list what each package does.", func(ev Event) {
		if ev.Type == "tool_execution_end" {
			db.Event(runID, ph.ID, "tool_call", ev.ToolName,
				map[string]any{"id": ev.ToolCallID, "args": ev.Args, "isError": ev.IsError})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	ph.Finish("success", "")
	if err := db.PhaseUpsert(ph); err != nil {
		t.Fatal(err)
	}
	if err := db.RunFinish(runID, "ok", res.Tokens, res.Cost); err != nil {
		t.Fatal(err)
	}
	t.Logf("run %s: %d events, %d tokens, $%.5f\n%s", runID, res.Events, res.Tokens, res.Cost, res.Text)
	if res.Events == 0 || res.Text == "" {
		t.Fatal("the agent produced nothing")
	}
}
