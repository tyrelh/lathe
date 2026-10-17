package workspace

// Managed worktrees: a writing run executes in a detached worktree of the
// source checkout, created under the data root at the run's base commit and
// removed once the run's result is preserved. Only paths under
// <dataRoot>/worktrees/<run-id>/iteration-<n> are ever created or removed.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/permit"
)

// WorktreeRoot is the directory every managed worktree lives under.
func WorktreeRoot(dataRoot string) string { return filepath.Join(dataRoot, "worktrees") }

// WorktreePath is where one iteration of runID executes.
func WorktreePath(dataRoot, runID string, iteration int) string {
	return filepath.Join(WorktreeRoot(dataRoot), runID, "iteration-"+strconv.Itoa(iteration))
}

// RecoveryRef is the ref an iteration's otherwise unreachable commits are
// kept under.
func RecoveryRef(runID string, iteration int) string {
	return "refs/lathe/runs/" + runID + "/iteration-" + strconv.Itoa(iteration)
}

// CreateWorktree adds a detached worktree of repo for runID's iteration,
// checked out at commit, and returns its path. The path must not exist yet.
func CreateWorktree(dataRoot, repo, runID string, iteration int, commit string) (string, error) {
	path, err := owned(dataRoot, runID, iteration)
	if err != nil {
		return "", err
	}
	if commit == "" {
		return "", errors.New("no base commit to create a worktree from")
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("%s already exists; lathe does not reuse a worktree", path)
	}
	if _, err := git(repo, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return "", fmt.Errorf("%s does not have commit %s to start from", repo, Short(commit))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, gitRun(repo, nil, "worktree", "add", "--detach", path, commit)
}

// RemoveWorktree removes runID's iteration worktree from repo and from disk.
// Only that exact managed path and its entry in repo are ever removed. It
// does not preserve anything; the caller does that first.
func RemoveWorktree(dataRoot, repo, runID string, iteration int) error {
	path, err := owned(dataRoot, runID, iteration)
	if err != nil {
		return err
	}
	listed, err := worktreeListed(repo, path)
	if err != nil {
		return err
	}
	if listed {
		if err := gitRun(repo, nil, "worktree", "remove", "--force", "--force", path); err != nil {
			return err
		}
	}
	// A directory git does not list is the remains of an interrupted add or
	// remove of this run's iteration. A symlink there is never followed.
	if fi, err := os.Lstat(path); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory lathe created; it was left alone", path)
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	os.Remove(filepath.Dir(path)) // the run directory, once its last iteration is gone
	return nil
}

// worktreeListed reports whether repo lists a worktree at path.
func worktreeListed(repo, path string) (bool, error) {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git worktree list in %s: %w", repo, err)
	}
	want := canonical(path)
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && canonical(p) == want {
			return true, nil
		}
	}
	return false, nil
}

// owned returns runID's iteration path, refusing a run ID that is not one
// path segment or a path whose canonical form is not exactly
// <root>/<runID>/iteration-<n> under the canonical managed root.
func owned(dataRoot, runID string, iteration int) (string, error) {
	path := WorktreePath(dataRoot, runID, iteration)
	if runID == "" || runID == "." || runID == ".." || strings.ContainsAny(runID, `/\`) || iteration < 0 {
		return "", fmt.Errorf("%q iteration %d does not name a worktree lathe manages", runID, iteration)
	}
	root := canonical(WorktreeRoot(dataRoot))
	want := filepath.Join(root, runID, "iteration-"+strconv.Itoa(iteration))
	if canonical(path) != want {
		return "", fmt.Errorf("%s resolves outside %s; it was left alone", path, root)
	}
	return path, nil
}

// canonical resolves symlinks in the longest existing prefix of path, so a
// path that does not exist yet compares the same as it will once it does.
func canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	var rest []string
	for p := abs; ; {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return abs
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// SavePatch writes everything the worktree at wt holds beyond base — commits,
// tracked changes, deletions, new files git does not ignore, binary content
// and modes — to dest as a git patch, atomically. Paths matching protected
// are left out. It uses a temporary index, so neither the worktree's index
// nor the source checkout's changes. It reports whether there was anything
// to write; with nothing, any earlier export at dest is removed, so dest
// always reflects the latest export.
func SavePatch(wt, base, dest string, protected []string) (bool, error) {
	tmp, err := os.MkdirTemp("", "lathe-patch-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	index := filepath.Join(tmp, "index")
	env := []string{"GIT_INDEX_FILE=" + index}

	if _, err := gitEnv(wt, env, "read-tree", "HEAD"); err != nil {
		return false, err
	}
	if _, err := gitEnv(wt, env, "add", "-A"); err != nil {
		return false, err
	}
	names, err := gitEnv(wt, env, "diff-index", "--cached", "--name-only", "-z", base)
	if err != nil {
		return false, err
	}
	denied, kept := permit.Partition(strings.Split(string(names), "\x00"), protected)
	if len(kept) == 0 {
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return false, nil
	}
	if len(denied) > 0 {
		if _, err := gitEnv(wt, env, append([]string{"reset", "-q", base, "--"}, denied...)...); err != nil {
			return false, err
		}
	}
	patch, err := gitEnv(wt, env, "diff-index", "--cached", "-p", "--binary", "--full-index", base)
	if err != nil {
		return false, err
	}
	return true, config.WriteFileAtomic(dest, patch)
}

// PreserveHead points ref at the worktree's HEAD when HEAD has moved past
// base and no branch, remote-tracking branch or tag contains it. It returns
// the ref it wrote, or "" when HEAD needed no preserving.
func PreserveHead(wt, base, ref string) (string, error) {
	head, err := git(wt, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("reading HEAD in %s: %w", wt, err)
	}
	if head == base {
		return "", nil
	}
	reachable, err := git(wt, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", head,
		"refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return "", fmt.Errorf("checking whether %s is reachable: %w", Short(head), err)
	}
	if reachable != "" {
		return "", nil
	}
	if err := gitRun(wt, nil, "update-ref", ref, head); err != nil {
		return "", err
	}
	return ref, nil
}

// PublishedProtected lists the protected paths among the changes base..HEAD
// in repo carries, which is what a push would publish.
func PublishedProtected(repo, base string, protected []string) ([]string, error) {
	out, err := gitEnv(repo, nil, "diff-tree", "-r", "--name-only", "--no-commit-id", "-z", base, "HEAD")
	if err != nil {
		return nil, err
	}
	denied, _ := permit.Partition(strings.Split(string(out), "\x00"), protected)
	return denied, nil
}
