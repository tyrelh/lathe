package workspace

// The git in this file is lathe's own work. It runs through exec.Command and
// never through run.Handle.Command, so the roster's shell deny list — which
// refuses `git push` and `git commit` to every agent — stays intact while lathe
// performs both itself. An agent judges the text; lathe performs the git.

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Head is the commit HEAD points at. A worker records it before a run starts,
// and the commit phase compares against it afterwards.
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

// DiffStat is the shape of the uncommitted change, for an agent that has to
// describe it. Untracked files are absent by construction; the caller passes
// the changed path list alongside, which has them.
func DiffStat(repo string) (string, error) { return git(repo, "diff", "--stat", "HEAD") }

// Checkout creates branch and switches to it. It fails when the name is taken,
// which is the same thing the gate on the proposed name already checked.
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

// Push publishes branch and sets it as the upstream, so gh has a remote branch
// to open a pull request against.
func Push(repo, branch string) error { return gitRun(repo, nil, "push", "-u", "origin", branch) }

// gitRun is git for its exit code, with the output folded into the error.
// These are the calls whose failure a run reports rather than parses. Paths are
// literal, as in permit: a route like pages/blog/[slug].tsx is one file, not a glob.
func gitRun(repo string, stdin io.Reader, args ...string) error {
	cmd := exec.Command("git", append([]string{"--literal-pathspecs"}, args...)...)
	cmd.Dir = repo
	cmd.Stdin = stdin
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), repo, err, strings.TrimSpace(string(out)))
	}
	return nil
}
