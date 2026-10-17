package workspace

// The git in this file is lathe's own work. It runs through the process
// package and never through an agent's tools, so the roster's shell deny list — which
// refuses `git push` and `git commit` to every agent — stays intact while lathe
// performs both itself. An agent judges the text; lathe performs the git.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tyrelh/lathe/internal/process"
)

// Head is the commit HEAD points at in repo.
func Head(repo string) (string, error) { return git(repo, "rev-parse", "HEAD") }

// ValidBranch reports whether git would accept name as a branch name. A name
// starting with a dash is read as an option and rejected with everything else
// git refuses, which is the answer this asks for anyway.
func ValidBranch(repo, name string) bool {
	_, err := git(repo, "check-ref-format", "--branch", name)
	return err == nil
}

// RefExists reports whether name is already taken in repo: a local branch or a
// tag, which the plain name resolves to, or a branch on origin, which it does
// not. The last is the one Push would collide with, turning a bad name into a
// late non-fast-forward, or into advancing a branch someone else owns.
//
// ponytail: origin's branches are read from the remote-tracking refs, so one
// created since the last fetch is missed. `git ls-remote --heads origin` if
// that ever bites; it costs a network call and credentials on every name.
func RefExists(repo, name string) bool {
	for _, ref := range []string{name, "refs/remotes/origin/" + name} {
		if _, err := git(repo, "rev-parse", "--verify", "--quiet", ref); err == nil {
			return true
		}
	}
	return false
}

// DiffStat is `git diff --stat HEAD` for repo's tracked changes; untracked
// files are absent.
func DiffStat(repo string) (string, error) { return git(repo, "diff", "--stat", "HEAD") }

// WorkingDiff is the uncommitted tracked change against HEAD, cut to limit
// bytes. Untracked files are absent; the caller lists them separately.
func WorkingDiff(repo string, limit int) (string, error) {
	patch, err := git(repo, "diff", "HEAD")
	if err != nil {
		return "", err
	}
	return cutPatch(patch, limit), nil
}

// cutPatch is patch cut to limit bytes, with a marker saying where it was cut.
func cutPatch(patch string, limit int) string {
	if len(patch) <= limit {
		return patch
	}
	return patch[:limit] + fmt.Sprintf("\n… cut at %d bytes; read the files for the rest\n", limit)
}

// Checkout creates branch at HEAD and switches to it; it fails when the name is
// taken.
func Checkout(repo, branch string) error { return gitRun(repo, nil, "checkout", "-b", branch) }

// Commit stages exactly paths and commits message. The message goes in on
// stdin, so a multi-paragraph body meets no shell and no quoting rules. -A so a
// deleted path is staged as a deletion; the pathspec is what keeps it from
// sweeping up anything the guard tolerated.
func Commit(repo, message string, paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("nothing to stage in %s", repo)
	}
	if err := gitRun(repo, nil, append([]string{"add", "-A", "--"}, paths...)...); err != nil {
		return err
	}
	return gitRun(repo, strings.NewReader(message), "commit", "-F", "-")
}

// Push publishes branch to origin. It sets no upstream, so repo's
// configuration is not written.
func Push(repo, branch string) error { return gitRun(repo, nil, "push", "origin", branch) }

// gitCmd is git in repo with literal pathspecs, env added to the process
// environment when non-nil, and stdin.
func gitCmd(repo string, env []string, stdin io.Reader, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"--literal-pathspecs"}, args...)...)
	cmd.Dir = repo
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = stdin
	return cmd
}

// git is git's trimmed stdout; a failure is the bare exit error.
func git(repo string, args ...string) (string, error) {
	out, err := process.Output(gitCmd(repo, nil, nil, args...))
	return strings.TrimSpace(string(out)), err
}

// gitRun is git for its exit code, with the output folded into the error.
// These are the calls whose failure a run reports rather than parses. Paths are
// literal, as in permit: a route like pages/blog/[slug].tsx is one file, not a glob.
func gitRun(repo string, stdin io.Reader, args ...string) error {
	if out, err := process.CombinedOutput(gitCmd(repo, nil, stdin, args...)); err != nil {
		return fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), repo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitEnv is git in repo with env added, returning stdout; a failure carries
// stderr.
func gitEnv(repo string, env []string, args ...string) ([]byte, error) {
	cmd := gitCmd(repo, env, nil, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := process.Output(cmd)
	if err != nil {
		return nil, fmt.Errorf("git %s in %s: %w: %s", strings.Join(args, " "), repo, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// RemoteHead is the commit origin's branch points at now, asked of origin
// itself rather than read from a remote-tracking ref that may be stale. A
// branch origin does not have is "", which is not an error.
func RemoteHead(repo, branch string) (string, error) {
	out, err := process.CombinedOutput(gitCmd(repo, nil, nil, "ls-remote", "origin", "refs/heads/"+branch))
	if err != nil {
		return "", fmt.Errorf("git ls-remote origin %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	sha, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	return sha, nil
}

// FetchBranch updates origin/<branch> from origin and returns the commit it
// names now. Nothing local but that remote-tracking ref changes. A fetch that
// finds the ref locked by another is retried.
func FetchBranch(repo, branch string) (string, error) {
	ref := "refs/remotes/origin/" + branch
	var err error
	for try := 1; ; try++ {
		err = gitRun(repo, nil, "fetch", "--no-tags", "origin", "+refs/heads/"+branch+":"+ref)
		if err == nil || try == fetchTries || !strings.Contains(err.Error(), ".lock") {
			break
		}
		time.Sleep(time.Duration(try) * 200 * time.Millisecond)
	}
	if err != nil {
		return "", err
	}
	return git(repo, "rev-parse", "--verify", ref)
}

// fetchTries is how many times FetchBranch tries a fetch that finds its ref
// locked.
const fetchTries = 5

// PushExpecting publishes HEAD to origin's branch only while that branch is
// still at expect, so an update someone made meanwhile is refused rather than
// overwritten. HEAD descends from expect, so a push the lease admits is a
// fast-forward and the branch keeps its history. HEAD may be detached; no
// local branch is moved.
func PushExpecting(repo, branch, expect string) error {
	ref := "refs/heads/" + branch
	return gitRun(repo, nil, "push", "--force-with-lease="+ref+":"+expect, "origin", "HEAD:"+ref)
}

// BranchDiff is the change a pull request carries, against origin's copy of
// its base branch: the stat, then the patch cut to limit bytes. A base that
// was never fetched is an error for the caller to report, not to guess past.
func BranchDiff(repo, base string, limit int) (stat, patch string, err error) {
	rng := "origin/" + base + "...HEAD"
	if stat, err = git(repo, "diff", "--stat", rng); err != nil {
		return "", "", fmt.Errorf("git diff %s: %w", rng, err)
	}
	if patch, err = git(repo, "diff", rng); err != nil {
		return "", "", fmt.Errorf("git diff %s: %w", rng, err)
	}
	return stat, cutPatch(patch, limit), nil
}
