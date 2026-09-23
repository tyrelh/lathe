package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	issueNumber = regexp.MustCompile(`^#?([1-9][0-9]*)$`)
	issueShort  = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)
	issueURL    = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(?::[0-9]+)?/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/issues/[1-9][0-9]*/?$`)
)

// IssueReference validates input before queuing and expands shorthand for gh.
// A bare number is resolved against the target checkout's GitHub remote.
func IssueReference(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if m := issueNumber.FindStringSubmatch(ref); m != nil {
		return m[1], nil
	}
	if m := issueShort.FindStringSubmatch(ref); m != nil {
		return "https://github.com/" + m[1] + "/issues/" + m[2], nil
	}
	if issueURL.MatchString(ref) {
		return strings.TrimSuffix(ref, "/"), nil
	}
	return "", fmt.Errorf("invalid GitHub issue reference %q: use a number, issue URL, or owner/repo#number", ref)
}

// Issue is the task snapshot fetched at the start of a workflow.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
}

func (i Issue) Request() string {
	return fmt.Sprintf("GitHub issue #%d: %s\n\nSource: %s\n\n%s", i.Number, i.Title, i.URL, i.Body)
}

// FetchIssue reads only the issue title and description, without an agent turn.
func FetchIssue(ctx context.Context, repo, ref string) (Issue, error) {
	var issue Issue
	ref, err := IssueReference(ref)
	if err != nil {
		return issue, err
	}
	cmd := exec.CommandContext(ctx, "gh", "issue", "view", ref, "--json", "number,title,body,url")
	cmd.Dir = repo
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return issue, fmt.Errorf("gh issue view: %w", ctx.Err())
	}
	if err != nil {
		return issue, fmt.Errorf("gh issue view: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(out, &issue); err != nil {
		return issue, fmt.Errorf("decode gh issue view: %w", err)
	}
	if issue.Number <= 0 || strings.TrimSpace(issue.Title) == "" || issue.URL == "" {
		return issue, fmt.Errorf("gh issue view returned an incomplete issue (number, title and URL are required)")
	}
	return issue, nil
}
