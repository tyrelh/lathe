package workflow

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/run"
)

// PlanOutput is what the planner must return. Pointers for the same reason
// ScoutOutput uses them: encoding/json cannot otherwise tell an absent key from
// an empty one.
//
// Files is the load-bearing key. It is the builder's write scope, so this
// struct and the `## The file list` block of seed/agents/planner/user.md are one
// contract in two files.
//
// Reversals, Dismissals and Tickets are optional, so a plan from an edited
// prompt that omits them stays valid; an omitted list authorizes nothing.
type PlanOutput struct {
	Summary *string   `json:"summary"`
	Steps   *[]string `json:"steps"`
	Files   *[]string `json:"files"`
	Risks   *[]string `json:"risks"`
	Wrote   *[]string `json:"artifacts"`

	Reversals  *[]Reversal  `json:"reversals,omitempty"`
	Dismissals *[]Dismissal `json:"dismissals,omitempty"`
	Tickets    *[]Ticket    `json:"future_tickets,omitempty"`
}

// Validate returns what is missing, phrased as the agent will read it. An empty
// files list is a violation rather than an empty scope: a plan that permits no
// writes is a plan nothing can be built from.
func (p *PlanOutput) Validate() []string { return p.validate(false) }

// validate is Validate with the one rule a revision relaxes: a revision may be
// answered without any code change.
func (p *PlanOutput) validate(noFiles bool) []string {
	var v []string
	if p.Summary == nil || *p.Summary == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if p.Steps == nil || len(*p.Steps) == 0 {
		v = append(v, `"steps" is missing or empty; a plan needs at least one step`)
	}
	switch {
	case p.Files == nil:
		v = append(v, `"files" is missing`)
	case len(*p.Files) == 0 && !noFiles:
		v = append(v, `"files" is empty; it is the only list of files the builder may write, so an empty one leaves nothing to build`)
	}
	if p.Risks == nil {
		v = append(v, `"risks" is missing; use [] if you see none`)
	}
	if p.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	if p.Reversals != nil {
		for _, r := range *p.Reversals {
			if strings.TrimSpace(r.Path) == "" || strings.TrimSpace(r.Reason) == "" {
				v = append(v, `every reversal needs a "path" and a "reason"`)
			}
		}
	}
	if p.Dismissals != nil {
		for _, d := range *p.Dismissals {
			if strings.TrimSpace(d.Ref) == "" || strings.TrimSpace(d.Reason) == "" {
				v = append(v, `every dismissal needs a "ref" and a "reason"`)
			}
		}
	}
	if p.Tickets != nil {
		for _, t := range *p.Tickets {
			if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Reason) == "" {
				v = append(v, `every future ticket needs a "title" and a "reason"`)
			}
		}
	}
	return v
}

// Artifacts is what the gates check. A planner writes nothing, so this is
// normally empty; `files` is a permission, not a claim, and is gated separately.
func (p *PlanOutput) Artifacts() []string {
	if p.Wrote == nil {
		return nil
	}
	return *p.Wrote
}

// revisionPlan is a revision's plan. It may permit no files at all: a request
// that needs no code change is answered by validating the branch as it stands
// and committing nothing. Everything else about it is PlanOutput.
type revisionPlan PlanOutput

func (p *revisionPlan) Validate() []string  { return (*PlanOutput)(p).validate(true) }
func (p *revisionPlan) Artifacts() []string { return (*PlanOutput)(p).Artifacts() }

// planOf is the PlanOutput inside a plan or revision plan envelope.
func planOf(e run.Envelope) (*PlanOutput, bool) {
	if rp, ok := e.(*revisionPlan); ok {
		return (*PlanOutput)(rp), true
	}
	p, ok := e.(*PlanOutput)
	return p, ok
}

// FilesPermitted rejects a plan that would hand a builder a path it must never
// have. The check belongs here rather than at the first blocked write: a plan
// naming .env costs one planner turn to reject and a whole run to discover.
func FilesPermitted(protected []string) run.Gate {
	return func(e run.Envelope, _ *run.Run) []string {
		p, ok := planOf(e)
		if !ok || p.Files == nil {
			return nil
		}
		var v []string
		for _, f := range *p.Files {
			if problem := pathProblem(f, "files", protected); problem != "" {
				v = append(v, problem)
			}
		}
		return v
	}
}

// pathProblem is why one path may not join a write scope, or "" if it may.
// key names the list it came from, so the agent is told which of its lists to fix.
func pathProblem(f, key string, protected []string) string {
	switch {
	case permit.Escapes(f):
		return fmt.Sprintf(`%q is not inside the repository; every path in %q is repo-relative`, f, key)
	case globby(f):
		return fmt.Sprintf(`%q looks like a glob; every path in %q is one exact file`, f, key)
	}
	if pattern, denied := permit.Denied(f, protected); denied {
		return fmt.Sprintf(`%q is protected (it matches %s) and may never be written; plan the change without it`, f, pattern)
	}
	return ""
}

// globby is why the permission check downstream can be string equality rather
// than a glob engine. Brackets are not on it: they are real filenames in the
// file-routed frameworks (Next.js pages/blog/[slug].tsx, SvelteKit, Remix), and
// every allow check is equality, so a bracketed path only ever means itself.
// The git that takes paths runs with --literal-pathspecs for the same reason.
func globby(path string) bool {
	for _, c := range path {
		if c == '*' || c == '?' {
			return true
		}
	}
	return false
}

// Plan reads and reviews the plan a builder will implement. Build hands the
// same reviewed envelope to the builder.
func Plan(r *run.Run) int {
	var out PlanOutput

	g := run.NewGraph(r)
	g.Add(run.Node{Name: "request", Owner: "engineer"},
		func(e *run.Entry) (string, error) { return "", e.Log("request", r.Request) })
	addPlanNodes(g, r, &out, "", nil)

	err := g.Run()
	// The plan is the product of this workflow, so it goes to the terminal as
	// well as to disk; a failed phase has nothing to print.
	if err == nil {
		printPlan(r.Out, &out)
	}
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	return r.Finish(err == nil, reason)
}

// maxReviewSendBacks is how many times plan review may return a plan to the
// planner per iteration, including reviews of plans revised for a scope
// request; the budget is not reset by a scope request. The review after the
// last forwards remaining objections to the builder as risks.
const maxReviewSendBacks = 8

// addPlanNodes appends auth, optional issue lookup, planner routing when
// configured, planning and review.
// A nonempty brief is a revision's planner brief and permits an empty file list.
// c is a writing workflow's state, or nil for a plan-only run; with it, the
// plan must keep or abandon work in the tree and may answer scope requests.
func addPlanNodes(g *run.Graph, r *run.Run, out *PlanOutput, brief string, c *code) {
	g.Add(run.Node{Name: "auth", Owner: "engineer"}, func(e *run.Entry) (string, error) {
		return "", e.AuthPreflight()
	})
	if r.Issue != "" {
		g.Add(run.Node{Name: "issue", Owner: "engineer"}, func(e *run.Entry) (string, error) {
			request, err := e.IssueRequest()
			if err != nil {
				return "", err
			}
			r.Request = request
			return "", r.Resolve(request)
		})
	}
	// plannerRequest reads r.Request on each call because the issue node may
	// rewrite it.
	plannerRequest := func() string {
		if brief != "" {
			return brief
		}
		return r.Request
	}
	// After issue, which rewrites r.Request. Nothing sends back here, so the
	// planner's tier holds for the whole iteration.
	if r.Routes("planner") {
		g.Add(run.Node{Name: "route-plan", Owner: "engineer"}, func(e *run.Entry) (string, error) {
			return "", e.Route("planner", PlanRouteState(plannerRequest()))
		})
	}
	// The reviewer's objections, which of them block the plan outright, and
	// whether they are a real review or the reviewer having returned nothing
	// usable. All three are read by the plan node on its way back round.
	var feedback, blocking []string
	var unusable bool
	// reviewed is set when review sends the plan back, so the planner's next
	// entry is a revision against that feedback rather than a fresh brief.
	var reviewed bool
	// cycle counts reviews since planning last started: at the first plan and
	// again at each scope request. The send-back budget is not reset with it.
	cycle := 0
	// planned is set after the first planner entry, which alone carries a
	// recovery's context.
	var planned bool

	var plan run.Envelope = out
	if brief != "" {
		plan = (*revisionPlan)(out)
	}
	g.Add(run.Node{Name: "plan", Owner: "planner"}, func(e *run.Entry) (string, error) {
		request := plannerRequest()
		if !planned {
			request += recoveryContext(r, c)
			planned = true
		}
		gates := []run.Gate{run.ArtifactsExist, run.FilesNonEmpty, FilesPermitted(r.Protected())}
		if c != nil {
			// Work already in the tree must be kept or explicitly abandoned, and
			// only findings still outstanding may be dismissed.
			dirty, err := permit.Changed(r.Repo)
			if err != nil {
				return "", err
			}
			gates = append(gates, Retained(dirty), Dismissible(c.outstanding))
			if c.scope.brief {
				request = scopeBrief(c, r, dirty)
				c.scope.brief, reviewed, cycle = false, false, 0
			}
		}
		if reviewed {
			var resubmit strings.Builder
			resubmit.WriteString("Revise the plan using the review feedback below. Return the complete plan.\n")
			printPlan(&resubmit, out)
			section(&resubmit, "Blocking objections: the plan cannot succeed until these are resolved", blocking)
			section(&resubmit, "Review feedback", feedback)
			if unusable {
				resubmit.WriteString("\nThe reviewer returned nothing usable. You may reply with the same plan unchanged.\n")
			}
			if c != nil && c.scope.pending != nil {
				resubmit.WriteString("\nThe builder's scope request above still stands; the plan must still answer it.\n")
			}
			request = resubmit.String()
			reviewed = false
		}
		return "", e.Call(plan, request, gates...)
	})

	g.Add(run.Node{Name: "review", Owner: "plan-reviewer", SendBacks: maxReviewSendBacks}, func(e *run.Entry) (string, error) {
		cycle++
		round := cycle
		var message strings.Builder
		fmt.Fprintf(&message, "Review round %d of %d.\nYou may send this plan back %d more times.\n",
			round, round+e.SendBacksLeft, e.SendBacksLeft)
		if round > 1 {
			message.WriteString("\nThe planner returned the following complete plan.\n")
		}
		fmt.Fprintf(&message, "\n## Request\n\n%s\n", r.Request)
		if r.Recovery != nil {
			fmt.Fprintf(&message, "\n## Recovery guidance (iteration %d ended: %s)\n\n%s\n", r.Recovery.Source, r.Recovery.Failure, r.Recovery.Guidance)
			section(&message, "Work restored into the tree", r.Recovery.Restored)
		}
		printPlan(&message, out)
		if c != nil && c.scope.pending != nil {
			scopeReview(&message, c)
		}

		var report ReviewOutput
		callErr := e.Call(&report, message.String(), run.ArtifactsExist, run.FilesNonEmpty)
		var unusableReport *run.EnvelopeError
		switch {
		case callErr == nil:
			feedback, blocking, unusable = *report.Feedback, *report.Blocking, false
		case !errors.As(callErr, &unusableReport):
			// Cancellation, a provider refusing the request, Pi failing to
			// start: the reviewer never assessed anything, and sending the plan
			// round again would only forward it unreviewed at the limit.
			return "", callErr
		default:
			// A reviewer that said nothing usable is a failed phase inside a run
			// that continues: the planner is told so and gets another round.
			e.Failed(callErr)
			e.ResetSession()
			feedback = []string{"review failed: reviewer returned nothing usable: " + callErr.Error()}
			blocking, unusable = nil, true
		}
		if len(feedback)+len(blocking) > 0 && e.SendBacksLeft > 0 {
			reviewed = true
			return "plan", nil
		}
		if unusable && c != nil && c.scope.pending != nil {
			// No usable review established that the scope request is answered,
			// so the candidate plan is kept for diagnosis and never handed on.
			req := c.scope.pending
			if err := writeResult(r.Dir, fmt.Sprintf("plan-%d.json", req.N), out); err != nil {
				return "", err
			}
			req.Outcome = scopeUnreviewed
			if err := saveScope(r, req); err != nil {
				return "", err
			}
			return "", fmt.Errorf("plan review: no usable review of the plan revised for scope request %d after %d send-backs; the builder's scope request is unresolved and the partial work is kept%s",
				req.N, maxReviewSendBacks, resumeHint(r))
		}
		if unusable && c != nil && r.Recovery != nil && !c.disposed {
			// A recovery's plan decides what happens to restored work and
			// prior findings; without a usable review it decides nothing.
			if err := writeResult(r.Dir, "plan.json", out); err != nil {
				return "", err
			}
			return "", fmt.Errorf("plan review: no usable review of the recovery's plan after %d send-backs; the candidate plan is saved unapplied, and the restored work and prior findings are as they were%s",
				maxReviewSendBacks, resumeHint(r))
		}
		if len(blocking) > 0 {
			// Saved for diagnosis, but never handed on: the reviewer said this
			// plan cannot succeed, and nothing after this node could change that.
			name := "plan.json"
			if c != nil && c.scope.pending != nil {
				// The plan the builder was blocked on stays in plan.json.
				name = fmt.Sprintf("plan-%d.json", c.scope.pending.N)
			}
			if err := writeResult(r.Dir, name, out); err != nil {
				return "", err
			}
			section(r.Out, "blocking objections", blocking)
			err := fmt.Errorf("plan review: the plan cannot succeed as written after %d revisions: %s",
				round-1, strings.Join(blocking, "; "))
			if c != nil && c.scope.pending != nil {
				c.scope.pending.Outcome = scopeBlocked
				if saveErr := saveScope(r, c.scope.pending); saveErr != nil {
					return "", saveErr
				}
				err = fmt.Errorf("%w; the builder's scope request %d is unresolved%s", err, c.scope.pending.N, resumeHint(r))
			}
			return "", err
		}

		for _, objection := range feedback {
			*out.Risks = append(*out.Risks, "unresolved review: "+objection)
		}
		if err := writeResult(r.Dir, "plan.json", out); err != nil {
			return "", err
		}
		if round > 1 {
			if len(feedback) == 0 {
				fmt.Fprintf(r.Out, "plan revised %d times; reviewer accepted\n", round-1)
			} else {
				fmt.Fprintf(r.Out, "plan revised %d times; %d objections unresolved\n", round-1, len(feedback))
			}
		}
		if c != nil && c.scope.pending != nil {
			return c.replanned(e, r)
		}
		if c != nil && r.Recovery != nil && !c.disposed {
			// A recovery's first reviewed plan decides what happens to the
			// restored work and the prior findings.
			c.disposed = true
			d, err := c.dispose(e, r, "abandoned-recovery.patch", 0)
			if err != nil {
				return "", err
			}
			return "", writeResult(r.Dir, "disposition.json", &d)
		}
		return "", nil
	})
}

// Printable reports whether Summary, Steps, Files and Risks are all set, the
// fields printPlan and BuildRouteState dereference.
func (p *PlanOutput) Printable() bool {
	return p.Summary != nil && p.Steps != nil && p.Files != nil && p.Risks != nil
}

// printPlan renders an envelope Validate has already accepted, so every list is
// non-nil by the time it gets here.
func printPlan(w io.Writer, p *PlanOutput) {
	fmt.Fprintf(w, "\n%s\n", *p.Summary)
	section(w, "steps", *p.Steps)
	section(w, "files the builder may write", *p.Files)
	section(w, "risks", *p.Risks)
	var reversals, dismissals, tickets []string
	if p.Reversals != nil {
		for _, r := range *p.Reversals {
			reversals = append(reversals, r.Path+": "+r.Reason)
		}
	}
	if p.Dismissals != nil {
		for _, d := range *p.Dismissals {
			dismissals = append(dismissals, d.Ref+": "+d.Reason)
		}
	}
	if p.Tickets != nil {
		for _, t := range *p.Tickets {
			tickets = append(tickets, t.Title+": "+t.Reason)
		}
	}
	section(w, "work to abandon (saved, then reverted)", reversals)
	section(w, "findings dismissed by the revised plan", dismissals)
	section(w, "proposed future tickets (not part of this run)", tickets)
}

func section(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	for _, item := range items {
		fmt.Fprintf(w, "  - %s\n", item)
	}
}
