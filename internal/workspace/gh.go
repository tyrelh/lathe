package workspace

import (
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
// writes about a repository.
func CreatePR(repo, title, body string) (string, error) {
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

	cmd := exec.Command("gh", "pr", "create", "--title", title, "--body-file", f.Name())
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
