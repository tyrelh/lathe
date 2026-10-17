package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyrelh/lathe/internal/permit"
)

// PatchDigest is the sha256 of the file at path, in hex.
func PatchDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PatchPaths lists every path a saved patch touches, checked in repo
// without applying it. It refuses a patch that is damaged, escapes the
// repository or touches a protected path.
func PatchPaths(repo, patch string, protected []string) ([]string, error) {
	out, err := gitEnv(repo, nil, "apply", "--numstat", "-z", patch)
	if err != nil {
		return nil, fmt.Errorf("the saved patch is damaged: %w", err)
	}
	var paths []string
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		fields := strings.SplitN(rec, "\t", 3)
		if len(fields) != 3 || fields[2] == "" {
			return nil, fmt.Errorf("the saved patch is damaged: unreadable entry %q", rec)
		}
		paths = append(paths, fields[2])
	}
	if len(paths) == 0 {
		return nil, errors.New("the saved patch is damaged: it changes nothing")
	}
	for _, p := range paths {
		if permit.Escapes(p) {
			return nil, fmt.Errorf("the saved patch names %q, outside the repository; it was not applied", p)
		}
	}
	if denied, _ := permit.Partition(paths, protected); len(denied) > 0 {
		return nil, fmt.Errorf("the saved patch changes protected paths (%s); it was not applied", strings.Join(denied, ", "))
	}
	return paths, nil
}

// Restore applies patch, whose sha256 must be digest, exactly to the
// worktree wt, which must be clean and at base. Binary content, modes, new
// files and deletions are applied; nothing is staged or committed. It
// returns the uncommitted paths afterwards.
func Restore(wt, base, patch, digest string, protected []string) ([]string, error) {
	got, err := PatchDigest(patch)
	if err != nil {
		return nil, fmt.Errorf("the saved patch %s is unavailable: %w", patch, err)
	}
	if got != digest {
		return nil, fmt.Errorf("the saved patch %s changed since the recovery was submitted; it was not applied", patch)
	}
	head, err := git(wt, "rev-parse", "--verify", "HEAD")
	if err != nil || head != base {
		return nil, fmt.Errorf("the worktree is at %s, not the recorded base %s; nothing was applied", Short(head), Short(base))
	}
	if err := permit.Clean(wt); err != nil {
		return nil, err
	}
	if _, err := PatchPaths(wt, patch, protected); err != nil {
		return nil, err
	}
	if _, err := gitEnv(wt, nil, "apply", "--check", "--binary", patch); err != nil {
		return nil, fmt.Errorf("the saved patch does not apply to %s: %w", Short(base), err)
	}
	if _, err := gitEnv(wt, nil, "apply", "--binary", patch); err != nil {
		return nil, fmt.Errorf("applying the saved patch: %w", err)
	}
	return permit.Changed(wt)
}

// patchTree is the tree base would have with patch applied, computed in a
// temporary index so nothing in repo changes. A patch whose tree equals a
// commit's holds no work beyond that commit.
func patchTree(repo, base, patch string) (string, error) {
	tmp, err := os.MkdirTemp("", "lathe-tree-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + tmp + "/index"}
	if _, err := gitEnv(repo, env, "read-tree", base); err != nil {
		return "", err
	}
	if _, err := gitEnv(repo, env, "apply", "--cached", "--binary", patch); err != nil {
		return "", fmt.Errorf("the saved patch does not apply to %s: %w", Short(base), err)
	}
	out, err := gitEnv(repo, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Residual writes to dest the work patch holds beyond commit, where patch
// was saved against base and commit is the published part of that work. It
// reports whether there is any; with none, dest is removed. A patch that
// does not apply to base is damaged evidence.
func Residual(repo, base, patch, commit, dest string) (bool, error) {
	tree, err := patchTree(repo, base, patch)
	if err != nil {
		return false, err
	}
	want, err := treeOf(repo, commit)
	if err != nil {
		return false, fmt.Errorf("%s does not have the published commit %s: %w", repo, Short(commit), err)
	}
	if tree == want {
		if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return false, nil
	}
	diff, err := gitEnv(repo, nil, "diff", "--binary", "--full-index", commit, tree)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return false, err
	}
	return true, os.WriteFile(dest, diff, 0o600)
}

// treeOf is commit's tree.
func treeOf(repo, commit string) (string, error) {
	return git(repo, "rev-parse", "--verify", commit+"^{tree}")
}

// ResolveCommit is the exact commit ref names in repo.
func ResolveCommit(repo, ref string) (string, error) {
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("%q is not a commit in %s", ref, repo)
	}
	sha, err := git(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || sha == "" {
		return "", fmt.Errorf("%q is not a commit in %s", ref, repo)
	}
	return sha, nil
}
