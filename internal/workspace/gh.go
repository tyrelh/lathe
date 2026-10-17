package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tyrelh/lathe/internal/process"
)

// CreatePR opens a pull request from branch, already pushed, and returns its
// URL. A draft is for work lathe could not accept.
func CreatePR(repo, branch, title, body string, draft bool) (string, error) {
	// The body goes through a file rather than an argument: it is long, it has
	// newlines, and it is text an agent wrote. The file lives outside the
	// repository, like everything else lathe writes about a repository.
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

	args := []string{"pr", "create", "--head", branch, "--title", title, "--body-file", f.Name()}
	if draft {
		args = append(args, "--draft")
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = repo
	out, err := process.CombinedOutput(cmd)
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
	Draft bool   `json:"isDraft"`
	// HeadOid is the commit the pull request's head points at, and Cross is
	// whether that head lives in a repository other than its base's.
	HeadOid string `json:"headRefOid"`
	Cross   bool   `json:"isCrossRepository"`
}

// prFields is every field PR decodes.
const prFields = "url,state,headRefName,baseRefName,isDraft,headRefOid,isCrossRepository"

// CheckPublication establishes, fail-closed, that origin's branch is at head
// (absent when head is "") and, with url, that the pull request is open,
// comes from branch in origin's own repository, and points at head.
func CheckPublication(repo, branch, head, url string) (PR, error) {
	remote, err := RemoteHead(repo, branch)
	if err != nil {
		return PR{}, err
	}
	if remote != head {
		return PR{}, fmt.Errorf("origin/%s is at %s, not %s where lathe left it: it moved outside lathe", branch, orNone(Short(remote)), orNone(Short(head)))
	}
	if url == "" {
		return PR{}, nil
	}
	pr, err := ViewPR(repo, url)
	if err != nil {
		return PR{}, err
	}
	return pr, OwnsPR(pr, branch, head)
}

// OwnsPR refuses a pull request that is not open, or whose head is not
// branch in its base repository at head.
func OwnsPR(pr PR, branch, head string) error {
	switch {
	case pr.State != "OPEN":
		return fmt.Errorf("pull request %s is %s; reopening or follow-up work needs a separate request", pr.URL, strings.ToLower(pr.State))
	case pr.Cross:
		return fmt.Errorf("pull request %s comes from another repository; lathe does not own it", pr.URL)
	case pr.Head != branch:
		return fmt.Errorf("pull request %s comes from %s, not %s", pr.URL, pr.Head, branch)
	case pr.HeadOid == "" || pr.HeadOid != head:
		return fmt.Errorf("pull request %s points at %s, not %s; its ownership cannot be established", pr.URL, orNone(Short(pr.HeadOid)), Short(head))
	}
	return nil
}

// FindPR is the one pull request, in any state, whose head is branch in
// repo's GitHub repository. It returns false when there is none and refuses
// more than one: ownership must be unambiguous.
func FindPR(repo, branch string) (PR, bool, error) {
	var prs []PR
	if err := ghJSON(repo, "gh pr list --head "+branch, "gh pr list", &prs, "pr", "list", "--head", branch, "--state", "all", "--json", prFields); err != nil {
		return PR{}, false, err
	}
	var match []PR
	for _, pr := range prs {
		if pr.Head != branch {
			continue
		}
		// A fork may use the same branch name; that alone makes the
		// match ambiguous.
		if pr.Cross {
			return PR{}, false, fmt.Errorf("pull request %s from another repository also uses branch %s; lathe cannot establish which pull request is its own", pr.URL, branch)
		}
		match = append(match, pr)
	}
	switch len(match) {
	case 0:
		return PR{}, false, nil
	case 1:
		return match[0], true, nil
	}
	return PR{}, false, fmt.Errorf("%d pull requests come from %s; lathe cannot tell which is its own", len(match), branch)
}

// ReadyPR marks a draft pull request ready for review.
func ReadyPR(repo, url string) error {
	cmd := exec.Command("gh", "pr", "ready", url)
	cmd.Dir = repo
	if out, err := process.CombinedOutput(cmd); err != nil {
		return fmt.Errorf("gh pr ready %s: %w: %s", url, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ViewPR reads url's current state from gh.
func ViewPR(repo, url string) (PR, error) {
	var pr PR
	if err := ghJSON(repo, "gh pr view "+url, "gh pr view", &pr, "pr", "view", url, "--json", prFields); err != nil {
		return PR{}, err
	}
	return pr, nil
}

// ghJSON runs gh with args in repo and decodes its stdout into v. A failure
// is reported under label with gh's stderr, a bad decode under decodeLabel.
func ghJSON(repo, label, decodeLabel string, v any, args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Dir = repo
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := process.Output(cmd)
	if err != nil {
		return fmt.Errorf("%s: %w: %s", label, err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("decode %s: %w", decodeLabel, err)
	}
	return nil
}

// Evidence is what a run recorded about the pull request it opened: the URL,
// its branch, and the commit lathe last shipped to it.
type Evidence struct {
	PR, Branch, Commit string
}

// CheckRevisable checks the pull request half of revision eligibility: the
// pull request is open, comes from the recorded branch, and origin's branch
// is at the commit lathe last shipped. gh and git run in repo, whose branch,
// HEAD and changes are not checked. Every refusal names the state a person
// has to restore.
func CheckRevisable(repo string, e Evidence) (PR, error) {
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
		return pr, BranchChanged(e.Branch, remote, e.Commit)
	}
	return pr, nil
}

// BranchChanged is the refusal for origin's branch having moved from shipped,
// the commit lathe last shipped to it, to at.
func BranchChanged(branch, at, shipped string) error {
	return fmt.Errorf("origin/%s is at %s, but lathe last shipped %s: the pull request's branch changed outside lathe (a push, or an accepted suggestion); bring it back to %s, or start a new build",
		branch, orNone(Short(at)), Short(shipped), Short(shipped))
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
