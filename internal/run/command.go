package run

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// CommandFailure is a measured red suite, recoverable by a later fix phase.
// Infrastructure, timeout and enforcement errors remain terminal.
type CommandFailure struct{ ExitCode int }

func (e *CommandFailure) Error() string { return fmt.Sprintf("test command exited %d", e.ExitCode) }

// tailBytes is how much of a suite's output the fix round is shown.
const tailBytes = 4096

// tailWriter bounds memory even when a test suite emits arbitrarily much output.
type tailWriter struct{ data []byte }

// Trimming in place rather than reslicing keeps one backing array for the life
// of the command: a chatty suite writes line by line, and append's growth on a
// resliced buffer would reallocate and copy the whole tail on every line.
func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > tailBytes {
		p = p[len(p)-tailBytes:]
	}
	if extra := len(w.data) + len(p) - tailBytes; extra > 0 {
		w.data = w.data[:copy(w.data, w.data[extra:])]
	}
	w.data = append(w.data, p...)
	return n, nil
}

// Command runs the discovered suite behind the same coarse shell deny list as
// the tester. The timeout covers descendants too, so cleanup cannot race tests.
func (h *Handle) Command(command string) (string, error) {
	r := h.run
	if !r.clean {
		return "", fmt.Errorf("verify requires EnsureClean")
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("empty test command")
	}
	if err := h.Log("command", command); err != nil {
		return "", err
	}
	for _, rule := range r.cfg.BashDenied {
		re, err := regexp.Compile(rule)
		if err != nil {
			return "", fmt.Errorf("invalid bash deny rule %q: %w", rule, err)
		}
		if re.MatchString(command) {
			return "", fmt.Errorf("the command matches the deny rule %s and will not be run", rule)
		}
	}
	agent, err := r.cfg.Resolve("tester", r.ov)
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	cancel := func() {}
	if agent.Deadline > 0 {
		ctx, cancel = context.WithTimeout(ctx, agent.Deadline)
	}
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = r.Repo
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var output tailWriter
	cmd.Stdout, cmd.Stderr = &output, &output
	r.commandMu.Lock()
	runErr := cmd.Start()
	if runErr == nil {
		r.commandPID = cmd.Process.Pid
	}
	r.commandMu.Unlock()
	if runErr == nil {
		runErr = cmd.Wait()
		r.commandMu.Lock()
		// Stop any surviving children before enforcement inspects the tree.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		r.commandPID = 0
		r.commandMu.Unlock()
	}
	tail := string(output.data)
	logErr := h.Log("output", tail)
	reverted, enforceErr := r.enforce(h)
	if enforceErr != nil {
		return tail, enforceErr
	}
	if len(reverted) > 0 {
		return tail, fmt.Errorf("command wrote outside its scope; reverted %s", strings.Join(reverted, ", "))
	}
	if logErr != nil {
		return tail, logErr
	}
	if ctx.Err() != nil {
		return tail, fmt.Errorf("test command: %w", ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) && exit.ExitCode() >= 0 {
		return tail, &CommandFailure{ExitCode: exit.ExitCode()}
	}
	return tail, runErr
}
