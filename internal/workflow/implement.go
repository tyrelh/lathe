package workflow

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/run"
)

// ImplementOutput describes the work left in the tree and any missing permission.
type ImplementOutput struct {
	Summary *string   `json:"summary"`
	Changed *[]string `json:"changed"`
	Needed  *[]string `json:"needed"`
	Wrote   *[]string `json:"artifacts"`
}

func (b *ImplementOutput) Validate() []string {
	var v []string
	if b.Summary == nil || strings.TrimSpace(*b.Summary) == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if b.Changed == nil {
		v = append(v, `"changed" is missing; use [] if nothing changed`)
	}
	if b.Needed == nil {
		v = append(v, `"needed" is missing; use [] if the plan allowed every file you needed`)
	}
	if b.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no nonempty files`)
	}
	return v
}

func (b *ImplementOutput) Artifacts() []string {
	if b.Wrote == nil {
		return nil
	}
	return *b.Wrote
}

// Failure makes ImplementOutput a run.Failing: a missing permission is terminal
// even when the rest of the report needs correction, because correcting it would
// turn a plan that was never sufficient into an apparently successful run.
func (b *ImplementOutput) Failure() error {
	if b.Needed != nil && len(*b.Needed) > 0 {
		return fmt.Errorf("plan missed files the builder needs: %s", strings.Join(*b.Needed, ", "))
	}
	return nil
}

// ChangesMatchClaim compares both directions against Git, not the plan: files
// permitted to change are not necessarily files the builder actually changed.
func ChangesMatchClaim(e run.Envelope, r *run.Run) []string {
	b, ok := e.(*ImplementOutput)
	if !ok || b.Changed == nil {
		return nil
	}
	paths, err := permit.Changed(r.Repo)
	if err != nil {
		return []string{err.Error()}
	}
	// One map holds both directions: a key is a path git reports, and its value
	// is whether the builder claimed it. Marking on the accepting branch only is
	// what keeps "claimed" from also meaning "rejected as invented".
	claimed := make(map[string]bool, len(paths))
	for _, p := range paths {
		claimed[p] = false
	}
	var v []string
	for _, p := range *b.Changed {
		clean := permit.Norm(p)
		if _, reported := claimed[clean]; !reported || permit.Escapes(p) {
			v = append(v, fmt.Sprintf("you listed %q in changed, but git reports no such repo-relative change", p))
			continue
		}
		claimed[clean] = true
	}
	for _, p := range paths {
		if !claimed[p] {
			v = append(v, fmt.Sprintf("git reports %q changed, but you omitted it from changed", p))
		}
	}
	return v
}

// implementRequest is the handoff: the request, plus the plan the builder is
// held to, with any amendments the adjudicator has made to it since. It renders
// through the same section helper printPlan uses, so what the builder is told
// and what the engineer was shown cannot drift apart. Every list is non-nil
// because the plan phase's Validate has already accepted it.
func implementRequest(request string, plan *PlanOutput, amendments []Amendment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Request\n\n%s\n\n## Accepted plan\n\n%s\n", request, *plan.Summary)
	section(&b, "### Steps", *plan.Steps)
	section(&b, "### Allowed files", *plan.Files)
	section(&b, "### Risks", *plan.Risks)
	var lines []string
	for _, a := range amendments {
		lines = append(lines, a.Change+" (because "+a.Reason+")")
	}
	section(&b, "### Plan amendments", lines)
	return b.String()
}

// maxRepairs is how many times the adjudicator may send an implementation back.
// Four repairs after the initial implementation bound the cost of work that
// will not converge; the fourth repair still gets a complete validation round.
// The planning loop keeps its own budget.
const maxRepairs = 4

// code is what the implement, validate and adjudicate nodes accumulate. Build's
// git phases read it after those nodes forward: the handoff and the
// implementation summary are what a commit message and a pull request body are
// written from, and neither is recoverable from the tree. v says whether what
// they ship was accepted.
type code struct {
	plan     PlanOutput
	out      ImplementOutput
	handoff  string
	feedback string
	v        Validation
}

// addRequestNode opens a writing workflow. The request is a phase so the trace
// starts with what was asked, in the same table as everything that followed
// from it, and the checkout is refused here if it is not clean.
func addRequestNode(g *run.Graph, r *run.Run) {
	g.Add(run.Node{Name: "request", Owner: "engineer"}, func(e *run.Entry) (string, error) {
		if err := e.Log("request", r.Request); err != nil {
			return "", err
		}
		return "", r.EnsureClean()
	})
}

// addCodeNodes appends the implement, validate and adjudicate nodes both
// writing workflows share. They are shared rather than repeated for the same
// reason addPlanNodes is: the graph decides what comes before and after them,
// so nothing here has to know whether a branch was created first or a commit
// follows.
//
// They forward on every outcome that leaves something to hand off — accepted,
// unresolved at the send-back limit, or incomplete because a worker failed —
// and c.v.Outcome says which. They end the run only on what nothing should be
// published from: an error, cancellation, or source that changed under review.
func addCodeNodes(g *run.Graph, r *run.Run, c *code) {
	g.Add(run.Node{Name: "implement", Owner: "builder"}, func(e *run.Entry) (string, error) {
		request := c.feedback
		if e.Round == 0 {
			// The plan is settled by the time anything reaches here: review only
			// ever sends back to the planner, never past this node.
			c.handoff = implementRequest(r.Request, &c.plan, nil)
			request = c.handoff
		}
		e.Scope(*c.plan.Files)
		err := e.Call(&c.out, request, run.ArtifactsExist, run.FilesNonEmpty, ChangesMatchClaim)
		// Keep a terminal needed report alongside the plan for diagnosis.
		if err == nil || c.out.Failure() != nil {
			if saveErr := writeResult(r.Dir, "implement.json", &c.out); saveErr != nil {
				return "", saveErr
			}
		}
		if err == nil {
			c.v.Rounds = append(c.v.Rounds, newRound(e.Round))
		}
		return "", err
	})

	// Discovery is a claim; green and red are a measurement. The tester
	// reassesses the command every round, but only the exit code from Command
	// is evidence: a tester that could report the suite green could end a run
	// green by saying so.
	workers := []run.Worker{{Name: "test", Owner: "tester", Run: func(e *run.Entry) error {
		cur := c.v.current()
		var out TestOutput
		if err := e.Call(&out, testRequest(c)); err != nil {
			return err
		}
		cur.Tests = &out
		tail, err := e.Command(*out.Command)
		var red *run.CommandFailure
		switch {
		case err == nil:
			cur.Measured = &Measured{Command: *out.Command, Green: true, Result: "passed", Tail: tail}
		case errors.As(err, &red):
			// A red suite is a result for the adjudicator, not a failed worker.
			cur.Measured = &Measured{Command: *out.Command, Result: err.Error(), Tail: tail}
			e.Failed(err)
		default:
			return err // denied or timed out: no measurement, so retry
		}
		return nil
	}}}
	for _, name := range reviewers {
		workers = append(workers, run.Worker{Name: name, Owner: name, Run: func(e *run.Entry) error {
			report := c.v.current().Reports[name]
			if err := e.Call(report, reviewRequest(c), run.ArtifactsExist, run.FilesNonEmpty); err != nil {
				return err
			}
			report.stamp(name, e.Round)
			return nil
		}})
	}

	g.AddGroup(run.Node{Name: "validate", Owner: "engineer"}, workers, func(e *run.Entry, res run.GroupResult) (string, error) {
		cur := c.v.current()
		for _, name := range res.FailedNames() {
			cur.Failures = append(cur.Failures, WorkerFailure{Worker: name, Error: res.Failed[name].Error()})
			// Whatever a failed reviewer decoded is not a report.
			delete(cur.Reports, name)
		}
		cur.Mutated = res.Mutated
		if len(res.Mutated) > 0 {
			reason := "source changed while validation ran, so this round cannot approve the implementation: " +
				strings.Join(res.Mutated, ", ")
			c.v.settle(outcomeInvalidated, reason)
			if err := writeResult(r.Dir, "validation.json", &c.v); err != nil {
				return "", err
			}
			return "", errors.New(reason)
		}
		return "", writeResult(r.Dir, "validation.json", &c.v)
	})

	g.Add(run.Node{Name: "adjudicate", Owner: "adjudicator", SendBacks: maxRepairs}, func(e *run.Entry) (string, error) {
		cur := c.v.current()
		// Acceptance needs a usable report from every worker, so a round with a
		// missing one is handed off as it stands, with whatever evidence it has.
		if len(cur.Failures) > 0 {
			var names []string
			for _, f := range cur.Failures {
				names = append(names, f.Worker)
			}
			c.v.settle(outcomeIncomplete, "validation incomplete: "+strings.Join(names, ", ")+" failed after retries")
			return "", writeResult(r.Dir, "validation.json", &c.v)
		}

		findings := cur.findings()
		var out AdjudicationOutput
		err := e.Call(&out, adjudicationRequest(c, r.Request, e.Round+1, e.SendBacksLeft),
			run.ArtifactsExist, run.FilesNonEmpty, Adjudicated(findings, cur.Measured, r.Protected()))
		if err != nil {
			return "", err
		}
		cur.Adjudication = &out

		if *out.Verdict == verdictAccept {
			c.v.settle(outcomeAccepted, "")
			return "", writeResult(r.Dir, "validation.json", &c.v)
		}
		fixes := out.fixes(findings)
		if e.SendBacksLeft == 0 {
			c.v.Unresolved = fixes
			reason := fmt.Sprintf("%d send-backs spent; %d findings unresolved", maxRepairs, len(fixes))
			if !cur.Measured.Green {
				reason += "; " + cur.Measured.Result
			}
			c.v.settle(outcomeUnresolved, reason)
			return "", writeResult(r.Dir, "validation.json", &c.v)
		}
		c.amend(r.Request, &out)
		c.feedback = repairRequest(c, r.Request, &out, fixes, maxRepairs-e.SendBacksLeft+1)
		if err := writeResult(r.Dir, "validation.json", &c.v); err != nil {
			return "", err
		}
		return "implement", nil
	})
}

// printOutcome tells the engineer how validation ended when it did not accept.
func printOutcome(w io.Writer, v *Validation) {
	fmt.Fprintf(w, "\nvalidation %s: %s\n", v.Outcome, v.Reason)
	for _, f := range v.Unresolved {
		writeFinding(w, f)
	}
}

// Implement plans, implements and validates a change. Reports live in the run
// directory; the implementation stays in the target working tree for the user
// to review, whether or not it was accepted. Implement never commits, pushes or
// opens anything: an unaccepted change stays local. Build is this run plus the
// three git phases that ship it.
func Implement(r *run.Run) int {
	var c code

	g := run.NewGraph(r)
	addRequestNode(g, r)
	addPlanNodes(g, r, &c.plan)
	addCodeNodes(g, r, &c)

	if err := g.Run(); err != nil {
		return r.Finish(false, err.Error())
	}
	fmt.Fprintf(r.Out, "\n%s\n", *c.out.Summary)
	section(r.Out, "changed (uncommitted)", *c.out.Changed)
	if !c.v.Accepted() {
		printOutcome(r.Out, &c.v)
		return r.Finish(false, fmt.Sprintf("validation %s: %s; the changes are left uncommitted", c.v.Outcome, c.v.Reason))
	}
	return r.Finish(true, "")
}
