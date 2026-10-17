package worker

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tyrelh/lathe/internal/config"
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

// Revise validates that runID can take a revision (run state, recorded
// evidence, checkout, pull request, origin), then appends a revision iteration
// with the configuration capture resolves now. It returns the new iteration
// number. A refusal changes nothing, and db.Revise rechecks the run's state in
// its transaction; see ADR 0005 for the race between the two checks.
func Revise(db *trace.DB, dataRoot, runID, request string, ov config.Overrides, capture Capture) (int, error) {
	request = strings.TrimSpace(request)
	if request == "" {
		return 0, errors.New("a revision needs the change to make")
	}
	row, err := runRow(db, runID)
	if err != nil {
		return 0, err
	}
	if err := trace.Revisable(row); err != nil {
		return 0, err
	}
	ev, err := Evidence(dataRoot, row)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", trace.ErrNotRevisable, err)
	}
	var recorded Spec
	if err := json.Unmarshal(row.Spec, &recorded); err != nil || recorded.Workspace.Path == "" {
		return 0, fmt.Errorf("%w: %s records no checkout to revise it in", trace.ErrNotRevisable, runID)
	}
	ws := recorded.Workspace
	if err := ws.Check(); err != nil {
		return 0, err
	}
	if _, err := workspace.CheckRevisable(ws.Path, ev); err != nil {
		return 0, err
	}
	roster, err := capture(workflow.NameRevise, ov)
	if err != nil {
		return 0, err
	}
	spec, err := Spec{
		Version: SpecVersion, Workflow: workflow.NameRevise, Request: request,
		Workspace: ws, Roster: roster, Overrides: ov, Base: ev.Commit,
	}.Marshal()
	if err != nil {
		return 0, err
	}
	return db.Revise(runID, row.Iteration, request, spec)
}

// runRow is runID's run, or a "no run" error when there is none.
func runRow(db *trace.DB, runID string) (trace.Row, error) {
	row, err := db.Get(runID)
	if errors.Is(err, sql.ErrNoRows) {
		return trace.Row{}, fmt.Errorf("no run %s", runID)
	}
	return row, err
}
