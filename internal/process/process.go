// Package process launches subprocesses under a supervisor whose admission
// is gated on a durable registration. A launch is three records: an intent
// written before anything starts, a registration written once an inert helper
// is running in its own process group, and a drained record once that group
// has stopped. The helper launches the payload only after it receives GO,
// which the parent sends only after the registration is stored, so an intent
// without a registration proves that no payload ran.
//
// The helper holds a private control pipe from the parent for the whole
// invocation. EOF on it (the parent died or gave up), cancellation, or an
// authenticated stop request on its loopback endpoint makes the helper kill
// its entire process group, payload descendants included.
package process

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// HelperArg is the hidden first argument that turns a lathe binary into the
// supervisor helper.
const HelperArg = "__lathe-supervise"

// Supervisor wire protocol. Each message is one line: a verb, then
// space-separated fields.
const (
	// Parent to helper, on the control pipe.
	msgToken = "TOKEN"
	msgGo    = "GO"

	// Helper to parent, on the status pipe.
	msgReady    = "READY"
	msgExit     = "EXIT"
	msgSignal   = "SIGNAL"
	msgStartErr = "STARTERR"
	msgStopped  = "STOPPED"

	// Stop endpoint request and replies.
	msgStop     = "STOP"
	replyOK     = "OK"
	replyDenied = "DENIED"
)

// Registration is what a running, not yet admitted, helper reports about
// itself: where it runs and how to stop it.
type Registration struct {
	Host     string
	PID      int
	PGID     int
	Endpoint string // loopback address accepting "STOP <token>"
	Token    string
}

// Registrar persists the three launch records. Register must fail when the
// launch may no longer be admitted; the payload then never starts.
type Registrar interface {
	Intent(id string, argv []string) error
	Register(id string, reg Registration) error
	Drained(id string) error
}

// Runner starts supervised launches. Bin is an executable that dispatches
// HelperArg to Helper.
type Runner struct {
	Bin       string
	Registrar Registrar
}

// ExitError is a payload that ran and did not exit 0. Code is its exit
// status, or -1 when a signal ended it.
type ExitError struct {
	Code   int
	Signal syscall.Signal
	Stderr []byte
}

func (e *ExitError) Error() string {
	if e.Code < 0 {
		return "signal: " + e.Signal.String()
	}
	return "exit status " + strconv.Itoa(e.Code)
}

// ExitCode is the payload's exit status, or -1 for a signal.
func (e *ExitError) ExitCode() int { return e.Code }

// ErrStopped is a payload stopped by its supervisor: control-pipe EOF or an
// authenticated stop request.
var ErrStopped = errors.New("stopped by its supervisor")

// Process is one admitted launch.
type Process struct {
	id      string
	reg     Registrar
	helper  *exec.Cmd
	pgid    int
	ctl     *os.File
	closer  sync.Once
	status  chan string
	ctx     context.Context
	stopped chan struct{}
}

// Start launches cmd's program under a supervisor. cmd must be unstarted; its
// Path, Args, Dir, Env, Stdin, Stdout and Stderr are used, and nothing else.
// Cancelling ctx stops the whole process group. It returns once the payload
// has been admitted, or with an error when it was not.
func (r *Runner) Start(ctx context.Context, cmd *exec.Cmd) (*Process, error) {
	if r == nil || r.Registrar == nil || r.Bin == "" {
		return nil, errors.New("process: no supervised runner configured")
	}
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id := rand.Text()
	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	if err := r.Registrar.Intent(id, argv); err != nil {
		return nil, fmt.Errorf("recording the launch intent: %w", err)
	}

	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		r.Registrar.Drained(id)
		return nil, err
	}
	stR, stW, err := os.Pipe()
	if err != nil {
		ctlR.Close()
		ctlW.Close()
		r.Registrar.Drained(id)
		return nil, err
	}
	args := append([]string{HelperArg, "--dir", cmd.Dir, "--", cmd.Path}, cmd.Args[1:]...)
	h := exec.Command(r.Bin, args...)
	h.Env = cmd.Env
	h.Stdin, h.Stdout, h.Stderr = cmd.Stdin, cmd.Stdout, cmd.Stderr
	h.ExtraFiles = []*os.File{ctlR, stW}
	h.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	h.WaitDelay = 2 * time.Second
	err = h.Start()
	// The parent keeps only its own ends; the helper's copies are its own.
	ctlR.Close()
	stW.Close()
	if err != nil {
		ctlW.Close()
		stR.Close()
		r.Registrar.Drained(id)
		return nil, err
	}
	p := &Process{id: id, reg: r.Registrar, helper: h, pgid: h.Process.Pid, ctl: ctlW,
		status: make(chan string, 1), ctx: ctx, stopped: make(chan struct{})}
	go p.watch()

	token := rand.Text()
	st := bufio.NewReader(stR)
	ready, rerr := func() (string, error) {
		if _, err := fmt.Fprintf(ctlW, msgToken+" %s\n", token); err != nil {
			return "", err
		}
		return st.ReadString('\n')
	}()
	reg, perr := parseReady(ready)
	if rerr != nil || perr != nil {
		p.abort(stR)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the supervisor did not become ready: %v", errors.Join(rerr, perr))
	}
	reg.Host, _ = os.Hostname()
	reg.Token = token
	if err := r.Registrar.Register(id, reg); err != nil {
		p.abort(stR)
		return nil, fmt.Errorf("registering the launch: %w", err)
	}
	go func() {
		rest, _ := io.ReadAll(st)
		stR.Close()
		p.status <- string(rest)
	}()
	if _, err := io.WriteString(ctlW, msgGo+"\n"); err != nil {
		p.Wait()
		return nil, fmt.Errorf("admitting the launch: %w", err)
	}
	return p, nil
}

// abort ends a launch that was never admitted: closing the control pipe makes
// the helper exit before it starts anything.
func (p *Process) abort(stR *os.File) {
	p.closeCtl()
	p.helper.Wait()
	stR.Close()
	close(p.stopped)
	waitGroupGone(p.pgid, 5*time.Second)
	p.reg.Drained(p.id)
}

func (p *Process) watch() {
	select {
	case <-p.ctx.Done():
		p.closeCtl()
		syscall.Kill(-p.pgid, syscall.SIGKILL)
	case <-p.stopped:
	}
}

func (p *Process) closeCtl() { p.closer.Do(func() { p.ctl.Close() }) }

// PID is the supervisor's process ID, which is also its process group.
func (p *Process) PID() int { return p.pgid }

// Wait waits for the payload and its whole process group to stop, records the
// launch drained, and returns the payload's outcome: nil for exit 0, an
// *ExitError for any other status, ErrStopped or the context's error when the
// supervisor stopped it.
func (p *Process) Wait() error {
	werr := p.helper.Wait()
	var status string
	select {
	case status = <-p.status:
	case <-time.After(5 * time.Second):
	}
	p.closeCtl()
	close(p.stopped)
	// The supervisor has exited; any member left is a straggler, such as a
	// child forked while the group was being killed.
	gone := waitGroupGone(p.pgid, 10*time.Second)
	var drainErr error
	if gone {
		drainErr = p.reg.Drained(p.id)
	} else {
		drainErr = fmt.Errorf("process group %d is still running after its supervisor stopped", p.pgid)
	}
	err := interpret(status, werr)
	if p.ctx.Err() != nil && err != nil {
		var exit *ExitError
		if !errors.As(err, &exit) || exit.Code < 0 {
			err = p.ctx.Err()
		}
	}
	if drainErr != nil {
		return errors.Join(err, drainErr)
	}
	return err
}

// interpret reads the helper's status lines.
func interpret(status string, werr error) error {
	for _, line := range strings.Split(status, "\n") {
		kind, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch kind {
		case msgExit:
			n, err := strconv.Atoi(rest)
			if err != nil {
				return fmt.Errorf("the supervisor reported an unreadable status %q", line)
			}
			if n == 0 {
				return nil
			}
			return &ExitError{Code: n}
		case msgSignal:
			n, _ := strconv.Atoi(rest)
			return &ExitError{Code: -1, Signal: syscall.Signal(n)}
		case msgStartErr:
			return errors.New(rest)
		case msgStopped:
			return ErrStopped
		}
	}
	return fmt.Errorf("the supervisor ended without reporting its payload's status: %v", werr)
}

func parseReady(line string) (Registration, error) {
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != msgReady {
		return Registration{}, fmt.Errorf("unexpected handshake %q", strings.TrimSpace(line))
	}
	pid, err1 := strconv.Atoi(f[2])
	pgid, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return Registration{}, fmt.Errorf("unexpected handshake %q", strings.TrimSpace(line))
	}
	return Registration{Endpoint: f[1], PID: pid, PGID: pgid}, nil
}

// GroupAlive reports whether any process remains in process group pgid.
func GroupAlive(pgid int) (bool, error) {
	if pgid <= 1 {
		return false, fmt.Errorf("unusable process group %d", pgid)
	}
	return Alive(-pgid)
}

// Alive reports whether pid exists, or for a negative pid whether that
// process group has any member. A process that exists but is not ours
// counts as alive.
func Alive(pid int) (bool, error) {
	switch err := syscall.Kill(pid, 0); err {
	case nil, syscall.EPERM:
		return true, nil
	case syscall.ESRCH:
		return false, nil
	default:
		return false, err
	}
}

// GroupLive reports whether process group pgid has a member that is not a
// zombie. A group of only exited, unreaped processes can write nothing, so it
// counts as stopped. When membership cannot be read, it answers true.
func GroupLive(pgid int) (bool, error) {
	alive, err := GroupAlive(pgid)
	if err != nil || !alive {
		return alive, err
	}
	if live, ok := procGroupLive(pgid); ok {
		return live, nil
	}
	out, err := exec.Command("ps", "-A", "-o", "pgid=,stat=").Output()
	if err != nil {
		return true, nil
	}
	want := strconv.Itoa(pgid)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == want && !strings.HasPrefix(f[1], "Z") {
			return true, nil
		}
	}
	return false, nil
}

// procGroupLive reads /proc for a member of pgid that is not a zombie. ok is
// false where /proc is unavailable.
func procGroupLive(pgid int) (live, ok bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	seen := false
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		seen = true
		// pid (comm) state ppid pgrp ...; comm may hold spaces and parens.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 3 {
			continue
		}
		if g, err := strconv.Atoi(f[2]); err == nil && g == pgid && f[0] != "Z" && f[0] != "X" {
			return true, true
		}
	}
	return false, seen
}

// waitGroupGone kills process group pgid until it has no live member or limit
// passes, and reports whether it stopped.
func waitGroupGone(pgid int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		if live, err := GroupLive(pgid); err == nil && !live {
			return true
		} else if live {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// RequestStop asks the supervisor at endpoint to stop its process group,
// authenticated by token.
func RequestStop(endpoint, token string) error {
	c, err := net.DialTimeout("tcp", endpoint, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c, msgStop+" %s\n", token); err != nil {
		return err
	}
	reply, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(reply) != replyOK {
		return fmt.Errorf("the supervisor at %s refused the stop request", endpoint)
	}
	return nil
}

// Helper is the supervisor's entry point; args follow HelperArg. It reads the
// token, opens its stop endpoint, reports READY and waits for GO. Only then
// does it start the payload, in its own process group, in --dir. It returns
// the exit code for the helper process.
func Helper(args []string) int {
	dir, argv, err := parseHelperArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe supervisor:", err)
		return 125
	}
	// Neither inherited end may reach the payload or its children.
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	ctl := os.NewFile(3, "control")
	st := os.NewFile(4, "status")
	if ctl == nil || st == nil {
		return 125
	}
	rd := bufio.NewReader(ctl)
	line, err := rd.ReadString('\n')
	token, ok := strings.CutPrefix(strings.TrimSpace(line), msgToken+" ")
	if err != nil || !ok || token == "" {
		return 125
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 125
	}
	pgid, err := syscall.Getpgid(0)
	if err != nil {
		return 125
	}
	if _, err := fmt.Fprintf(st, msgReady+" %s %d %d\n", ln.Addr().String(), os.Getpid(), pgid); err != nil {
		return 125
	}
	if line, err = rd.ReadString('\n'); err != nil || strings.TrimSpace(line) != msgGo {
		// Not admitted: nothing was started, so there is nothing to stop.
		return 125
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(st, msgStartErr+" %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		return 127
	}
	stop := make(chan string, 3)
	go func() {
		// The parent writes nothing after GO; EOF means it closed the pipe.
		io.Copy(io.Discard, rd)
		stop <- "EOF"
	}()
	go serve(ln, token, stop)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		fmt.Fprint(st, exitLine(cmd, err))
	case reason := <-stop:
		fmt.Fprintf(st, msgStopped+" %s\n", reason)
	}
	// Leftover descendants go with the group, and so does this helper.
	syscall.Kill(-pgid, syscall.SIGKILL)
	return 0
}

func exitLine(cmd *exec.Cmd, err error) string {
	if ps := cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return fmt.Sprintf(msgSignal+" %d\n", int(ws.Signal()))
		}
		return fmt.Sprintf(msgExit+" %d\n", ps.ExitCode())
	}
	return fmt.Sprintf(msgStartErr+" %s\n", strings.ReplaceAll(fmt.Sprint(err), "\n", " "))
}

func serve(ln net.Listener, token string, stop chan<- string) {
	want := []byte(msgStop + " " + token)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			l, _ := bufio.NewReader(c).ReadString('\n')
			if subtle.ConstantTimeCompare(bytes.TrimSpace([]byte(l)), want) != 1 {
				fmt.Fprintln(c, replyDenied)
				return
			}
			fmt.Fprintln(c, replyOK)
			select {
			case stop <- "REQUEST":
			default:
			}
		}()
	}
}

func parseHelperArgs(args []string) (dir string, argv []string, err error) {
	if len(args) < 4 || args[0] != "--dir" || args[2] != "--" {
		return "", nil, errors.New("usage: " + HelperArg + " --dir <dir> -- <program> [args...]")
	}
	return args[1], args[3:], nil
}

// The installed runner routes this process's workspace commands. A worker
// installs one for a writing run; without one, commands run directly.
var (
	mu        sync.RWMutex
	installed *Runner
)

// Install makes r the runner Output, CombinedOutput and Run use, and returns
// a function restoring the previous one.
func Install(r *Runner) (restore func()) {
	mu.Lock()
	prev := installed
	installed = r
	mu.Unlock()
	return func() {
		mu.Lock()
		installed = prev
		mu.Unlock()
	}
}

// Installed is the runner Install set, or nil.
func Installed() *Runner {
	mu.RLock()
	defer mu.RUnlock()
	return installed
}

// Exec runs cmd to completion through r, or directly when r is nil.
func (r *Runner) Exec(ctx context.Context, cmd *exec.Cmd) error {
	if r == nil {
		return cmd.Run()
	}
	p, err := r.Start(ctx, cmd)
	if err != nil {
		return err
	}
	return p.Wait()
}

// Output is exec.Cmd.Output through the installed runner. A failing command's
// stderr is kept on its *ExitError.
func Output(cmd *exec.Cmd) ([]byte, error) {
	r := Installed()
	if r == nil {
		return cmd.Output()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if cmd.Stderr == nil {
		cmd.Stderr = &stderr
	}
	err := r.Exec(context.Background(), cmd)
	var exit *ExitError
	if errors.As(err, &exit) {
		exit.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), err
}

// CombinedOutput is exec.Cmd.CombinedOutput through the installed runner.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	r := Installed()
	if r == nil {
		return cmd.CombinedOutput()
	}
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := r.Exec(context.Background(), cmd)
	return b.Bytes(), err
}

// Run is exec.Cmd.Run through the installed runner.
func Run(cmd *exec.Cmd) error {
	return Installed().Exec(context.Background(), cmd)
}

// GroupCommand returns an exec.CommandContext command that runs in its own
// process group, which cancellation kills as a whole.
func GroupCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	// The command's children inherit its pipes, so cancellation has to reach
	// the process group: killing the command alone leaves them running,
	// holding the pipes open after the caller has stopped.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A buffered stdout or stderr is copied through a pipe those children
	// inherit. Without a delay, Wait blocks on that copy until the last
	// grandchild exits, which is the case cancellation is trying to escape.
	cmd.WaitDelay = time.Second
	return cmd
}
