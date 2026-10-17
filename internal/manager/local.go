package manager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/tyrelh/lathe/internal/process"
)

// Local launches workers as processes on this machine. Its handle is the
// worker's PID, encoded by localHandle and decoded by localPID.
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
	p, err := spawnDetached(j.LogPath, bin, "worker", "--data", j.DataRoot, "--run", j.RunID, "--attempt", j.AttemptID)
	if err != nil {
		return "", err
	}
	// Nothing waits on the child: the database is how its outcome comes back,
	// and reaping it would tie the manager's lifetime to the worker's.
	go func() { _ = p.Release() }()
	return localHandle(p.Pid), nil
}

// spawnDetached starts bin with args in its own session, with no stdin and
// its output appended to logPath, and returns the unreleased process.
func spawnDetached(logPath, bin string, args ...string) (*os.Process, error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer log.Close()

	cmd := exec.Command(bin, args...)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	// Its own session: the process keeps running when its starter stops, and
	// a Ctrl-C in the starter's terminal does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// Alive asks the OS whether that process is still there.
func (l Local) Alive(handle string) (bool, error) {
	pid, err := strconv.Atoi(handle)
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("unusable worker handle %q", handle)
	}
	return process.Alive(pid)
}

// GroupLive reports whether process group pgid on this host has a member
// that is not a zombie.
func (l Local) GroupLive(pgid int) (bool, error) { return process.GroupLive(pgid) }

// Stop asks the supervisor at endpoint to stop its process group.
func (l Local) Stop(endpoint, token string) error { return process.RequestStop(endpoint, token) }

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
	p, err := spawnDetached(filepath.Join(dataRoot, logFile), bin, "manager")
	if err != nil {
		return err
	}
	return p.Release()
}

// localHandle is the Local handle for pid.
func localHandle(pid int) string { return strconv.Itoa(pid) }

// localPID is the PID in a Local handle, or 0 for any other handle.
func localPID(handle string) int {
	n, _ := strconv.Atoi(handle)
	return n
}

var (
	_ Launcher    = Local{}
	_ Supervisors = Local{}
)
