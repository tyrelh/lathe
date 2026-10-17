package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/tyrelh/lathe/internal/workspace"
)

// issueFetchTimeout bounds the issue fetch when command_timeout is unset.
const issueFetchTimeout = time.Minute

// IssueRequest fetches and saves the source task inside a traced code phase.
// The fetch is cancelled with the run and bounded by command_timeout, else by
// issueFetchTimeout.
func (h *Handle) IssueRequest() (string, error) {
	r := h.run
	if err := h.Log("issue", r.Issue); err != nil {
		return "", err
	}
	ctx, cancel := r.commandContext(issueFetchTimeout)
	defer cancel()
	issue, err := workspace.FetchIssue(ctx, r.Repo, r.Issue)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(issue, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(r.Dir, "issue.json"), append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	request := issue.Request()
	if err := h.Log("request", request); err != nil {
		return "", err
	}
	return request, nil
}
