package workflow

import (
	"fmt"
	"io"
	"strings"

	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/workspace"
)

// The three envelopes below share a division of labour: the agent judges the
// text, lathe performs the git. Nothing an agent returns here is a claim about
// work it did, so there is nothing to gate after the fact — lathe ran the
// action, and no report can lie about having run it. The only checks left are
// on the proposed text, before it is used.

// BranchOutput is the name the change lands on.
type BranchOutput struct {
	Summary *string   `json:"summary"`
	Branch  *string   `json:"branch"`
	Wrote   *[]string `json:"artifacts"`
}

func (b *BranchOutput) Validate() []string {
	var v []string
	if b.Summary == nil || strings.TrimSpace(*b.Summary) == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if b.Branch == nil || strings.TrimSpace(*b.Branch) == "" {
		v = append(v, `"branch" is missing or empty`)
	}
	if b.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	return v
}

func (b *BranchOutput) Artifacts() []string {
	if b.Wrote == nil {
		return nil
	}
	return *b.Wrote
}

// Name is the proposed branch, trimmed. The gate and the checkout read it
// through here so they cannot disagree about surrounding whitespace.
func (b *BranchOutput) Name() string { return strings.TrimSpace(*b.Branch) }

// BranchUsable asks git, not the agent, whether the proposed name can be
// created. It runs while the agent still has a session to correct in, and
// before a single file has been written, which is the cheapest place for a
// branch name to fail.
func BranchUsable(e run.Envelope, r *run.Run) []string {
	// Gates run only on an envelope Validate accepted, so Branch is set.
	b, ok := e.(*BranchOutput)
	if !ok {
		return nil
	}
	name := b.Name()
	if current, err := workspace.Branch(r.Repo); err == nil && current == name {
		return []string{fmt.Sprintf("%q is the branch that is already checked out; the change needs its own", name)}
	}
	switch {
	case !workspace.ValidBranch(r.Repo, name):
		return []string{fmt.Sprintf("git will not accept %q as a branch name", name)}
	case workspace.RefExists(r.Repo, name):
		return []string{fmt.Sprintf("%q already exists in this repository; propose a name that does not", name)}
	}
	return nil
}

// CommitOutput carries the complete commit message, subject and body.
type CommitOutput struct {
	Summary *string   `json:"summary"`
	Message *string   `json:"message"`
	Wrote   *[]string `json:"artifacts"`
}

func (c *CommitOutput) Validate() []string {
	var v []string
	if c.Summary == nil || strings.TrimSpace(*c.Summary) == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if c.Message == nil || strings.TrimSpace(*c.Message) == "" {
		v = append(v, `"message" is missing or empty; it is the whole commit message, subject and body`)
	}
	if c.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	return v
}

func (c *CommitOutput) Artifacts() []string {
	if c.Wrote == nil {
		return nil
	}
	return *c.Wrote
}

// PROutput is the pull request as the repository's readers expect it.
type PROutput struct {
	Summary *string   `json:"summary"`
	Title   *string   `json:"title"`
	Body    *string   `json:"body"`
	Wrote   *[]string `json:"artifacts"`
}

func (p *PROutput) Validate() []string {
	var v []string
	if p.Summary == nil || strings.TrimSpace(*p.Summary) == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if p.Title == nil || strings.TrimSpace(*p.Title) == "" {
		v = append(v, `"title" is missing or empty`)
	}
	if p.Body == nil || strings.TrimSpace(*p.Body) == "" {
		v = append(v, `"body" is missing or empty`)
	}
	if p.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	return v
}

func (p *PROutput) Artifacts() []string {
	if p.Wrote == nil {
		return nil
	}
	return *p.Wrote
}

// prResult is pr.json: the agent's report plus the URL lathe got back. There is
// no branch.json or commit.json because the branch name and the commit sha are
// both recoverable from git and already in the trace; the URL is the one thing
// here the tree cannot reproduce, which is what earns it a file.
//
// Draft and Validation keep an unaccepted change's pull request from reading as
// an accepted one in anything that reads the file rather than the banner.
type prResult struct {
	*PROutput
	URL        string `json:"url"`
	Draft      bool   `json:"draft"`
	Validation string `json:"validation"`
}

// branchRequest is what the brancher is given. The plan is everything that has
// happened by the time it runs, and the name has to describe the change rather
// than the work that has not started yet.
func branchRequest(request string, plan *PlanOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Request\n\n%s\n\n## Accepted plan\n\n%s\n", request, *plan.Summary)
	section(&b, "### Steps", *plan.Steps)
	return b.String()
}

// commitRequest is the builder's handoff plus what the tree actually holds: the
// summary is a claim, the diffstat and the path list are measured.
func commitRequest(c *code, stat string, paths []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n## Implementation\n\n%s\n\n## git diff --stat\n\n%s\n",
		c.handoff, *c.out.Summary, stat)
	section(&b, "### Changed files", paths)
	unaccepted(&b, c)
	return b.String()
}

// prRequest adds what the two git phases settled: the branch the change is on
// and the message it was committed with.
func prRequest(c *code, branch, message string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n## Implementation\n\n%s\n\n## Branch\n\n%s\n\n## Commit message\n\n%s\n",
		c.handoff, *c.out.Summary, branch, message)
	unaccepted(&b, c)
	return b.String()
}

// unaccepted tells a git agent the change it is describing was not accepted,
// so neither the message nor the title claims it is finished. The validation
// report itself is appended to the body by lathe, not left to the agent.
func unaccepted(w io.Writer, c *code) {
	if !c.v.Accepted() {
		fmt.Fprintf(w, "\n## Validation not accepted\n\nThis change is unfinished (%s: %s). It ships as a draft pull request; do not describe it as tested or complete.\n",
			c.v.Outcome, c.v.Reason)
	}
}

// Build plans, implements, validates and ships a change: it ends at a pull
// request, which is where building one ends for an engineer. Implement is the
// same run without the three git phases.
//
// An accepted change opens a normal pull request. One the code nodes handed off
// unaccepted — findings unresolved at the send-back limit, or validation
// incomplete — opens a draft carrying the validation report, and the run still
// fails: publishing it is not accepting it. Nothing else is published: an
// error, cancellation or changed source ends the run before commit.
//
// The three git nodes are forward-only. A failure in any of them ends the run,
// and a failure after the commit leaves the commit on its branch rather than
// losing the work; the closing reason says so.
func Build(r *run.Run) int {
	var c code
	var branch, sha, message, url string

	g := run.NewGraph(r)
	addRequestNode(g, r)
	addPlanNodes(g, r, &c.plan)

	// Immediately after the plan nodes, so it runs whether review accepted the
	// plan or its send-back budget ran out and the objections became risks. The
	// graph gives both for free; neither needs a condition.
	g.Add(run.Node{Name: "branch", Owner: "brancher"}, func(e *run.Entry) (string, error) {
		var out BranchOutput
		if err := e.Call(&out, branchRequest(r.Request, &c.plan),
			run.ArtifactsExist, run.FilesNonEmpty, BranchUsable); err != nil {
			return "", err
		}
		branch = out.Name()
		return "", workspace.Checkout(r.Repo, branch)
	})

	addCodeNodes(g, r, &c)

	// After adjudicate, which forwards accepted work and the eligible
	// unaccepted outcomes; c.v.Outcome says which this is.
	g.Add(run.Node{Name: "commit", Owner: "committer"}, func(e *run.Entry) (string, error) {
		// Captured on entry: the paths are the accumulated accepted scope after
		// enforcement, which is what lathe stages — an agent's own `git add -A`
		// would sweep up anything the guard tolerated.
		paths, err := permit.Changed(r.Repo)
		if err != nil {
			return "", err
		}
		if len(paths) == 0 {
			return "", fmt.Errorf("the tree holds no changes to commit")
		}
		before, err := workspace.Head(r.Repo)
		if err != nil {
			return "", err
		}
		stat, err := workspace.DiffStat(r.Repo)
		if err != nil {
			return "", err
		}

		var out CommitOutput
		if err := e.Call(&out, commitRequest(&c, stat, paths),
			run.ArtifactsExist, run.FilesNonEmpty); err != nil {
			return "", err
		}
		message = *out.Message
		if err := workspace.Commit(r.Repo, message, paths); err != nil {
			return "", err
		}
		// lathe ran the commit, so all that is left to establish is that git did
		// what it was asked: HEAD moved, and nothing was left behind.
		if sha, err = workspace.Head(r.Repo); err != nil {
			return "", err
		}
		if sha == before {
			return "", fmt.Errorf("git commit left HEAD at %s", before)
		}
		// Recorded here rather than at the end, so a failed pull request still
		// leaves a run that names where its commit is.
		if err := r.Shipped(branch, sha); err != nil {
			return "", err
		}
		return "", permit.Clean(r.Repo)
	})

	g.Add(run.Node{Name: "pr", Owner: "pr-author"}, func(e *run.Entry) (string, error) {
		// lathe pushes, so gh pr create never has to and the deny list that puts
		// `git push` out of every agent's reach stays intact. It happens before
		// the turn: a push that fails should not cost one.
		if err := workspace.Push(r.Repo, branch); err != nil {
			return "", err
		}
		var out PROutput
		if err := e.Call(&out, prRequest(&c, branch, message),
			run.ArtifactsExist, run.FilesNonEmpty); err != nil {
			return "", err
		}
		draft := !c.v.Accepted()
		body := *out.Body
		if draft {
			body += validationReport(&c.v)
		}
		created, err := workspace.CreatePR(r.Repo, *out.Title, body, draft)
		if err != nil {
			return "", err
		}
		url = created
		return "", writeResult(r.Dir, "pr.json", prResult{
			PROutput: &out, URL: url, Draft: draft, Validation: c.v.Outcome})
	})

	err := g.Run()
	if err == nil {
		fmt.Fprintf(r.Out, "\n%s\n\n  %s @ %s\n  %s\n", *c.out.Summary, branch, short(sha), url)
		if !c.v.Accepted() {
			printOutcome(r.Out, &c.v)
			return r.Finish(false, fmt.Sprintf("validation %s: %s; opened draft pull request %s with the unaccepted change",
				c.v.Outcome, c.v.Reason, url))
		}
		return r.Finish(true, "")
	}
	reason := err.Error()
	if sha != "" {
		reason += fmt.Sprintf(" (the change is committed on %s as %s)", branch, short(sha))
	}
	return r.Finish(false, reason)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
