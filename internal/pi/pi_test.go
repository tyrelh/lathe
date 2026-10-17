package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/process"
)

// The test binary doubles as the supervisor helper, the way lathe does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == process.HelperArg {
		os.Exit(process.Helper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// launches is a Registrar that records each launch record in order, runs
// onReg before storing a registration, and refuses it when refuse is set.
type launches struct {
	mu     sync.Mutex
	calls  []string
	reg    process.Registration
	refuse bool
	onReg  func()
}

func (l *launches) Intent(string, []string) error { return l.add("intent") }
func (l *launches) Drained(string) error          { return l.add("drained") }
func (l *launches) Register(_ string, reg process.Registration) error {
	if l.onReg != nil {
		l.onReg()
	}
	l.mu.Lock()
	l.reg = reg
	l.mu.Unlock()
	if l.refuse {
		return errors.New("registration refused")
	}
	return l.add("register")
}

func (l *launches) add(call string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
	return nil
}

func (l *launches) log() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.calls, ",")
}

func supervisor(l *launches) *process.Runner {
	return &process.Runner{Bin: os.Args[0], Registrar: l}
}

func authStub(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestCheckAuth(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		exit         int
		ok           bool
	}{
		{"ready", `{"provider":"test","status":"ready","authType":"api_key"}`, 0, true},
		{"missing", `{"provider":"test","status":"not_ready","reason":"credentials_not_configured"}`, 1, false},
		{"invalid", `{"provider":"test","status":"invalid","reason":"invalid_state"}`, 2, false},
		{"nonzero ready", `{"provider":"test","status":"ready"}`, 1, false},
		{"zero not ready", `{"provider":"test","status":"not_ready","reason":"credential_not_available"}`, 0, false},
		{"unsupported", "help text SECRET", 0, false},
		{"unknown status", `{"provider":"test","status":"SECRET"}`, 0, false},
		{"unknown reason", `{"provider":"test","status":"not_ready","reason":"SECRET"}`, 1, false},
		{"wrong provider", `{"provider":"SECRET","status":"ready"}`, 0, false},
		{"missing status", `{"provider":"test"}`, 0, false},
		{"trailing output", `{"provider":"test","status":"ready"} SECRET`, 0, false},
		{"extra secret", `{"provider":"test","status":"ready","credentials":"SECRET"}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := authStub(t, fmt.Sprintf("echo 'SECRET' >&2\ncat <<'JSON'\n%s\nJSON\nexit %d\n", tc.output, tc.exit))
			err := CheckAuth(context.Background(), Options{Bin: bin, Provider: "test"})
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v; success = %v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("leaked output: %v", err)
			}
		})
	}
}

func TestCheckAuthInvocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LATHE_AUTH_TEST", "inherited")
	bin := authStub(t, `printf '%s\n' "$@" > args
pwd > cwd
printf '%s' "$LATHE_AUTH_TEST" > env
printf '{"provider":"test","status":"ready","authType":"oauth"}\n'
`)
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := CheckAuth(context.Background(), Options{Dir: dir, Provider: "test"}); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"args": "auth\ncheck\n--provider\ntest\n--json\n", "cwd": real + "\n", "env": "inherited",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if name == "cwd" && err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(strings.TrimSpace(string(got)))
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			got = []byte(resolved + "\n")
		}
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestCheckAuthCancellationKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	bin := authStub(t, "(sleep 0.4; echo late > late) &\nwait\n")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := CheckAuth(ctx, Options{Bin: bin, Dir: dir, Provider: "test"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("cancellation: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "late")); !os.IsNotExist(err) {
		t.Fatalf("descendant survived: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := CheckAuth(ctx, Options{Bin: bin, Provider: "test"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v", err)
	}
}

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
	res, err := Scan(openSample(t, "pi-sample.jsonl"), &raw, func(ev Event) bool {
		if ev.Type == "tool_execution_end" {
			tools = append(tools, ev)
		}
		return true
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
	var names []string
	for _, ev := range tools {
		names = append(names, ev.ToolName)
	}
	if got := strings.Join(names, ","); got != "find,read,read,read" {
		t.Fatalf("tool names = %q", got)
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
	if _, err := Scan(openSample(t, "pi-sample-toolerror.jsonl"), nil, func(ev Event) bool {
		if ev.Type == "tool_execution_end" && ev.IsError {
			failed++
		}
		return true
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
		func(ev Event) bool { got = len(ev.Result); return true })
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 || got < 1<<20 {
		t.Fatalf("events = %d, result = %d bytes; want 1 and > 1MB", res.Events, got)
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
		Tools: []string{"read", "grep"}, Extensions: []string{"/run/guard.ts", "/run/read.ts"},
		OnStart: func(p int) { pid = p },
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
		// The guard first, so it sees a tool call before anything else does.
		"--no-extensions\n-e\n/run/guard.ts\n-e\n/run/read.ts\n--\n",
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
// tokens against the real provider.
func TestLiveScout(t *testing.T) {
	if os.Getenv("LATHE_PI_LIVE") == "" {
		t.Skip("set LATHE_PI_LIVE=1 to spawn the real agent")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	rawFile, err := os.Create(filepath.Join(t.TempDir(), "raw.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rawFile.Close()
	res, err := Run(context.Background(), Options{
		Dir: repo, Provider: "moonshotai", Model: "kimi-k2.7-code",
		Tools: []string{"read", "grep", "find", "ls"}, Raw: rawFile,
	}, "Read every .go file under internal/ and list what each package does.", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d events, %d tokens, $%.5f\n%s", res.Events, res.Tokens, res.Cost, res.Text)
	if res.Events == 0 || res.Text == "" {
		t.Fatal("the agent produced nothing")
	}
}

// A supervised auth check reads the same response and exit status as a direct
// one, and its launch is recorded from intent to drained.
func TestCheckAuthSupervised(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		exit         int
		ok           bool
	}{
		{"ready", `{"provider":"test","status":"ready"}`, 0, true},
		{"missing", `{"provider":"test","status":"not_ready","reason":"credentials_not_configured"}`, 1, false},
		{"nonzero ready", `{"provider":"test","status":"ready"}`, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &launches{}
			bin := authStub(t, fmt.Sprintf("cat <<'JSON'\n%s\nJSON\nexit %d\n", tc.output, tc.exit))
			err := CheckAuth(context.Background(), Options{Bin: bin, Provider: "test", Runner: supervisor(l)})
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v; success = %v", err, tc.ok)
			}
			if got := l.log(); got != "intent,register,drained" {
				t.Fatalf("launch records = %s", got)
			}
		})
	}
}

// A refused registration admits nothing: the check fails without Pi running.
func TestCheckAuthRefusedRegistrationRunsNothing(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	bin := authStub(t, "touch "+marker+"\nprintf '{\"provider\":\"test\",\"status\":\"ready\"}\\n'\n")
	l := &launches{refuse: true}
	if err := CheckAuth(context.Background(), Options{Bin: bin, Provider: "test", Runner: supervisor(l)}); err == nil {
		t.Fatal("a refused launch reported readiness")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pi ran without a stored registration: %v", err)
	}
}

// Supervised, Run streams every event, reports the supervisor's PID, and does
// not start Pi until its registration is stored.
func TestRunSupervisedStreams(t *testing.T) {
	dir := t.TempDir()
	sample, err := filepath.Abs(filepath.Join("..", "..", "testdata", "pi-sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ran")
	stub := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(stub, []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\ncat %q\n", marker, sample)), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &launches{}
	l.onReg = func() {
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(marker); err == nil {
			t.Error("pi ran before its registration was stored")
		}
	}
	var raw bytes.Buffer
	var pid int
	res, err := Run(context.Background(), Options{Bin: stub, Dir: dir, Raw: &raw, Runner: supervisor(l),
		OnStart: func(p int) { pid = p }}, "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 356 {
		t.Fatalf("events = %d; want 356", res.Events)
	}
	want, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw.Bytes(), want) {
		t.Fatalf("raw stream differs from Pi's output: %d bytes, want %d", raw.Len(), len(want))
	}
	if pid == 0 || pid != l.reg.PID {
		t.Fatalf("OnStart pid = %d; want the supervisor's %d", pid, l.reg.PID)
	}
	if got := l.log(); got != "intent,register,drained" {
		t.Fatalf("launch records = %s", got)
	}
}

// Pi's exit status survives supervision, including 126 and 127, and its
// stderr reaches the error.
func TestRunSupervisedExitStatus(t *testing.T) {
	for _, code := range []int{1, 3, 126, 127} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			stub := authStub(t, fmt.Sprintf("echo 'pi broke %d' >&2\nexit %d\n", code, code))
			l := &launches{}
			_, err := Run(context.Background(), Options{Bin: stub, Dir: t.TempDir(), Runner: supervisor(l)}, "go", nil)
			var exit *process.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != code {
				t.Fatalf("error = %v; want exit status %d", err, code)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("pi broke %d", code)) {
				t.Fatalf("stderr missing from %v", err)
			}
		})
	}
}

// A refused registration fails Run before Pi starts.
func TestRunSupervisedRefusedRegistration(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	stub := authStub(t, "touch "+marker+"\n")
	l := &launches{refuse: true}
	if _, err := Run(context.Background(), Options{Bin: stub, Dir: t.TempDir(), Runner: supervisor(l)}, "go", nil); err == nil {
		t.Fatal("a refused launch ran")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pi ran without a stored registration: %v", err)
	}
}

// Cancelling a supervised Run stops Pi's descendants too.
func TestRunSupervisedCancellationKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	stub := authStub(t, "(sleep 0.4; echo late > "+filepath.Join(dir, "late")+") &\nwait\n")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, Options{Bin: stub, Dir: dir, Runner: supervisor(&launches{})}, "go", nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation: %v after %s", err, time.Since(start))
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "late")); !os.IsNotExist(err) {
		t.Fatalf("descendant survived: %v", err)
	}
}
