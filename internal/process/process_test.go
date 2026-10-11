package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the supervisor helper, the way lathe does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		os.Exit(Helper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// recorder is a Registrar that keeps every call, and can refuse registration.
type recorder struct {
	mu      sync.Mutex
	calls   []string
	reg     Registration
	refuse  bool
	onReg   func()
	drained int
}

func (r *recorder) Intent(id string, argv []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "intent")
	return nil
}

func (r *recorder) Register(id string, reg Registration) error {
	if r.onReg != nil {
		r.onReg()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "register")
	r.reg = reg
	if r.refuse {
		return errors.New("refused")
	}
	return nil
}

func (r *recorder) Drained(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "drained")
	r.drained++
	return nil
}

func runner(rec *recorder) *Runner { return &Runner{Bin: os.Args[0], Registrar: rec} }

func sh(script string) *exec.Cmd { return exec.Command("/bin/sh", "-c", script) }

// The payload starts only after registration is stored, and the launch is
// recorded drained after it stops.
func TestAdmissionFollowsRegistration(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	rec := &recorder{}
	rec.onReg = func() {
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(marker); err == nil {
			t.Error("the payload ran before its registration was stored")
		}
	}
	if err := runner(rec).Exec(context.Background(), sh("touch "+marker)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the payload never ran")
	}
	if got := strings.Join(rec.calls, ","); got != "intent,register,drained" {
		t.Fatalf("calls = %s", got)
	}
	if rec.reg.PGID != rec.reg.PID || rec.reg.Endpoint == "" || rec.reg.Token == "" {
		t.Fatalf("registration = %+v", rec.reg)
	}
}

// A refused registration admits nothing.
func TestRefusedRegistrationRunsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	rec := &recorder{refuse: true}
	if err := runner(rec).Exec(context.Background(), sh("touch "+marker)); err == nil {
		t.Fatal("a refused registration was admitted")
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the payload ran without a registration")
	}
	if rec.drained != 1 {
		t.Fatalf("drained %d times", rec.drained)
	}
}

// Exit statuses and both output streams come back as the payload left them.
func TestPayloadStatusAndStreams(t *testing.T) {
	for _, code := range []int{0, 1, 3, 126, 127} {
		var stdout, stderr bytes.Buffer
		cmd := sh("echo out; echo err >&2; exit " + itoa(code))
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := runner(&recorder{}).Exec(context.Background(), cmd)
		if code == 0 {
			if err != nil {
				t.Fatalf("exit 0: %v", err)
			}
		} else {
			var exit *ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != code {
				t.Fatalf("exit %d: %v", code, err)
			}
		}
		if stdout.String() != "out\n" || stderr.String() != "err\n" {
			t.Fatalf("exit %d: stdout %q stderr %q", code, stdout.String(), stderr.String())
		}
	}
}

// Neither control descriptor reaches the payload.
func TestControlDescriptorsAreNotInherited(t *testing.T) {
	var stdout bytes.Buffer
	cmd := sh(`for fd in 3 4; do if { : >&$fd; } 2>/dev/null; then echo "$fd open"; else echo "$fd closed"; fi; done`)
	cmd.Stdout = &stdout
	if err := runner(&recorder{}).Exec(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "3 closed\n4 closed\n" {
		t.Fatalf("payload descriptors: %q", got)
	}
}

// A descendant left behind by the payload is stopped with its group.
func TestLeftoverDescendantsStop(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	if err := runner(&recorder{}).Exec(context.Background(), sh("sleep 30 & echo $! > "+pidFile)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	if exec.Command("kill", "-0", pid).Run() == nil {
		t.Fatalf("descendant %s survived its supervisor", pid)
	}
}

// An authenticated stop request ends the group; a wrong token is refused.
func TestStopRequest(t *testing.T) {
	rec := &recorder{}
	p, err := runner(rec).Start(context.Background(), sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestStop(rec.reg.Endpoint, "wrong"); err == nil {
		t.Fatal("a wrong token was accepted")
	}
	if err := RequestStop(rec.reg.Endpoint, rec.reg.Token); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("Wait = %v; want ErrStopped", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stop request did not stop the payload")
	}
	if live, _ := GroupLive(rec.reg.PGID); live {
		t.Fatal("the group survived")
	}
}

// Cancellation stops the payload and reports the context's error.
func TestCancellationStopsGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recorder{}
	p, err := runner(rec).Start(ctx, sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := p.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v", err)
	}
	// Killed members may linger as zombies until init reaps them; none may
	// still be running.
	if live, _ := GroupLive(rec.reg.PGID); live {
		t.Fatal("the group survived cancellation")
	}
	if rec.drained != 1 {
		t.Fatalf("drained %d times; want 1 once the group stopped", rec.drained)
	}
}

// Closing the parent's control pipe, as a dead worker does, stops the group.
func TestControlEOFStopsGroup(t *testing.T) {
	rec := &recorder{}
	p, err := runner(rec).Start(context.Background(), sh("sleep 30"))
	if err != nil {
		t.Fatal(err)
	}
	p.closeCtl()
	if err := p.Wait(); !errors.Is(err, ErrStopped) {
		t.Fatalf("Wait = %v; want ErrStopped", err)
	}
}

// A group with a running or stopped member counts as live even while it also
// holds an unreaped zombie child.
func TestGroupLiveSeesRunningMembers(t *testing.T) {
	// The child exits at once and its parent never waits for it.
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.1 & exec sleep 3")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	}()
	if live, _ := GroupLive(cmd.Process.Pid); !live {
		t.Fatal("a running group reported stopped")
	}
	time.Sleep(300 * time.Millisecond)
	syscall.Kill(cmd.Process.Pid, syscall.SIGSTOP)
	if live, _ := GroupLive(cmd.Process.Pid); !live {
		t.Fatal("a stopped but present member reported gone")
	}
}

// A group whose only member is an unreaped zombie counts as stopped.
func TestGroupLiveIgnoresZombies(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("ps", "-o", "stat=", "-p", itoa(pid)).Output()
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("member never became a zombie: %q", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if live, err := GroupLive(pid); err != nil || live {
		t.Fatalf("GroupLive = %v, %v; want a zombie-only group stopped", live, err)
	}
}

// A member left running after its supervisor exited is killed while waiting.
func TestWaitGroupGoneKillsStragglers(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if !waitGroupGone(cmd.Process.Pid, 5*time.Second) {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatal("the straggler survived")
	}
	if err := <-done; err == nil {
		t.Fatal("the straggler exited cleanly; want it killed")
	}
}

// Without an installed runner the package helpers run commands directly.
func TestHelpersWithoutRunner(t *testing.T) {
	out, err := Output(sh("echo hi"))
	if err != nil || string(out) != "hi\n" {
		t.Fatalf("Output = %q, %v", out, err)
	}
	restore := Install(runner(&recorder{}))
	defer restore()
	out, err = CombinedOutput(sh("echo a; echo b >&2"))
	if err != nil || string(out) != "a\nb\n" {
		t.Fatalf("CombinedOutput = %q, %v", out, err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
