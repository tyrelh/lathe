package workflow

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tyrelh/lathe/internal/run"
)

// The validation group runs the tester and three code reviewers against the
// same implementation at once, and the adjudicator judges what they found. The
// types below are that contract: what a reviewer returns, what the adjudicator
// decides, and the record of every round that validation.json keeps.

// The code reviewers, in the order their reports are shown. Their agent names
// are also their finding sources.
var reviewers = []string{"code-review-general", "code-review-security", "code-review-slop"}

// Finding is one reviewer's objection. Ref and Source are lathe's, assigned once
// the report is accepted, so a reference is unique across reviewers and never
// changes in a later round. Location is optional: not every finding has one.
type Finding struct {
	Ref         string `json:"ref"`
	Source      string `json:"source"`
	Location    string `json:"location"`
	Evidence    string `json:"evidence"`
	Explanation string `json:"explanation"`
	Outcome     string `json:"outcome"`
}

// CodeReviewOutput is what each code reviewer returns. An empty findings list
// approves the implementation from that reviewer's angle.
type CodeReviewOutput struct {
	Summary  *string    `json:"summary"`
	Findings *[]Finding `json:"findings"`
	Wrote    *[]string  `json:"artifacts"`
}

func (c *CodeReviewOutput) Validate() []string {
	var v []string
	if c.Summary == nil || strings.TrimSpace(*c.Summary) == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if c.Findings == nil {
		v = append(v, `"findings" is missing; use [] to approve`)
	} else {
		for i, f := range *c.Findings {
			for _, field := range [][2]string{{"evidence", f.Evidence}, {"explanation", f.Explanation}, {"outcome", f.Outcome}} {
				if strings.TrimSpace(field[1]) == "" {
					v = append(v, fmt.Sprintf(`finding %d has no %q`, i+1, field[0]))
				}
			}
		}
	}
	if c.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	return v
}

func (c *CodeReviewOutput) Artifacts() []string {
	if c.Wrote == nil {
		return nil
	}
	return *c.Wrote
}

// stamp gives every finding its source and a reference of the form
// security-2.1: reviewer, implementation round counted from one, position.
func (c *CodeReviewOutput) stamp(source string, round int) {
	short := strings.TrimPrefix(source, "code-review-")
	for i := range *c.Findings {
		f := &(*c.Findings)[i]
		f.Source = source
		f.Ref = fmt.Sprintf("%s-%d.%d", short, round+1, i+1)
	}
}

// Verdicts an adjudicator may return, and what each finding may be decided.
const (
	verdictAccept = "accept"
	verdictRevise = "revise"
	actionFix     = "fix"
	actionDismiss = "dismiss"
)

// Decision is what the adjudicator decided about one finding, and why.
type Decision struct {
	Ref    string `json:"ref"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// ScopeAddition is a file the adjudicator adds to the builder's write scope.
type ScopeAddition struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Amendment is a simplification the adjudicator makes to the accepted plan.
// Changing what the engineer asked for is not one: that needs a person.
type Amendment struct {
	Change string `json:"change"`
	Reason string `json:"reason"`
}

// AdjudicationOutput is the adjudicator's decision on one round.
type AdjudicationOutput struct {
	Summary    *string          `json:"summary"`
	Verdict    *string          `json:"verdict"`
	Decisions  *[]Decision      `json:"decisions"`
	Changes    *string          `json:"changes"`
	Scope      *[]ScopeAddition `json:"scope"`
	Amendments *[]Amendment     `json:"amendments"`
	Wrote      *[]string        `json:"artifacts"`
}

func (a *AdjudicationOutput) Validate() []string {
	var v []string
	if a.Summary == nil || strings.TrimSpace(*a.Summary) == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if a.Verdict == nil || (*a.Verdict != verdictAccept && *a.Verdict != verdictRevise) {
		v = append(v, `"verdict" must be "accept" or "revise"`)
	}
	if a.Decisions == nil {
		v = append(v, `"decisions" is missing; use [] if there were no findings`)
	} else {
		for _, d := range *a.Decisions {
			if d.Action != actionFix && d.Action != actionDismiss {
				v = append(v, fmt.Sprintf(`decision for %q: "action" must be "fix" or "dismiss"`, d.Ref))
			}
			if strings.TrimSpace(d.Reason) == "" {
				v = append(v, fmt.Sprintf(`decision for %q has no "reason"`, d.Ref))
			}
		}
	}
	if a.Changes == nil {
		v = append(v, `"changes" is missing; use "" when accepting`)
	}
	if a.Scope == nil {
		v = append(v, `"scope" is missing; use [] if the plan's files suffice`)
	} else {
		for _, s := range *a.Scope {
			if strings.TrimSpace(s.Reason) == "" {
				v = append(v, fmt.Sprintf(`scope addition %q has no "reason"`, s.Path))
			}
		}
	}
	if a.Amendments == nil {
		v = append(v, `"amendments" is missing; use [] if the plan stands`)
	} else {
		for _, m := range *a.Amendments {
			if strings.TrimSpace(m.Change) == "" || strings.TrimSpace(m.Reason) == "" {
				v = append(v, `every amendment needs a "change" and a "reason"`)
			}
		}
	}
	if a.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	if len(v) > 0 {
		return v
	}
	if *a.Verdict == verdictRevise && strings.TrimSpace(*a.Changes) == "" {
		v = append(v, `a "revise" verdict needs "changes": the one set of instructions the implementer receives`)
	}
	if *a.Verdict == verdictAccept {
		for _, d := range *a.Decisions {
			if d.Action == actionFix {
				v = append(v, fmt.Sprintf(`you accepted, but decided %q must be fixed; revise, or dismiss it with a reason`, d.Ref))
			}
		}
		if len(*a.Scope) > 0 || len(*a.Amendments) > 0 {
			v = append(v, `scope additions and amendments only apply to a "revise" verdict`)
		}
	}
	return v
}

func (a *AdjudicationOutput) Artifacts() []string {
	if a.Wrote == nil {
		return nil
	}
	return *a.Wrote
}

// fixes is every finding the adjudicator decided must be fixed.
func (a *AdjudicationOutput) fixes(findings []Finding) []Finding {
	action := map[string]string{}
	for _, d := range *a.Decisions {
		action[d.Ref] = d.Action
	}
	var out []Finding
	for _, f := range findings {
		if action[f.Ref] == actionFix {
			out = append(out, f)
		}
	}
	return out
}

// Adjudicated is the gate that makes an adjudication complete and honest: every
// finding decided exactly once, no invented references, a scope that could be
// granted, and no acceptance without a measured green suite. The last is here
// rather than in the prompt because a waiver is exactly what an adjudicator
// might be talked into, and the measurement is lathe's, not its.
func Adjudicated(findings []Finding, tests *Measured, protected []string) run.Gate {
	return func(e run.Envelope, _ *run.Run) []string {
		a, ok := e.(*AdjudicationOutput)
		if !ok {
			return nil
		}
		var v []string
		known := map[string]bool{}
		for _, f := range findings {
			known[f.Ref] = true
		}
		decided := map[string]int{}
		for _, d := range *a.Decisions {
			if !known[d.Ref] {
				v = append(v, fmt.Sprintf(`%q is not a finding from this round`, d.Ref))
			}
			decided[d.Ref]++
		}
		for _, f := range findings {
			switch decided[f.Ref] {
			case 0:
				v = append(v, fmt.Sprintf(`%q has no decision; fix or dismiss every finding`, f.Ref))
			case 1:
			default:
				v = append(v, fmt.Sprintf(`%q is decided more than once`, f.Ref))
			}
		}
		for _, s := range *a.Scope {
			if problem := pathProblem(s.Path, "scope", protected); problem != "" {
				v = append(v, problem)
			}
		}
		if *a.Verdict == verdictAccept && (tests == nil || !tests.Green) {
			v = append(v, "you cannot accept while the measured test suite is red: tests must pass, "+
				"even when the failures look unrelated or pre-existing; revise instead")
		}
		return v
	}
}

// Measured is lathe's own run of the tester's command: the only evidence of
// green or red that counts.
type Measured struct {
	Command string `json:"command"`
	Green   bool   `json:"green"`
	Result  string `json:"result"`
	Tail    string `json:"tail"`
}

// WorkerFailure is a worker that exhausted its retries. It is an
// infrastructure outcome, kept apart from findings.
type WorkerFailure struct {
	Worker string `json:"worker"`
	Error  string `json:"error"`
}

// Round is one implementation round's validation evidence. Rounds are kept,
// never overwritten, so a later repair cannot erase what an earlier one was
// told.
type Round struct {
	Round        int                          `json:"round"`
	Tests        *TestOutput                  `json:"tester,omitempty"`
	Measured     *Measured                    `json:"measured,omitempty"`
	Reports      map[string]*CodeReviewOutput `json:"reports"`
	Failures     []WorkerFailure              `json:"failures,omitempty"`
	Mutated      []string                     `json:"mutated,omitempty"`
	Adjudication *AdjudicationOutput          `json:"adjudication,omitempty"`
}

// newRound makes the round every worker fills in. Each reviewer's report is
// allocated here, so the workers only ever write through their own pointer and
// never into the map.
func newRound(n int) *Round {
	r := &Round{Round: n, Reports: map[string]*CodeReviewOutput{}}
	for _, name := range reviewers {
		r.Reports[name] = &CodeReviewOutput{}
	}
	return r
}

// findings is every reviewer's findings, in reviewer order.
func (r *Round) findings() []Finding {
	var out []Finding
	for _, name := range reviewers {
		if rep := r.Reports[name]; rep != nil && rep.Findings != nil {
			out = append(out, *rep.Findings...)
		}
	}
	return out
}

// Outcomes of validation. Only accepted is success; unresolved and incomplete
// are the outcomes build may still publish as a draft, and invalidated is a
// repository-integrity failure, which it may not.
const (
	outcomeAccepted    = "accepted"
	outcomeUnresolved  = "unresolved"
	outcomeIncomplete  = "incomplete"
	outcomeInvalidated = "invalidated"
)

// Validation is validation.json: every round, every amendment, and how it ended.
type Validation struct {
	Summary    string          `json:"summary"`
	Outcome    string          `json:"outcome"`
	Reason     string          `json:"reason,omitempty"`
	Rounds     []*Round        `json:"rounds"`
	Scope      []ScopeAddition `json:"scope,omitempty"`
	Amendments []Amendment     `json:"amendments,omitempty"`
	Unresolved []Finding       `json:"unresolved,omitempty"`
}

func (v *Validation) current() *Round { return v.Rounds[len(v.Rounds)-1] }

// Accepted is the one question every outcome's handling asks: may this ship as
// finished work? Only accepted says yes; every other outcome is unfinished.
func (v *Validation) Accepted() bool { return v.Outcome == outcomeAccepted }

// settle records how validation ended. The summary is what `lathe show` prints.
func (v *Validation) settle(outcome, reason string) {
	v.Outcome, v.Reason = outcome, reason
	v.Summary = fmt.Sprintf("%s after %d round(s)", outcome, len(v.Rounds))
	if reason != "" {
		v.Summary += ": " + reason
	}
}

// evidence renders the parts of a round every worker and the adjudicator are
// shown: the implementation as it now stands.
func evidence(w io.Writer, c *code) {
	fmt.Fprintf(w, "\n## Implementation (round %d)\n\n%s\n", len(c.v.Rounds), *c.out.Summary)
	section(w, "### Changed files", *c.out.Changed)
}

// previousDecision renders the last adjudication, which every worker in the
// next round is shown alongside the implementer's response to it.
func previousDecision(w io.Writer, c *code) {
	if len(c.v.Rounds) < 2 {
		return
	}
	last := c.v.Rounds[len(c.v.Rounds)-2].Adjudication
	if last == nil {
		return
	}
	fmt.Fprintf(w, "\n## Previous adjudication\n\n%s\n\n### Requested changes\n\n%s\n", *last.Summary, *last.Changes)
	section(w, "### Decisions", decisionLines(*last.Decisions))
	fmt.Fprintf(w, "\nThe implementer's response to it is the implementation summary above.\n")
}

// reviewRequest is one reviewer's brief for one round.
func reviewRequest(c *code) string {
	var b strings.Builder
	b.WriteString(c.handoff)
	evidence(&b, c)
	previousDecision(&b, c)
	b.WriteString("\nInspect the code as it is now. Anything you concluded in an earlier round may be stale.\n")
	return b.String()
}

// testRequest is the tester's brief for one round. The command is reassessed
// every round, because a repair can change how the suite has to be invoked.
func testRequest(c *code) string {
	var b strings.Builder
	b.WriteString(c.handoff)
	evidence(&b, c)
	if n := len(c.v.Rounds); n > 1 {
		if prev := c.v.Rounds[n-2]; prev.Measured != nil {
			fmt.Fprintf(&b, "\n## Previous measurement\n\nCommand: %s\nResult: %s\n", prev.Measured.Command, prev.Measured.Result)
		}
	}
	previousDecision(&b, c)
	b.WriteString("\nReassess the test command for the code as it is now, then run it.\n")
	return b.String()
}

// adjudicationRequest is everything the adjudicator weighs: the request, the
// plan as amended, the implementation, the measurement, every report, and
// every earlier decision.
func adjudicationRequest(c *code, request string, round, left int) string {
	cur := c.v.current()
	var b strings.Builder
	fmt.Fprintf(&b, "Adjudication round %d of %d.\nYou may send the implementation back %d more times.\n",
		round, maxRepairs+1, left)
	b.WriteString("\n")
	b.WriteString(implementRequest(request, &c.plan, c.v.Amendments))
	evidence(&b, c)

	m := cur.Measured
	fmt.Fprintf(&b, "\n## Measured tests\n\nCommand: %s\nResult: %s\n\n### Output (last 4KB)\n\n%s\n", m.Command, m.Result, m.Tail)
	fmt.Fprintf(&b, "\n## Tester report\n\n%s\n\nCoverage: %s\n", *cur.Tests.Summary, *cur.Tests.Coverage)
	section(&b, "### Observed failures", *cur.Tests.Failures)

	b.WriteString("\n## Reviewer summaries\n")
	for _, name := range reviewers {
		fmt.Fprintf(&b, "\n%s: %s\n", name, *cur.Reports[name].Summary)
	}
	findings := cur.findings()
	b.WriteString("\n## Findings\n")
	if len(findings) == 0 {
		b.WriteString("\nNone.\n")
	}
	for _, f := range findings {
		writeFinding(&b, f)
	}

	for _, prev := range c.v.Rounds[:len(c.v.Rounds)-1] {
		if a := prev.Adjudication; a != nil {
			fmt.Fprintf(&b, "\n## Your decision in round %d: %s\n\n%s\n", prev.Round+1, *a.Verdict, *a.Summary)
			section(&b, "### Decisions", decisionLines(*a.Decisions))
		}
	}
	return b.String()
}

// decisionLines renders decisions one per line, as every later round sees them.
func decisionLines(decisions []Decision) []string {
	lines := make([]string, len(decisions))
	for i, d := range decisions {
		lines[i] = fmt.Sprintf("%s: %s (%s)", d.Ref, d.Action, d.Reason)
	}
	return lines
}

func writeFinding(w io.Writer, f Finding) {
	fmt.Fprintf(w, "\n- %s (%s)", f.Ref, f.Source)
	if f.Location != "" {
		fmt.Fprintf(w, " at %s", f.Location)
	}
	fmt.Fprintf(w, "\n  Evidence: %s\n  Explanation: %s\n  Requested: %s\n", f.Evidence, f.Explanation, f.Outcome)
}

// repairRequest is the one set of changes the implementer is sent back with.
func repairRequest(c *code, request string, a *AdjudicationOutput, fixes []Finding, sent int) string {
	var b strings.Builder
	b.WriteString(implementRequest(request, &c.plan, c.v.Amendments))
	fmt.Fprintf(&b, "\n## Repair requested (send-back %d of %d)\n\n%s\n", sent, maxRepairs, *a.Changes)
	if len(fixes) > 0 {
		b.WriteString("\n### Findings to fix\n")
		for _, f := range fixes {
			writeFinding(&b, f)
		}
	}
	var added []string
	for _, s := range *a.Scope {
		added = append(added, fmt.Sprintf("%s: %s", s.Path, s.Reason))
	}
	section(&b, "### Files added to your scope", added)
	b.WriteString("\nMake these changes. Report what you changed and anything that stopped you; do not waive a finding yourself.\n")
	return b.String()
}

// amend applies an adjudicator's scope additions and plan amendments, so every
// later builder call and every later worker sees the plan as it now stands.
// The additions passed the same protected-path check a plan's files do.
func (c *code) amend(request string, a *AdjudicationOutput) {
	for _, s := range *a.Scope {
		if !slices.Contains(*c.plan.Files, s.Path) {
			*c.plan.Files = append(*c.plan.Files, s.Path)
		}
	}
	c.v.Scope = append(c.v.Scope, *a.Scope...)
	c.v.Amendments = append(c.v.Amendments, *a.Amendments...)
	c.handoff = implementRequest(request, &c.plan, c.v.Amendments)
}

// validationReport is the section a draft pull request carries: why automated
// work stopped, and everything a reviewer needs to pick it up.
func validationReport(v *Validation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n## Validation not accepted\n\nThis is unfinished work. Automated validation stopped with the change unaccepted (%s): %s\n", v.Outcome, v.Reason)
	cur := v.current()
	if m := cur.Measured; m != nil {
		fmt.Fprintf(&b, "\n### Tests\n\nCommand: `%s`\nResult: %s\n\n````\n%s\n````\n", m.Command, m.Result, m.Tail)
	}
	if len(v.Unresolved) > 0 {
		b.WriteString("\n### Unresolved findings\n")
		for _, f := range v.Unresolved {
			writeFinding(&b, f)
		}
	}
	var failures []string
	for _, f := range cur.Failures {
		failures = append(failures, f.Worker+": "+f.Error)
	}
	section(&b, "\n### Missing validation", failures)
	var dismissed []string
	for _, r := range v.Rounds {
		if a := r.Adjudication; a != nil {
			for _, d := range *a.Decisions {
				if d.Action == actionDismiss {
					dismissed = append(dismissed, fmt.Sprintf("%s: %s", d.Ref, d.Reason))
				}
			}
		}
	}
	section(&b, "\n### Dismissed findings", dismissed)
	var scope, amendments []string
	for _, s := range v.Scope {
		scope = append(scope, s.Path+": "+s.Reason)
	}
	for _, m := range v.Amendments {
		amendments = append(amendments, m.Change+": "+m.Reason)
	}
	section(&b, "\n### Scope amendments", scope)
	section(&b, "\n### Plan amendments", amendments)
	return b.String()
}
