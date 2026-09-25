package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CreatePR opens a pull request for the branch checked out in repo and returns
// its URL. The body goes through a temporary file rather than an argument: it is
// long, it has newlines, and it is text an agent wrote.
//
// The temporary file lives outside the repository, like everything else lathe
// writes about a repository. A draft is for work lathe could not accept.
func CreatePR(repo, title, body string, draft bool) (string, error) {
	f, err := os.CreateTemp("", "lathe-pr-*.md")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	args := []string{"pr", "create", "--title", title, "--body-file", f.Name()}
	if draft {
		args = append(args, "--draft")
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	url := lastLine(string(out))
	if url == "" {
		return "", fmt.Errorf("gh pr create printed no pull request URL")
	}
	return url, nil
}

// lastLine is the URL: gh prints progress before it, so the URL is the last
// thing it said rather than the whole of what it said.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// PR is what gh reports about a pull request: enough to tell whether it can
// still take a revision and where its branch diff starts.
type PR struct {
	URL   string `json:"url"`
	State string `json:"state"` // OPEN, CLOSED or MERGED
	Head  string `json:"headRefName"`
	Base  string `json:"baseRefName"`
}

// ViewPR reads a pull request's state now. Publication calls it on both sides
// of a push, because pushing and closing a pull request are separate
// operations and someone else may do the second in between.
func ViewPR(repo, url string) (PR, error) {
	cmd := exec.Command("gh", "pr", "view", url, "--json", "url,state,headRefName,baseRefName")
	cmd.Dir = repo
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return PR{}, fmt.Errorf("gh pr view %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	var pr PR
	if err := json.Unmarshal(out, &pr); err != nil {
		return PR{}, fmt.Errorf("decode gh pr view: %w", err)
	}
	return pr, nil
}

// Evidence is what a run recorded about the pull request it opened: the URL,
// its branch, and the commit lathe last shipped to it.
type Evidence struct {
	PR, Branch, Commit string
}

// CheckRevisable is the whole checkout and pull request half of revision
// eligibility, shared by every submitter and repeated by the worker. The
// checkout must be on the recorded branch at the shipped commit, and origin's
// branch must be at that commit too, with the pull request still open from it.
// Lathe switches, merges and rebases nothing to get there: every refusal names
// the state a person has to restore. A clean tree is the caller's check, made
// for every writing workflow.
func CheckRevisable(repo string, e Evidence) (PR, error) {
	current, err := Branch(repo)
	if err != nil {
		return PR{}, err
	}
	if current != e.Branch {
		return PR{}, fmt.Errorf("%s is on %s; check out %s, the pull request's branch, first", repo, current, e.Branch)
	}
	head, err := Head(repo)
	if err != nil {
		return PR{}, err
	}
	if head != e.Commit {
		return PR{}, fmt.Errorf("%s is at %s in %s, but lathe last shipped %s: the branch changed outside lathe; restore it to %s before revising",
			e.Branch, Short(head), repo, Short(e.Commit), Short(e.Commit))
	}
	pr, err := ViewPR(repo, e.PR)
	if err != nil {
		return PR{}, err
	}
	switch {
	case pr.State != "OPEN":
		return pr, fmt.Errorf("pull request %s is %s; only an open one takes a revision", e.PR, strings.ToLower(pr.State))
	case pr.Head != e.Branch:
		return pr, fmt.Errorf("pull request %s comes from %s, not %s", e.PR, pr.Head, e.Branch)
	}
	remote, err := RemoteHead(repo, e.Branch)
	if err != nil {
		return pr, err
	}
	if remote != e.Commit {
		return pr, fmt.Errorf("origin/%s is at %s, but lathe last shipped %s: the pull request's branch changed outside lathe (a push, or an accepted suggestion); bring it back to %s, or start a new build",
			e.Branch, orNone(Short(remote)), Short(e.Commit), Short(e.Commit))
	}
	return pr, nil
}

// Short is a commit abbreviated the way the banners print it.
func Short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func orNone(s string) string {
	if s == "" {
		return "nothing (it does not exist)"
	}
	return s
}
