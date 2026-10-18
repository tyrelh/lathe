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
// Explanation is optional: why the plan cannot complete the request without
// Needed. The summary stands in for it when absent.
type ImplementOutput struct {
	Summary     *string   `json:"summary"`
	Changed     *[]string `json:"changed"`
	Needed      *[]string `json:"needed"`
	Wrote       *[]string `json:"artifacts"`
	Explanation *string   `json:"explanation,omitempty"`
}

func (b *ImplementOutput) Validate() []string {
	var v []string
	if blank(b.Summary) {
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

// Failure makes ImplementOutput a run.Failing: a nonempty needed list is a
// *ScopeError even when the rest of the report needs correction, because
// correcting it would turn a plan that was never sufficient into an
// apparently successful run.
func (b *ImplementOutput) Failure() error {
	if b.Needed == nil || len(*b.Needed) == 0 {
		return nil
	}
	err := &ScopeError{Needed: append([]string{}, *b.Needed...)}
	switch {
	case b.Explanation != nil && strings.TrimSpace(*b.Explanation) != "":
		err.Explanation = strings.TrimSpace(*b.Explanation)
	case b.Summary != nil:
		err.Explanation = strings.TrimSpace(*b.Summary)
	}
	return err
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

// implementRequest renders the builder's handoff: the request, the accepted
// plan's summary, steps, allowed files and risks, and any amendments. Every
// plan list must be non-nil.
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
// Ten repairs after the initial implementation bound the cost of work that will
// not converge; the last repair still gets a complete validation round. The
// planning loop keeps its own budget.
const maxRepairs = 10

// builderMode is why the builder is entered: the first implementation, an
// adjudicator repair, or a plan revised in answer to a scope request.
type builderMode int

const (
	buildInitial builderMode = iota
	buildRepair
	buildReplanned
)

// code is what the implement, validate and adjudicate nodes accumulate. Build's
// git phases read it after those nodes forward: the handoff and the
// implementation summary are what a commit message and a pull request body are
// written from, and neither is recoverable from the tree. v says whether what
// they ship was accepted.
//
// mode says why the builder is next entered. repair is the builder's request
// when mode is buildRepair. outstanding is the findings the last adjudication
// asked to fix and changes its requested changes, carried across a scope
// send-back.
type code struct {
	plan PlanOutput
	out  ImplementOutput
	// request is the run's resolved request, set when the builder is first
	// entered. handoff is implementRequest of request, plan and amendments, kept
	// current by refreshHandoff whenever plan or amendments change.
	request     string
	handoff     string
	repair      string
	v           Validation
	mode        builderMode
	outstanding []Finding
	changes     string
	scope       scopeLoop
	// disposed is set once a recovery's reviewed plan has been applied.
	disposed bool
}

// patchNote says where a managed worktree's result is saved, or "" for a run
// in the caller's tree.
func patchNote(r *run.Run) string {
	if r.Base() == "" {
		return ""
	}
	return fmt.Sprintf("; lathe saves them as %s/result.patch, to apply from a checkout at %s", r.Dir, short(r.Base()))
}

// resumeHint is how an engineer continues a run that stopped here.
func resumeHint(r *run.Run) string {
	if r.ID == "" {
		return ""
	}
	return fmt.Sprintf("; continue it with: lathe resume %s \"guidance\"", r.ID)
}

// addRequestNode opens a writing workflow. The request is a phase so the trace
// starts with what was asked, in the same table as everything that followed
// from it, and the checkout is refused here if it is not clean.
func addRequestNode(g *run.Graph, r *run.Run) {
	g.Add(run.Node{Name: "request", Owner: "engineer"}, func(e *run.Entry) (string, error) {
		if err := e.Log("request", r.Request); err != nil {
			return "", err
		}
		if r.Recovery != nil {
			if err := e.Log("recovery", fmt.Sprintf("continues iteration %d: %s", r.Recovery.Source, r.Recovery.Guidance)); err != nil {
				return "", err
			}
			// The worktree was verified clean before the prior work was
			// restored and registered as retained.
			if len(r.Recovery.Restored) > 0 {
				return "", e.Log("restored", strings.Join(r.Recovery.Restored, "\n"))
			}
		}
		return "", r.EnsureClean()
	})
}

// addCodeNodes appends builder routing when configured, then the implement,
// validate and adjudicate nodes every writing workflow shares. They are shared
// rather than repeated for the same reason addPlanNodes is: the graph decides
// what comes before and after them, so nothing here has to know whether a
// branch was created first or a commit follows.
//
// They forward on every outcome that leaves something to hand off — accepted,
// unresolved at the send-back limit, or incomplete because a worker failed —
// and c.v.Outcome says which. They end the run only on what nothing should be
// published from: an error, cancellation, or source that changed under review.
func addCodeNodes(g *run.Graph, r *run.Run, c *code) {
	// Repairs send back to implement, never here, so the builder's tier holds
	// for the whole iteration.
	if r.Routes("builder") {
		g.Add(run.Node{Name: "route-build", Owner: "engineer"}, func(e *run.Entry) (string, error) {
			if len(*c.plan.Files) == 0 {
				return "", e.Log("route", "skipped: the plan permits no files, so the builder does not run")
			}
			return "", e.Route("builder", BuildRouteState(r.Request, &c.plan))
		})
	}
	// A scope request sends back to the planner; review then forwards straight
	// here, past branch creation and routing, which are settled for the
	// iteration. Rounds count completed implementations, not entries.
	g.Add(run.Node{Name: "implement", Owner: "builder", SendBacks: maxScopeSendBacks}, func(e *run.Entry) (string, error) {
		c.request = r.Request
		if c.mode != buildRepair {
			c.refreshHandoff()
		}
		var request string
		switch c.mode {
		case buildRepair:
			request = c.repair
		case buildReplanned:
			request = replannedRequest(c)
		case buildInitial:
			request = initialRequest(c, r)
		}
		if c.mode != buildRepair && len(*c.plan.Files) == 0 {
			// Only a revision's plan can permit nothing. A builder that may write
			// nothing has nothing to do, so the branch is validated as it stands.
			summary, none := "No code changes: "+*c.plan.Summary, []string{}
			c.out = ImplementOutput{Summary: &summary, Changed: &none, Needed: &none, Wrote: &none}
			c.v.Rounds = append(c.v.Rounds, newRound(len(c.v.Rounds)))
			return "", writeResult(r.Dir, run.ImplementFile, &c.out)
		}
		e.Scope(*c.plan.Files)
		err := e.Call(&c.out, request, run.ArtifactsExist, run.FilesNonEmpty, ChangesMatchClaim)
		var scope *ScopeError
		if errors.As(err, &scope) {
			return c.blocked(e, r, scope)
		}
		if err != nil {
			return "", err
		}
		if err := writeResult(r.Dir, run.ImplementFile, &c.out); err != nil {
			return "", err
		}
		c.v.Rounds = append(c.v.Rounds, newRound(len(c.v.Rounds)))
		return "", nil
	})

	// The tester reassesses the command every round, runs it and reports its
	// exit code, which is the evidence of green or red.
	workers := []run.Worker{{Name: "test", Owner: "tester", Run: func(e *run.Entry) error {
		cur := c.v.current()
		var out TestOutput
		if err := e.Call(&out, testRequest(c)); err != nil {
			return err
		}
		cur.Tests = &out
		switch {
		case out.Green():
		case out.EnvironmentFailed():
			return &run.EnvironmentError{ExitCode: *out.ExitCode}
		default:
			// A red suite is a result for the adjudicator, not a failed worker.
			e.Failed(&CommandFailure{ExitCode: *out.ExitCode})
		}
		return nil
	}}}
	for _, name := range reviewers {
		workers = append(workers, run.Worker{Name: name, Owner: name, Run: func(e *run.Entry) error {
			cur := c.v.current()
			report := cur.Reports[name]
			if err := e.Call(report, reviewRequest(c), run.ArtifactsExist, run.FilesNonEmpty); err != nil {
				return err
			}
			if len(*report.Findings) > 0 {
				e.RequestChanges()
			}
			report.stamp(name, cur.number())
			return nil
		}})
	}

	g.AddGroup(run.Node{Name: "validate", Owner: "engineer"}, workers, func(e *run.Entry, res run.GroupResult) (string, error) {
		cur := c.v.current()
		var environmentErr error
		for _, name := range res.FailedNames() {
			failed := res.Failed[name]
			cur.Failures = append(cur.Failures, WorkerFailure{Worker: name, Error: failed.Error()})
			var environment *run.EnvironmentError
			if environmentErr == nil && errors.As(failed, &environment) {
				environmentErr = failed
			}
			// Whatever a failed reviewer decoded is not a report.
			delete(cur.Reports, name)
		}
		cur.Mutated = res.Mutated
		if environmentErr != nil {
			if err := c.settle(r.Dir, outcomeEnvironmentFailed, environmentErr.Error()); err != nil {
				return "", err
			}
			return "", environmentErr
		}
		if len(res.Mutated) > 0 {
			reason := "source changed while validation ran, so this round cannot approve the implementation: " +
				strings.Join(res.Mutated, ", ")
			if err := c.settle(r.Dir, outcomeInvalidated, reason); err != nil {
				return "", err
			}
			return "", errors.New(reason)
		}
		return "", writeResult(r.Dir, run.ValidationFile, &c.v)
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
			return "", c.settle(r.Dir, outcomeIncomplete, "validation incomplete: "+strings.Join(names, ", ")+" failed after retries")
		}

		findings := cur.findings()
		var out AdjudicationOutput
		err := e.Call(&out, adjudicationRequest(c, e.SendBacksLeft),
			run.ArtifactsExist, run.FilesNonEmpty, Adjudicated(findings, cur.Tests, r.Protected()))
		if err != nil {
			return "", err
		}
		cur.Adjudication = &out

		if *out.Verdict == verdictAccept {
			c.outstanding, c.changes = nil, ""
			return "", c.settle(r.Dir, outcomeAccepted, "")
		}
		e.RequestChanges()
		fixes := out.fixes(findings)
		if e.SendBacksLeft == 0 {
			c.v.Unresolved = fixes
			reason := fmt.Sprintf("%d send-backs spent; %d findings unresolved", maxRepairs, len(fixes))
			if !cur.Tests.Green() {
				reason += "; " + cur.Tests.Result()
			}
			return "", c.settle(r.Dir, outcomeUnresolved, reason)
		}
		c.amend(&out)
		c.repair = repairRequest(c, &out, fixes, maxRepairs-e.SendBacksLeft+1)
		c.mode, c.outstanding, c.changes = buildRepair, fixes, *out.Changes
		if err := writeResult(r.Dir, run.ValidationFile, &c.v); err != nil {
			return "", err
		}
		return "implement", nil
	})
}

// refreshHandoff rebuilds c.handoff from c.request, c.plan and the amendments.
func (c *code) refreshHandoff() {
	c.handoff = implementRequest(c.request, &c.plan, c.v.Amendments)
}

// initialRequest is the builder's first request: the handoff with a
// recovery's outstanding findings and restored work.
func initialRequest(c *code, r *run.Run) string {
	request := c.handoff
	// A recovery carries prior findings the plan did not dismiss.
	var b strings.Builder
	findingSection(&b, "### Findings from prior validation still to fix", c.outstanding)
	request += b.String()
	if r.Recovery != nil && len(r.Recovery.Restored) > 0 {
		request += "\nThe tree already holds the prior iteration's work on the paths the plan keeps; continue from it.\n"
	}
	return request
}

// settle settles validation with outcome and reason and writes validation.json
// to dir.
func (c *code) settle(dir, outcome, reason string) error {
	c.v.settle(outcome, reason)
	return writeResult(dir, run.ValidationFile, &c.v)
}

// printOutcome tells the engineer how validation ended when it did not accept.
func printOutcome(w io.Writer, v *Validation) {
	fmt.Fprintf(w, "\n%s\n", v.verdict())
	for _, f := range v.Unresolved {
		writeFinding(w, f)
	}
}

// Implement plans, implements and validates a change. Reports live in the run
// directory. The implementation is left uncommitted in the tree it ran in,
// accepted or not. Implement never commits, pushes or opens anything.
func Implement(r *run.Run) int {
	var c code

	g := run.NewGraph(r)
	addRequestNode(g, r)
	addPlanNodes(g, r, &c.plan, "", &c)
	addCodeNodes(g, r, &c)

	if err := g.Run(); err != nil {
		return r.Finish(false, err.Error())
	}
	fmt.Fprintf(r.Out, "\n%s\n", *c.out.Summary)
	section(r.Out, "changed (uncommitted)", *c.out.Changed)
	if note := patchNote(r); note != "" {
		fmt.Fprintf(r.Out, "\n %s\n", strings.TrimPrefix(note, "; "))
	}
	if !c.v.Accepted() {
		printOutcome(r.Out, &c.v)
		return r.Finish(false, fmt.Sprintf("%s; the changes are left uncommitted and unaccepted%s", c.v.verdict(), patchNote(r)))
	}
	return r.Finish(true, "")
}
