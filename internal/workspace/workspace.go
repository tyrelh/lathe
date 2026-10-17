// Package workspace is where a run executes: the source checkout a run was
// submitted from, on which host and branch, and the managed worktrees writing
// runs execute in. It holds the locks that keep two lathe processes out of
// one directory, and lathe's own git and gh calls.
//
// Commands that can change a managed worktree run through the process
// package, so a worker that installed a supervised runner has every one of
// them gated and stoppable.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tyrelh/lathe/internal/process"
)

// KindLocalPath is the only workspace kind this release ships. A local path
// cannot move a developer's checkout onto another machine, which is why the
// descriptor is typed now and why git_ref is the one that matters for hosting.
const KindLocalPath = "local_path"

// Desc names where a run executes. It is captured at submission and checked
// again when a worker starts.
type Desc struct {
	Kind string `json:"kind"`
	Path string `json:"path"` // canonical absolute checkout path
	Host string `json:"host"`
}

// Local describes a checkout on this machine.
func Local(path string) (Desc, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Desc{}, err
	}
	// Symlinks are resolved so two spellings of one checkout cannot both hold
	// a reservation on it.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	host, err := os.Hostname()
	if err != nil {
		return Desc{}, err
	}
	return Desc{Kind: KindLocalPath, Path: abs, Host: host}, nil
}

// Check rejects a workspace this host cannot execute.
func (d Desc) Check() error {
	if d.Kind != KindLocalPath {
		return fmt.Errorf("unknown workspace kind %q", d.Kind)
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	if d.Host != host {
		return fmt.Errorf("workspace %s belongs to host %s, not %s", d.Path, d.Host, host)
	}
	return nil
}

// Branch is the checked-out branch name, or an error on a detached HEAD: a
// run records the branch it was submitted against, and a detached HEAD has
// none to record.
func Branch(repo string) (string, error) {
	out, err := git(repo, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("%s has no checked-out branch (detached HEAD?)", repo)
	}
	return out, nil
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := process.Output(cmd)
	return strings.TrimSpace(string(out)), err
}

// Lock is an OS lock on one checkout or managed worktree, held for the length of an execution. It
// coordinates lathe processes only: an editor or any other tool can still
// write to the directory while it is held.
type Lock struct{ f *os.File }

// Acquire takes the lock for repo, without blocking. The lock file lives in
// the data root rather than in the repository: nothing lathe does is stamped
// into the repository it acts on.
func Acquire(dataRoot, repo string) (*Lock, error) {
	dir := filepath.Join(dataRoot, "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, lockName(repo)+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another lathe process holds %s", repo)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. Closing the file is what releases the flock, so the
// unlock and the close are one call rather than two chances to leak it.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return f.Close()
}

// lockName is the filesystem-safe lock file name for path.
func lockName(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Base(path) + "-" + hex.EncodeToString(sum[:4])
}
