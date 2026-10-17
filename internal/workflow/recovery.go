package workflow

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/workspace"
)

// publication is the already-delivered work a recovery continues, or nil.
func publication(r *run.Run) *run.Publication {
	if r.Recovery == nil {
		return nil
	}
	return r.Recovery.Publication
}

// continuePR publishes a recovered build on its verified branch: a new
// commit is pushed with pub.Head as the expected remote head and verified on
// origin, then the existing pull request is kept, or readied when it is
// lathe's own draft of unaccepted work and the work is now accepted, or a
// pull request is opened under normal build policy when none exists. The
// commit node has already refused unaccepted work for an existing one.
func continuePR(e *run.Entry, r *run.Run, c *code, pub *run.Publication, sha, message string) (string, error) {
	// Immediately before any delivery, including completing with nothing
	// new: origin's branch and the pull request are still as reconciled.
	if _, err := workspace.CheckPublication(r.Repo, pub.Branch, pub.Head, pub.PR); err != nil {
		return "", fmt.Errorf("%w; nothing was published, and the work is %s", err, short(sha))
	}
	if sha != pub.Head {
		// The lease expects pub.Head, which is absence for a branch origin
		// does not have yet.
		pushErr := workspace.PushExpecting(r.Repo, pub.Branch, pub.Head)
		if err := e.Log("push", pushed(pushErr)); err != nil {
			return "", err
		}
		after, err := workspace.RemoteHead(r.Repo, pub.Branch)
		if err != nil {
			return "", fmt.Errorf("publication is uncertain: %s, and origin could not be read afterwards (%v); the commit is %s", pushed(pushErr), err, short(sha))
		}
		if after != sha {
			return "", fmt.Errorf("not published: %s, and origin/%s is at %s; the commit is %s", pushed(pushErr), pub.Branch, short(after), short(sha))
		}
		if err := r.Shipped(pub.Branch, sha); err != nil {
			return "", err
		}
	}
	if pub.PR == "" {
		return openPR(e, r, c, pub.Branch, message)
	}
	pr, err := workspace.ViewPR(r.Repo, pub.PR)
	if err != nil {
		return "", fmt.Errorf("published %s to %s, but the pull request could not be read afterwards: %w", short(sha), pub.Branch, err)
	}
	// GitHub may not have caught up with the push yet, so only the state is
	// rechecked here; ownership was checked immediately before.
	if pr.State != "OPEN" {
		return "", fmt.Errorf("published %s to %s, but pull request %s is now %s", short(sha), pub.Branch, pub.PR, strings.ToLower(pr.State))
	}
	draft := pub.Draft
	switch {
	case draft && pub.LatheDraft:
		if err := workspace.ReadyPR(r.Repo, pub.PR); err != nil {
			return "", err
		}
		draft = false
		if err := e.Log("pr", "marked lathe's draft ready: the work is now accepted"); err != nil {
			return "", err
		}
	case draft:
		if err := e.Log("pr", "left as a draft: lathe has no record of opening it for unaccepted work"); err != nil {
			return "", err
		}
	}
	return pub.PR, writeResult(r.Dir, "pr.json", prResult{URL: pub.PR, Draft: draft, Validation: c.v.Outcome})
}

// A recovery is an engineer continuing a failed, cancelled or lost iteration
// with guidance. It runs the original workflow as a new iteration: planning
// starts fresh with the earlier iterations' evidence, and every allowance is
// the iteration's own.

// recoveryContext is what the first planner or scout turn of a recovery is
// told beyond the original request: the guidance, how the source iteration
// ended, its prior reports, and the work restored into the tree. It is "" for
// any other iteration.
func recoveryContext(r *run.Run, c *code) string {
	rec := r.Recovery
	if rec == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n## Recovery of iteration %d\n\nAn engineer resumed this run after iteration %d ended (%s). "+
		"The request above is still the goal; the guidance may clarify it, add constraints or change the approach, but not start a different feature. "+
		"Reports below are prior: they describe earlier iterations, not this one.\n",
		rec.Source, rec.Source, rec.Failure)
	fmt.Fprintf(&b, "\n### Engineer guidance\n\n%s\n", rec.Guidance)

	var plan PlanOutput
	if latest(rec.Prior, "plan.json", &plan) != "" && plan.Printable() {
		b.WriteString("\n### Prior plan\n")
		printPlan(&b, &plan)
	}
	var impl ImplementOutput
	if latest(rec.Prior, "implement.json", &impl) != "" && impl.Summary != nil {
		fmt.Fprintf(&b, "\n### Prior implementation report\n\n%s\n", *impl.Summary)
		if impl.Needed != nil {
			section(&b, "Files the builder said it needed", *impl.Needed)
		}
		if impl.Explanation != nil && *impl.Explanation != "" {
			fmt.Fprintf(&b, "\nWhy: %s\n", *impl.Explanation)
		}
	}
	if n := len(rec.Prior); n > 0 {
		dir := rec.Prior[n-1].Dir
		for i := maxScopeSendBacks + 1; i >= 1; i-- {
			var req ScopeRequest
			if latest([]run.Prior{{Dir: dir}}, fmt.Sprintf("scope-%d.json", i), &req) != "" {
				fmt.Fprintf(&b, "\n### Prior scope request %d (%s)\n", req.N, req.Outcome)
				section(&b, "Needed", req.Needed)
				if req.Explanation != "" {
					fmt.Fprintf(&b, "\nWhy: %s\n", req.Explanation)
				}
				break
			}
		}
	}
	// A validation report saved mid-repair has rounds and decisions but no
	// summary, which only settling writes; its evidence still counts.
	var v Validation
	dir := latest(rec.Prior, "validation.json", &v)
	if dir != "" && (v.Summary != "" || len(v.Rounds) > 0) {
		source := priorIteration(rec.Prior, dir)
		b.WriteString("\n### Prior validation\n\n")
		if v.Summary != "" {
			fmt.Fprintf(&b, "%s\n", v.Summary)
		} else {
			fmt.Fprintf(&b, "Unsettled after %d round(s).\n", len(v.Rounds))
			if a := v.current().Adjudication; a != nil && a.Summary != nil {
				fmt.Fprintf(&b, "\nLast adjudication (%s): %s\n", *a.Verdict, *a.Summary)
				if a.Changes != nil && *a.Changes != "" {
					fmt.Fprintf(&b, "\nRequested changes: %s\n", *a.Changes)
				}
			}
		}
		dismissed := dismissalsOf(rec.Prior, &v, source)
		section(&b, "Findings a revised plan dismissed", dismissalLines(dismissed))
		if c != nil {
			c.outstanding = outstandingOf(&v, dismissed)
			for i := range c.outstanding {
				c.outstanding[i].From = &source
			}
		}
	}
	if c != nil && len(c.outstanding) > 0 {
		findingSection(&b, "### Outstanding findings from prior validation", c.outstanding)
		b.WriteString("\nCarry these forward, or dismiss in `dismissals` any the new approach makes moot.\n")
	}
	section(&b, "### Work restored into the tree from the prior iteration", rec.Restored)
	if rec.Unavailable != "" {
		fmt.Fprintf(&b, "\n### Work not restored\n\n%s\n", rec.Unavailable)
	}
	if len(rec.Restored) > 0 {
		b.WriteString("\nEvery restored path must stay in `files` or be abandoned in `reversals`.\n")
	}
	return b.String()
}

// priorIteration is the iteration whose reports are in dir.
func priorIteration(prior []run.Prior, dir string) int {
	for _, p := range prior {
		if p.Dir == dir {
			return p.Iteration
		}
	}
	return -1
}

// dismissalsOf is every recorded dismissal of a finding raised by v, the
// validation report of iteration source. References repeat across
// iterations, so a dismissal counts only where it names its finding's
// iteration: in v's own report, one with no origin dismissed a finding of
// that iteration's own scope cycle; any other must name source. A later
// iteration's validation or disposition may dismiss v's findings the same
// way. Earlier iterations' dismissals never apply.
func dismissalsOf(prior []run.Prior, v *Validation, source int) []Dismissal {
	var out []Dismissal
	for _, d := range v.Dismissals {
		if (d.From == nil && d.Request > 0) || (d.From != nil && *d.From == source) {
			out = append(out, d)
		}
	}
	for _, p := range prior {
		if p.Iteration <= source {
			continue
		}
		for _, dm := range dismissalsIn(p) {
			if dm.From != nil && *dm.From == source {
				out = append(out, dm)
			}
		}
	}
	return out
}

// dismissalsIn is every dismissal iteration p recorded: its complete
// dismissals.json when that decodes, even empty, and otherwise, for older
// iterations, those in its validation.json and disposition.json.
func dismissalsIn(p run.Prior) []Dismissal {
	all := []Dismissal{}
	if latest([]run.Prior{p}, "dismissals.json", &all) != "" {
		return all
	}
	var out []Dismissal
	var v Validation
	if latest([]run.Prior{p}, "validation.json", &v) != "" {
		out = append(out, v.Dismissals...)
	}
	var d Disposition
	if latest([]run.Prior{p}, "disposition.json", &d) != "" {
		for _, dm := range d.Dismissed {
			if !slices.ContainsFunc(out, func(x Dismissal) bool { return sameDismissal(x, dm) }) {
				out = append(out, dm)
			}
		}
	}
	return out
}

// sameDismissal reports whether a and b record the same decision.
func sameDismissal(a, b Dismissal) bool {
	if a.Ref != b.Ref || a.Request != b.Request || (a.From == nil) != (b.From == nil) {
		return false
	}
	return a.From == nil || *a.From == *b.From
}

// outstandingOf is the findings the last adjudication of v asked to fix and
// that were never accepted away, less those dismissed.
func outstandingOf(v *Validation, dismissals []Dismissal) []Finding {
	if v.Accepted() || len(v.Rounds) == 0 {
		return nil
	}
	findings := v.Unresolved
	if len(findings) == 0 {
		// The latest adjudicated round: a later round may have stopped
		// before its adjudication.
		for i := len(v.Rounds) - 1; i >= 0; i-- {
			if a := v.Rounds[i].Adjudication; a != nil && a.Decisions != nil {
				findings = a.fixes(v.Rounds[i].findings())
				break
			}
		}
	}
	dismissed := map[string]bool{}
	for _, d := range dismissals {
		dismissed[d.Ref] = true
	}
	var out []Finding
	for _, f := range findings {
		if !dismissed[f.Ref] {
			out = append(out, f)
		}
	}
	return out
}

