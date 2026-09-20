package run

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/permit"
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
	if n := scalar[int](t, db, `SELECT count(*) FROM events WHERE run_id = ? AND type = 'input'`, r.ID); n != 2 {
		t.Fatalf("input turns = %d; want original and correction", n)
	}
	for _, field := range []string{"system", "prompt", "session_id"} {
		if value := scalar[string](t, db, `SELECT json_extract(payload, '$.`+field+`') FROM events WHERE run_id = ? AND type = 'input' LIMIT 1`, r.ID); value == "" {
			t.Errorf("input event missing %s", field)
		}
	}
	if prompt := scalar[string](t, db, `SELECT json_extract(payload, '$.prompt') FROM events WHERE run_id = ? AND type = 'input' ORDER BY event_id DESC LIMIT 1`, r.ID); !strings.Contains(prompt, "no fenced") {
		t.Errorf("correction input omitted gate failure: %q", prompt)
	}
	// The dashboard reads a phase's model off its usage events, so the spend and
	// the model that produced it have to travel together. Not pinned to a model
	// name: the roster is free to change, the pairing is not.
	if m := scalar[string](t, db,
		`SELECT json_extract(payload, '$.model') FROM events
		 WHERE run_id = ? AND type = 'usage' LIMIT 1`, r.ID); m == "" {
		t.Fatal("a usage event recorded no model; the phases table cannot name one")
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

// TestBudgetEndsTheTurnAndAsksForTheReport is the loop that cost ten minutes in
// a real run: an agent that keeps calling tools and never writes its report.
// The budget ends the turn and re-prompts the same session, so the phase
// produces an answer rather than dying on the deadline.
func TestBudgetEndsTheTurnAndAsksForTheReport(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	count := filepath.Join(dir, "count")
	if err := os.WriteFile(count, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reply.jsonl"), []byte(reply(t, envelope("the tui registry lives in scripts.go"))), 0o644); err != nil {
		t.Fatal(err)
	}
	// The first turn spirals: more tool calls than the budget, then a sleep
	// long enough that only the cancel can end it.
	bin := filepath.Join(dir, "fake-pi")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" >> %q
n=$(cat %q)
echo $((n+1)) > %q
if [ "$n" = "0" ]; then
  i=0
  while [ $i -lt %d ]; do
    printf '{"type":"tool_execution_end","toolName":"grep","toolCallId":"c%%s"}\n' $i
    i=$((i+1))
  done
  sleep 30
else
  cat %q
fi
`, argsFile, count, count, toolBudget+5, filepath.Join(dir, "reply.jsonl"))
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newRun(t, bin)
	var out scoutOutput
	if err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"},
		func(ph *Handle) error { return ph.Call(&out, "how do I extend the tui") }); err != nil {
		t.Fatalf("the phase should have produced a report: %v", err)
	}
	r.Finish(true, "")

	if out.Summary == nil || *out.Summary == "" {
		t.Fatal("no summary: the second turn's envelope was not accepted")
	}

	// The second turn has to ask for the report, in the same session.
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "Do not call any more tools") {
		t.Error("the second turn did not carry the budget prompt")
	}
	if n := strings.Count(string(args), "--session-id"); n != 2 {
		t.Errorf("want 2 turns against the same session, got %d", n)
	}

	db := readDB(t)
	if n := scalar[int](t, db, `SELECT count(*) FROM events WHERE type='log' AND name='budget_spent'`); n != 1 {
		t.Errorf("want one budget_spent event, got %d", n)
	}
	if s := scalar[string](t, db, `SELECT status FROM phases LIMIT 1`); s != "success" {
		t.Errorf("phase status = %q, want success", s)
	}
}

// gitRepo is a committed repo to point a run at. The stub agent is a shell
// script and writes files directly: the guard extension is not in the loop
// here, which is the point — this is the post-turn half proving it catches
// what the pre-execution half would have blocked.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// stubWriter is a fake pi that writes the named files before replying, so a
// turn leaves real dirt behind for Enforce to find.
func stubWriter(t *testing.T, reply string, writes ...string) string {
	t.Helper()
	dir := t.TempDir()
	body := filepath.Join(dir, "reply.jsonl")
	if err := os.WriteFile(body, []byte(reply), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n"
	for _, w := range writes {
		script += fmt.Sprintf("mkdir -p \"$(dirname %q)\" && printf 'the agent wrote this\\n' > %q\n", w, w)
	}
	script += fmt.Sprintf("cat %q\n", body)
	bin := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestEnforceRevertsWhatTheGuardWouldHaveBlocked(t *testing.T) {
	repo := gitRepo(t)
	r := newRun(t, stubWriter(t, reply(t, envelope("wrote both files")), "in-scope.go", "escaped.go"))
	r.Repo = repo
	if err := r.EnsureClean(); err != nil {
		t.Fatal(err)
	}

	var out scoutOutput
	err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"}, func(h *Handle) error {
		h.Scope([]string{"in-scope.go"})
		return h.Call(&out, "write two files")
	})
	if err == nil {
		t.Fatal("a write outside the scope has to fail the phase; the guard letting it through is a bug, not a warning")
	}
	if !strings.Contains(err.Error(), "escaped.go") {
		t.Fatalf("the failure has to name the path: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repo, "in-scope.go")); err != nil {
		t.Fatal("the in-scope file was reverted too:", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "escaped.go")); !os.IsNotExist(err) {
		t.Fatal("the out-of-scope file survived")
	}
	// Nothing else moved: an over-eager Enforce is worse than none.
	if err := permit.Clean(repo); err == nil || !strings.Contains(err.Error(), "in-scope.go") {
		t.Fatalf("the tree should hold exactly the in-scope file: %v", err)
	}

	db := readDB(t)
	payload := scalar[string](t, db,
		`SELECT payload FROM events WHERE run_id = ? AND type = 'permit'`, r.ID)
	if !strings.Contains(payload, "escaped.go") || !strings.Contains(payload, "in-scope.go") {
		t.Fatalf("the permit event should carry both sides: %s", payload)
	}
	if code := r.Finish(true, ""); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
}

// Without MarkClean, lathe never proved the tree was its to revert, so it does
// not. A scout on a repo you are mid-edit in must not touch your work.
func TestEnforceStaysOutOfATreeCleanDidNotPass(t *testing.T) {
	repo := gitRepo(t)
	r := newRun(t, stubWriter(t, reply(t, envelope("wrote a file")), "escaped.go"))
	r.Repo = repo // and no EnsureClean

	var out scoutOutput
	if err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"},
		func(h *Handle) error { return h.Call(&out, "write a file") }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "escaped.go")); err != nil {
		t.Fatal("Enforce reverted in a run that never proved the tree was clean:", err)
	}
	db := readDB(t)
	if n := scalar[int](t, db,
		`SELECT count(*) FROM events WHERE run_id = ? AND type = 'permit'`, r.ID); n != 0 {
		t.Fatalf("permit events = %d; want 0", n)
	}
	r.Finish(true, "")
}

// Every spawn goes out behind the guard, including a read-only one: the
// --no-extensions is what stops the repo under investigation loading its own
// extension into the agent reading it.
func TestEveryAgentRunsBehindTheGuard(t *testing.T) {
	bin, argsFile := stubPi(t, reply(t, envelope("nothing here")))
	r := newRun(t, bin)

	var out scoutOutput
	if err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"},
		func(h *Handle) error { return h.Call(&out, "what is here") }); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	guard := filepath.Join(r.Dir, permit.GuardFile)
	if want := "--no-extensions\n-e\n" + guard + "\n"; !strings.Contains(string(args), want) {
		t.Fatalf("args missing %q:\n%s", want, args)
	}
	// An empty allow list, and the roster's deny lists rather than the caller's.
	scope, err := os.ReadFile(filepath.Join(r.Dir, permit.ScopeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(scope), `"allow":[]`) || !strings.Contains(string(scope), `.git/`) ||
		!strings.Contains(string(scope), `sudo`) {
		t.Fatalf("scope file = %s", scope)
	}
	r.Finish(true, "")
}

// Enforce is handed the whole dirty tree, not a per-turn diff, so a later
// phase's narrower scope must not be read as a verdict on what an earlier one
// was allowed to write. This is the plan -> build -> test shape: the tester has
// no allow list at all and must still not revert the builder.
func TestALaterPhaseKeepsAnEarlierPhasesWork(t *testing.T) {
	repo := gitRepo(t)
	r := newRun(t, stubWriter(t, reply(t, envelope("built it")), "built.go"))
	r.Repo = repo
	if err := r.EnsureClean(); err != nil {
		t.Fatal(err)
	}

	var out scoutOutput
	if err := r.Phase(Params{Name: "build", Kind: "agent", Owner: "scout"}, func(h *Handle) error {
		h.Scope([]string{"built.go"})
		return h.Call(&out, "build it")
	}); err != nil {
		t.Fatal(err)
	}

	// A second phase with an empty scope, which is what a tester has. Its own
	// leavings are still out of scope and still fail it here — Phase 4 is where
	// test dirt stops being fatal — but the failure has to be about them and
	// nothing else.
	r.PiBin = stubWriter(t, reply(t, envelope("ran the suite")), "coverage.out")
	err := r.Phase(Params{Name: "test", Kind: "agent", Owner: "scout"},
		func(h *Handle) error { return h.Call(&out, "run the tests") })
	if err == nil || strings.Contains(err.Error(), "built.go") {
		t.Fatalf("the later phase blamed the earlier one's work: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repo, "built.go")); err != nil {
		t.Fatal("the builder's work was reverted by the phase after it:", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "coverage.out")); !os.IsNotExist(err) {
		t.Fatal("the tester's leavings survived; only what a phase was allowed carries forward")
	}
	r.Finish(true, "")
}

// multiResponse is one attempt that answers across several model responses: a
// streaming partial update, a response that ended in a tool call, the tool's
// own result message, and finally the reply carrying the envelope.
func multiResponse(t *testing.T, text string) string {
	t.Helper()
	line := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	usage := func(tokens int, cost float64) map[string]any {
		return map[string]any{"totalTokens": tokens, "cost": map[string]any{"total": cost}}
	}
	msg := func(role, stop string, u map[string]any, body string) string {
		m := map[string]any{"role": role, "usage": u,
			"content": []map[string]string{{"type": "text", "text": body}}}
		if stop != "" {
			m["stopReason"] = stop
		}
		return line(map[string]any{"type": "message_end", "message": m})
	}
	return line(map[string]any{"type": "message_update", "usage": usage(9, 0.9)}) +
		msg("assistant", "toolUse", usage(60, 0.006), "let me read that") +
		line(map[string]any{"type": "tool_execution_start", "toolCallId": "t1", "toolName": "read", "args": map[string]string{"path": "main.go"}}) +
		line(map[string]any{"type": "tool_execution_end", "toolCallId": "t1", "toolName": "read"}) +
		msg("toolResult", "", usage(500, 0.5), "file contents") +
		msg("assistant", "stop", usage(40, 0.004), text)
}

// Spend lands per completed model response, while the attempt is still
// running: each assistant response that reports usage is charged once, tool
// results and streaming partials are not, and the attempt's own end adds
// nothing on top.
func TestSpendIsRecordedPerResponse(t *testing.T) {
	bin, _ := stubPi(t, multiResponse(t, envelope("main.go dispatches subcommands")))
	r := newRun(t, bin)

	var out scoutOutput
	if err := r.Phase(Params{Name: "scout", Kind: "agent", Owner: "scout"},
		func(h *Handle) error { return h.Call(&out, "what is here") }); err != nil {
		t.Fatal(err)
	}
	if code := r.Finish(true, ""); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}

	db := readDB(t)
	// Two responses, not one per attempt and not one per message.
	if n := scalar[int](t, db, `SELECT count(*) FROM usage WHERE run_id = ?`, r.ID); n != 2 {
		t.Fatalf("usage records = %d; want 2 — partial updates and tool results must not count", n)
	}
	if seqs := scalar[string](t, db,
		`SELECT group_concat(response_seq) FROM (SELECT response_seq FROM usage WHERE run_id = ? ORDER BY response_seq)`,
		r.ID); seqs != "1,2" {
		t.Fatalf("response sequence = %q; want 1,2", seqs)
	}
	// The run total is the sum of the responses, and RunFinish did not add the
	// attempt's total again on top of it.
	tokens := scalar[int](t, db, `SELECT tokens FROM runs WHERE run_id = ?`, r.ID)
	cost := scalar[float64](t, db, `SELECT cost FROM runs WHERE run_id = ?`, r.ID)
	if tokens != 100 || cost < 0.0099 || cost > 0.0101 {
		t.Fatalf("run total = %d tok $%v; want 100 $0.01", tokens, cost)
	}
	if tokens != r.Tokens() {
		t.Fatalf("stored %d tokens, the stream totalled %d", tokens, r.Tokens())
	}
	// The provider and the model come from the options actually passed to Pi.
	if p := scalar[string](t, db, `SELECT provider FROM usage WHERE run_id = ? LIMIT 1`, r.ID); p == "" {
		t.Error("usage recorded no provider")
	}
	if m := scalar[string](t, db, `SELECT model FROM usage WHERE run_id = ? LIMIT 1`, r.ID); m == "" {
		t.Error("usage recorded no model")
	}
	// The first response was charged while the attempt was still running: its
	// event sits before the tool call that came after it in the stream.
	first := scalar[int64](t, db, `SELECT min(event_id) FROM events WHERE run_id = ? AND type = 'usage'`, r.ID)
	tool := scalar[int64](t, db, `SELECT min(event_id) FROM events WHERE run_id = ? AND type = 'tool_call'`, r.ID)
	if !(first < tool) {
		t.Fatalf("usage event %d is not before the tool call %d it preceded in the stream", first, tool)
	}
	// A later lathe opening the same database backfills it again; a run whose
	// spend was recorded response by response has nothing left to explain and
	// nothing to re-charge.
	root, err := trace.DataRoot()
	if err != nil {
		t.Fatal(err)
	}
	next, err := trace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	if d := scalar[float64](t, db, `SELECT difference FROM usage_reconcile WHERE run_id = ?`, r.ID); d != 0 {
		t.Fatalf("a v0.2 run reconciled to a difference of $%v", d)
	}
	if n := scalar[int](t, db, `SELECT count(*) FROM usage WHERE run_id = ?`, r.ID); n != 2 {
		t.Fatalf("the second backfill duplicated usage records: %d", n)
	}
	if again := scalar[int](t, db, `SELECT tokens FROM runs WHERE run_id = ?`, r.ID); again != tokens {
		t.Fatalf("the second backfill changed the run total: %d then %d", tokens, again)
	}
}
