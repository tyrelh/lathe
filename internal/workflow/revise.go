package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/workspace"
)

// diffLimit bounds the branch diff a revision's planner is shown. ponytail: a
// flat cut; the planner can read any file the stat names, and a smarter
// excerpt waits until a real pull request outgrows this.
const diffLimit = 20000

// Revise extends a build whose pull request is still open: it plans a further
// change on top of the branch as it stands, implements and validates it, and
// pushes it to the same pull request. The run keeps its ID, its pull request
// and its history; this is one more iteration of it.
//
// Every revision enters at the planner, however small, and gets fresh plan
// review and repair allowances. Only an accepted change is published: anything
// else stops before the commit and leaves the work uncommitted in the tree,
// with the pull request where it was. A plan that needs no code change is
// validated as the branch stands and succeeds without a commit.
//
// Publication is lathe's own, as in build: the committer writes the message,
// lathe commits, then checks the pull request is open and its branch is still
// where lathe left it, pushes with that as the expected head, and asks origin
// afterwards what arrived. The title and description are left alone.
func Revise(r *run.Run) int {
	rev := r.Revision
	if rev == nil {
		return r.Finish(false, "a revision needs the run it extends, and the worker established none")
	}
	r.Request = revisionRequest(rev, r.Request)

	var c code
	var sha string
	g := run.NewGraph(r)
	addRequestNode(g, r)
	addPlanNodes(g, r, &c.plan, revisionBrief(r.Request, rev, r.Repo))
	addCodeNodes(g, r, &c)

	// The stricter rule a revision holds to: build publishes unaccepted work
	// as a draft, but a revision's pull request is already open for review, and
	// pushing unaccepted work to it would present that work as finished.
	g.Add(run.Node{Name: "commit", Owner: "committer"}, func(e *run.Entry) (string, error) {
		if !c.v.Accepted() {
			printOutcome(r.Out, &c.v)
			return "", fmt.Errorf("validation %s: %s; the changes are left uncommitted and the pull request is unchanged",
				c.v.Outcome, c.v.Reason)
		}
		paths, err := permit.Changed(r.Repo)
		if err != nil {
			return "", err
		}
		if len(paths) == 0 {
			return "", e.Log("commit", "validated with no code changes; nothing to commit")
		}
		if sha, _, err = commit(e, r, &c, paths); err != nil {
			return "", err
		}
		// The commit is this iteration's evidence from here on; the run's
		// shipped commit moves only once origin has it.
		if err := r.Evidence(sha, rev.Head); err != nil {
			return "", err
		}
		return "", permit.Clean(r.Repo)
	})

	g.Add(run.Node{Name: "publish", Owner: "engineer"}, func(e *run.Entry) (string, error) {
		if sha == "" {
			return "", nil
		}
		return "", publish(e, r, rev, sha)
	})

	if err := g.Run(); err != nil {
		return r.Finish(false, err.Error())
	}
	fmt.Fprintf(r.Out, "\n%s\n", *c.out.Summary)
	if sha == "" {
		fmt.Fprintf(r.Out, "\n  no code changes; %s stays at %s\n  %s\n", rev.Branch, short(rev.Head), rev.PR)
		return r.Finish(true, "validated with no code changes; nothing was committed or published")
	}
	fmt.Fprintf(r.Out, "\n  %s @ %s\n  %s\n", rev.Branch, short(sha), rev.PR)
	return r.Finish(true, "")
}

// publish pushes sha to the pull request's branch and establishes whether it
// arrived. The checks before the push stop on a closed pull request or a
// branch someone else moved; the lease on the push refuses an update made
// after those checks. Neither covers a pull request closed between the check
// and the push, since git cannot see that — so it is read again afterwards,
// and the iteration fails if it closed, saying whether the commit was pushed.
func publish(e *run.Entry, r *run.Run, rev *run.Revision, sha string) error {
	local := fmt.Sprintf("the change is committed locally on %s as %s", rev.Branch, short(sha))
	pr, err := workspace.ViewPR(r.Repo, rev.PR)
	if err != nil {
		return fmt.Errorf("%w; nothing was pushed, and %s", err, local)
	}
	if pr.State != "OPEN" {
		return fmt.Errorf("pull request %s is %s; nothing was pushed, and %s", rev.PR, strings.ToLower(pr.State), local)
	}
	remote, err := workspace.RemoteHead(r.Repo, rev.Branch)
	if err != nil {
		return fmt.Errorf("%w; nothing was pushed, and %s", err, local)
	}
	if remote != rev.Head {
		if err := r.Evidence(sha, remote); err != nil {
			return err
		}
		return fmt.Errorf("origin/%s is at %s, not %s where lathe left it; nothing was pushed, and %s",
			rev.Branch, short(remote), short(rev.Head), local)
	}

	pushErr := workspace.PushExpecting(r.Repo, rev.Branch, rev.Head)
	if err := e.Log("push", pushed(pushErr)); err != nil {
		return err
	}
	// The push's own answer can be lost; origin is the record of what arrived.
	after, verifyErr := workspace.RemoteHead(r.Repo, rev.Branch)
	if verifyErr != nil {
		return fmt.Errorf("publication is uncertain: %s, and origin could not be read afterwards (%v); %s, and origin/%s was last verified at %s",
			pushed(pushErr), verifyErr, local, rev.Branch, short(remote))
	}
	if err := r.Evidence(sha, after); err != nil {
		return err
	}
	if after != sha {
		return fmt.Errorf("not published: %s, and origin/%s is at %s; %s", pushed(pushErr), rev.Branch, short(after), local)
	}
	if err := r.Shipped(rev.Branch, sha); err != nil {
		return err
	}
	if pushErr != nil {
		if err := e.Log("push", "origin has "+short(sha)+" despite the error, so the push is treated as delivered"); err != nil {
			return err
		}
	}
	if pr, err = workspace.ViewPR(r.Repo, rev.PR); err != nil {
		return fmt.Errorf("pushed %s to %s, but the pull request could not be read afterwards: %w", short(sha), rev.Branch, err)
	}
	if pr.State != "OPEN" {
		return fmt.Errorf("pushed %s to %s, but pull request %s is now %s", short(sha), rev.Branch, rev.PR, strings.ToLower(pr.State))
	}
	return nil
}

func pushed(err error) string {
	if err == nil {
		return "git push succeeded"
	}
	return "git push failed: " + err.Error()
}

// revisionRequest is the request every agent in a revision is given: the
// change asked for, and what it revises. The planner gets more than this;
// everyone after it works from the plan.
func revisionRequest(rev *run.Revision, change string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nThis revises pull request %s (branch %s, at %s).", change, rev.PR, rev.Branch, short(rev.Head))
	if len(rev.Prior) > 0 {
		fmt.Fprintf(&b, " It was built for:\n\n%s\n", rev.Prior[0].Request)
	}
	var earlier []string
	for _, p := range rev.Prior[min(1, len(rev.Prior)):] {
		earlier = append(earlier, fmt.Sprintf("revision %d: %s", p.Iteration, p.Request))
	}
	section(&b, "Earlier revisions asked for", earlier)
	return b.String()
}

// revisionBrief is the planner's first request: the revision request, then
// what the previous iterations planned and concluded, and the branch diff the
// pull request carries now.
func revisionBrief(request string, rev *run.Revision, repo string) string {
	var b strings.Builder
	b.WriteString(request)

	var plan PlanOutput
	if dir := latest(rev.Prior, "plan.json", &plan); dir != "" && plan.Summary != nil && plan.Steps != nil && plan.Files != nil && plan.Risks != nil {
		b.WriteString("\n### Previous plan\n")
		printPlan(&b, &plan)
	}
	var impl ImplementOutput
	if dir := latest(rev.Prior, "implement.json", &impl); dir != "" && impl.Summary != nil {
		fmt.Fprintf(&b, "\n### Previous implementation\n\n%s\n", *impl.Summary)
	}
	var v Validation
	if dir := latest(rev.Prior, "validation.json", &v); dir != "" && v.Summary != "" {
		fmt.Fprintf(&b, "\n### Previous validation\n\n%s\n", v.Summary)
	}

	stat, patch, err := workspace.BranchDiff(repo, rev.Base, diffLimit)
	if err != nil {
		fmt.Fprintf(&b, "\n### Branch diff\n\nUnavailable (%v); read the files instead.\n", err)
	} else {
		fmt.Fprintf(&b, "\n### Branch diff against %s\n\n%s\n\n```diff\n%s\n```\n", rev.Base, stat, patch)
	}

	b.WriteString("\n### What to plan\n\nThe branch already holds the pull request's work, committed and published. " +
		"Plan only the further change this revision asks for, on top of it. `files` is the whole write scope for this revision. " +
		"If the request needs no code change, say why in `summary` and return an empty `files` list: " +
		"the branch is then validated as it stands and nothing is committed.\n")
	return b.String()
}

// latest decodes name from the most recent earlier iteration that has it and
// returns that iteration's directory, or "" if none does.
func latest(prior []run.Prior, name string, out any) string {
	for _, p := range slices.Backward(prior) {
		b, err := os.ReadFile(filepath.Join(p.Dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && json.Unmarshal(b, out) == nil {
			return p.Dir
		}
	}
	return ""
}
