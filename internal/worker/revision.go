package worker

import (
	"fmt"

	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workflow"
	"github.com/tyrelh/lathe/internal/workspace"
)

// Evidence is what a run's records establish about the pull request it
// opened: its URL from pr.json, and the branch and commit the run row says it
// shipped. A run from before revisions qualifies when its records have all
// three. Nothing is inferred from the pull request as it is now: a URL alone
// does not say the branch is still what lathe left there.
func Evidence(dataRoot string, row trace.Row) (workspace.Evidence, error) {
	if row.Workflow != workflow.NameBuild {
		return workspace.Evidence{}, fmt.Errorf("%s is a %s run; only a build opens a pull request to revise", row.ID, row.Workflow)
	}
	// The iteration that opened the pull request owns it: the first, or a
	// recovery of it that published.
	url, _, _ := prRecord(dataRoot, row)
	if url == "" {
		return workspace.Evidence{}, fmt.Errorf("%s has no recorded pull request (pr.json in %s or an iteration of it): it never opened one, or its record is gone",
			row.ID, ReportDir(dataRoot, row.ID, 0))
	}
	ev := workspace.Evidence{PR: url, Branch: row.Branch, Commit: row.Commit}
	if ev.Branch == "" || ev.Commit == "" {
		return ev, fmt.Errorf("%s does not record the branch and commit it shipped to %s, so lathe cannot tell whether that branch is still as it left it", row.ID, url)
	}
	return ev, nil
}

// revisionOf establishes, under the worker's claim, everything a revision
// inherits: the pull request and origin checks the submitter ran, again; the
// shipped commit, fetched and verified; the pull request's base, fetched for
// its diff; and the earlier iterations. git and gh run in repo, whose own
// state is not checked.
func revisionOf(db *trace.DB, dataRoot string, row trace.Row, repo string) (*run.Revision, error) {
	ev, err := Evidence(dataRoot, row)
	if err != nil {
		return nil, err
	}
	pr, err := workspace.CheckRevisable(repo, ev)
	if err != nil {
		return nil, fmt.Errorf("%w; nothing was changed", err)
	}
	fetched, err := workspace.FetchBranch(repo, ev.Branch)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w; nothing was changed", ev.Branch, err)
	}
	if fetched != ev.Commit {
		return nil, fmt.Errorf("%w; nothing was changed", workspace.BranchChanged(ev.Branch, fetched, ev.Commit))
	}
	// The planner's branch diff is taken against origin's base; a stale or
	// missing one would describe the wrong change.
	if _, err := workspace.FetchBranch(repo, pr.Base); err != nil {
		return nil, fmt.Errorf("fetching %s, the pull request's base: %w; nothing was changed", pr.Base, err)
	}
	prior, err := priors(db, dataRoot, row)
	if err != nil {
		return nil, err
	}
	return &run.Revision{PR: ev.PR, Branch: ev.Branch, Base: pr.Base, Head: ev.Commit, Prior: prior}, nil
}
