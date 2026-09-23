package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/tyrelh/lathe/internal/workspace"
)

// IssueRequest fetches and saves the source task inside a traced code phase.
// Its deadline and cancellation belong to the run, just like other code phases.
func (h *Handle) IssueRequest() (string, error) {
	r := h.run
	if err := h.Log("issue", r.Issue); err != nil {
		return "", err
	}
	deadline := r.cfg.CommandDeadline
	if deadline <= 0 {
		deadline = time.Minute
	}
	ctx, cancel := context.WithTimeout(r.ctx, deadline)
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
