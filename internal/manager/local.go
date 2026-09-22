package manager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// Local launches workers as processes on this machine. Everything about a
// local process handle stays inside this file: the scheduler only ever sees
// the opaque string Launch returns.
type Local struct {
	// Bin overrides the executable to launch. Empty resolves this binary,
	// which is what makes a worker independent of how lathe was invoked.
	Bin string
}

// Launch starts a worker in its own session, with its output redirected to
// the attempt log. The worker inherits the manager's whole environment —
// credentials, PATH, test settings — which is the environment of whatever
// started the manager: a shell running `lathe manager`, or the first
// submission if it was auto-started. Changing it means restarting the manager.
func (l Local) Launch(j Job) (string, error) {
	bin := l.Bin
	if bin == "" {
		var err error
		if bin, err = os.Executable(); err != nil {
			return "", err
		}
	}
	log, err := os.OpenFile(j.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer log.Close()

	cmd := exec.Command(bin, "worker", "--data", j.DataRoot, "--run", j.RunID, "--attempt", j.AttemptID)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	// Its own session: the worker keeps running when the manager stops, and a
	// Ctrl-C in the manager's terminal does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	pid := cmd.Process.Pid
	// Nothing waits on the child: the database is how its outcome comes back,
	// and reaping it would tie the manager's lifetime to the worker's.
	go func() { _ = cmd.Process.Release() }()
	return strconv.Itoa(pid), nil
}

// Alive asks the OS whether that process is still there. Signal 0 checks for
// existence without delivering anything; EPERM means it exists and is not
// ours, which still counts as running.
func (l Local) Alive(handle string) (bool, error) {
	pid, err := strconv.Atoi(handle)
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("unusable worker handle %q", handle)
	}
	switch err := syscall.Kill(pid, 0); err {
	case nil, syscall.EPERM:
		return true, nil
	case syscall.ESRCH:
		return false, nil
	default:
		return false, err
	}
}

// Start makes sure a manager is running, starting a detached one if the data
// directory lock is free. A submitter calls it because a blocking submission
// would otherwise wait on a queue nobody drains. The started manager lives
// until it is killed; it does not exit when idle, which would only add a
// shutdown race against the next submission.
func Start(dataRoot string) error {
	lock, err := Lock(dataRoot)
	if err != nil {
		return nil // somebody holds it: a manager is already running
	}
	// Released before the spawn, so the new manager can take it. Two
	// submitters racing here start two managers, and the loser exits
	// immediately with "a manager is already running" in its log.
	lock.Release()

	bin, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(logPath(dataRoot), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()

	cmd := exec.Command(bin, "manager")
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func logPath(dataRoot string) string { return filepath.Join(dataRoot, LogFile) }

var _ Launcher = Local{}
