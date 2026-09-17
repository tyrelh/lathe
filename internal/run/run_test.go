package run

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"

	_ "modernc.org/sqlite"
)

// scoutOutput mirrors the scout's contract rather than importing it:
// internal/workflow imports this package, so a test inside it cannot import
// workflow back. What package run needs from an envelope is the interface,
// and this is the smallest thing that satisfies it.
type scoutOutput struct {
	Summary  *string   `json:"summary"`
	Findings *[]string `json:"findings"`
	Wrote    *[]string `json:"artifacts"`
}

func (s *scoutOutput) Validate() []string {
	var v []string
	if s.Summary == nil || *s.Summary == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if s.Findings == nil {
		v = append(v, `"findings" is missing; use [] if you found nothing`)
	}
	if s.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	return v
}

func (s *scoutOutput) Artifacts() []string {
	if s.Wrote == nil {
		return nil
	}
	return *s.Wrote
}

// reply builds one Pi stream carrying a single assistant message whose text is
// `text`. It is the smallest thing Scan will total and Call will parse.
func reply(t *testing.T, text string) string {
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

func envelope(summary string) string {
	return "Here is what I found.\n\n```json\n" +
		fmt.Sprintf(`{"summary": %q, "findings": ["main.go dispatches subcommands"], "artifacts": []}`, summary) +
		"\n```\n"
}

// stubPi writes a fake `pi` that serves replies[n] on its nth invocation and
// appends its own argv to an args file, so the command line lathe built and
// the number of turns it took are both checkable without a provider or a key.
func stubPi(t *testing.T, replies ...string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	count := filepath.Join(dir, "count")
	if err := os.WriteFile(count, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, r := range replies {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("reply%d.jsonl", i)), []byte(r), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin = filepath.Join(dir, "fake-pi")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %q\nn=$(cat %q)\necho $((n+1)) > %q\ncat %q/reply$n.jsonl\n",
		argsFile, count, count, dir)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

// newRun points the data root at a temp directory so no test ever writes to
// the real global database, and loads the real roster and real prompts.
func newRun(t *testing.T, bin string) *Run {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg, err := config.Load(os.DirFS(filepath.Join("..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(cfg, config.Overrides{}, "scout", t.TempDir(), "what is here")
	if err != nil {
		t.Fatal(err)
	}
	r.PiBin, r.Out = bin, io.Discard
	return r
}

// readDB opens whichever database the run wrote to, read-only, the way the
// dashboard will. It goes through trace.DataRoot so it follows the same
// XDG_DATA_HOME that newRun redirected.
func readDB(t *testing.T) *sql.DB {
	t.Helper()
	root, err := trace.DataRoot()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, "runs.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func scalar[T any](t *testing.T, db *sql.DB, q string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Phase 4's done-when: a malformed first reply costs exactly one correction
// turn, in the same Pi session, under one phase.
func TestCallCorrectsInTheSameSession(t *testing.T) {
	bin, argsFile := stubPi(t,
		reply(t, "I looked around. No json block here at all."),
		reply(t, envelope("main.go dispatches subcommands")),
	)
	r := newRun(t, bin)

	var out scoutOutput
	err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"},
		func(h *Handle) error { return h.Call(&out, "what is here", ArtifactsExist, FilesNonEmpty) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary == nil || *out.Summary != "main.go dispatches subcommands" {
		t.Fatalf("envelope not decoded: %+v", out)
	}
	if code := r.Finish(true, ""); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}

	db := readDB(t)
	if n := scalar[int](t, db, `SELECT count(*) FROM phases WHERE run_id = ?`, r.ID); n != 1 {
		t.Fatalf("phases = %d; want 1 — a correction is a second turn, not a second phase", n)
	}
	if s := scalar[string](t, db, `SELECT status FROM phases WHERE run_id = ?`, r.ID); s != "success" {
		t.Fatalf("phase status = %q; want success", s)
	}
	// Two agent turns: the rejected one and the corrected one.
	if n := scalar[int](t, db, `SELECT count(*) FROM events WHERE run_id = ? AND type = 'usage'`, r.ID); n != 2 {
		t.Fatalf("agent turns = %d; want 2", n)
	}
	if n := scalar[int](t, db,
		`SELECT count(*) FROM events WHERE run_id = ? AND type = 'envelope' AND payload LIKE '%no fenced%'`,
		r.ID); n != 1 {
		t.Fatal("the rejected envelope was not recorded with its violation")
	}
	// Spend from the rejected turn is real and is counted.
	if r.Tokens() != 200 {
		t.Fatalf("tokens = %d; want 200 (both turns)", r.Tokens())
	}
	if s := scalar[string](t, db, `SELECT status FROM runs WHERE run_id = ?`, r.ID); s != "ok" {
		t.Fatalf("run status = %q; want ok", s)
	}

	// Same session id on both turns is what makes the correction cheap; the
	// system prompt and the tool allowlist come from the roster.
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	phaseID := r.ID + "_01_scout"
	if n := strings.Count(string(args), "--session-id\n"+phaseID+"\n"); n != 2 {
		t.Fatalf("session id %q used %d times; want 2\n%s", phaseID, n, args)
	}
	for _, want := range []string{"--tools\nread,grep,find,ls\n", "You are the scout"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("args missing %q:\n%s", want, args)
		}
	}
	// The raw stream is the authoritative record, and both turns are in it.
	raw, err := os.ReadFile(filepath.Join(r.Dir, "raw.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "message_end"); n != 2 {
		t.Fatalf("raw.jsonl holds %d turns; want 2", n)
	}
}

// An agent that never gets it right fails its phase after maxCorrections, and
// the run says so rather than exiting 0 on a phase that did not work.
func TestCallGivesUpAfterMaxCorrections(t *testing.T) {
	bad := reply(t, "still no json")
	bin, _ := stubPi(t, bad, bad, bad, bad)
	r := newRun(t, bin)

	var out scoutOutput
	err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"},
		func(h *Handle) error { return h.Call(&out, "what is here") })
	if err == nil {
		t.Fatal("a phase whose agent never produced an envelope reported success")
	}
	if code := r.Finish(true, ""); code != 1 {
		t.Fatalf("exit code = %d; want 1 — a failed phase must not exit 0", code)
	}

	db := readDB(t)
	if n := scalar[int](t, db, `SELECT count(*) FROM events WHERE run_id = ? AND type = 'usage'`, r.ID); n != 1+maxCorrections {
		t.Fatalf("agent turns = %d; want %d", n, 1+maxCorrections)
	}
	if s := scalar[string](t, db, `SELECT status FROM runs WHERE run_id = ?`, r.ID); s != "fail" {
		t.Fatalf("run status = %q; want fail", s)
	}
}

// A gate checks the agent's claims, and a false claim is a correction like any
// other — so the second turn's honest envelope passes.
func TestGatesCatchAFalseArtifactClaim(t *testing.T) {
	lie := "```json\n" + `{"summary": "wrote it", "findings": [], "artifacts": ["docs/invented.md"]}` + "\n```\n"
	bin, _ := stubPi(t, reply(t, lie), reply(t, envelope("nothing was written")))
	r := newRun(t, bin)

	var out scoutOutput
	if err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"},
		func(h *Handle) error { return h.Call(&out, "write docs", ArtifactsExist, FilesNonEmpty) }); err != nil {
		t.Fatal(err)
	}
	db := readDB(t)
	if n := scalar[int](t, db,
		`SELECT count(*) FROM events WHERE run_id = ? AND type = 'gate' AND payload LIKE '%invented.md%'`,
		r.ID); n != 1 {
		t.Fatal("the gate did not record the false artifact claim")
	}
	r.Finish(true, "")
}

// An empty file is a claim that passes ArtifactsExist and must not pass
// FilesNonEmpty, which is the whole reason there are two gates.
func TestFilesNonEmpty(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "empty.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Run{Repo: repo}
	out := &scoutOutput{Wrote: &[]string{"empty.md"}}
	if v := ArtifactsExist(out, r); v != nil {
		t.Fatalf("ArtifactsExist = %v; want none, the file is there", v)
	}
	if v := FilesNonEmpty(out, r); len(v) != 1 {
		t.Fatalf("FilesNonEmpty = %v; want one violation", v)
	}
}

// A panic inside a phase still closes its trace honestly, which is the whole
// point of the closure form.
func TestPhaseSurvivesAPanic(t *testing.T) {
	r := newRun(t, "")
	err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"},
		func(h *Handle) error { panic("boom") })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v; want the panic", err)
	}
	if code := r.Finish(true, ""); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if s := scalar[string](t, readDB(t), `SELECT status FROM phases WHERE run_id = ?`, r.ID); s != "fail" {
		t.Fatalf("phase status = %q; want fail", s)
	}
}

// The last json block wins, so an agent that shows an example before its
// answer still gets read correctly.
func TestDecodeTakesTheLastBlock(t *testing.T) {
	text := "For example:\n```json\n{\"summary\": \"an example\"}\n```\nMy answer:\n" + envelope("the real one")
	var out scoutOutput
	if v := decode(text, &out); v != nil {
		t.Fatalf("decode = %v", v)
	}
	if *out.Summary != "the real one" {
		t.Fatalf("summary = %q; want the last block", *out.Summary)
	}
	// A rejected attempt must not leave a field behind for the next one.
	if v := decode("no block here", &out); len(v) != 1 {
		t.Fatalf("decode = %v; want one violation", v)
	}
	if out.Summary != nil {
		t.Fatal("a stale summary survived a failed decode")
	}
}

// Hand-run: LATHE_PI_LIVE=1 go test ./internal/run -run Live -v spends real
// tokens. It settles the one thing the stub cannot — that re-prompting the
// same Pi session id continues the context instead of starting cold, which is
// the assumption the whole correction loop rests on.
func TestLiveCorrection(t *testing.T) {
	if os.Getenv("LATHE_PI_LIVE") == "" {
		t.Skip("set LATHE_PI_LIVE=1 to spawn the real agent")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(os.DirFS(repo))
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(cfg, config.Overrides{}, "scout", repo, "live phase 4 check")
	if err != nil {
		t.Fatal(err)
	}

	var out scoutOutput
	err = r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"}, func(h *Handle) error {
		return h.Call(&out, `Name the Go packages under internal/ and what each one does.
In your report, deliberately omit the "summary" key so the correction loop is exercised.`,
			ArtifactsExist, FilesNonEmpty)
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary == nil || *out.Summary == "" {
		t.Fatal("the corrected envelope still has no summary")
	}

	db := readDB(t)
	turns := scalar[int](t, db, `SELECT count(*) FROM events WHERE run_id = ? AND type = 'usage'`, r.ID)
	t.Logf("run %s: %d agent turns, %d tokens, $%.5f\nsummary: %s",
		r.ID, turns, r.Tokens(), r.Cost(), *out.Summary)
	if turns < 2 {
		t.Log("the agent obeyed the contract on its first try, so the correction path went unexercised")
	}
	r.Finish(true, "")
}
