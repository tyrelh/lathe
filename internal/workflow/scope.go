package workflow

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/workspace"
)

// A scope request is a builder stopping because the reviewed plan cannot
// complete the request. It returns planning authority to the planner, which
// revises the plan under review; the builder never widens its own scope.

// maxScopeSendBacks is how many scope requests per iteration go back to the
// planner. The next one ends the iteration. It is independent of the review
// and repair allowances.
const maxScopeSendBacks = 2

// ScopeError is a builder's typed scope request: the exact paths it needs and
// why the plan cannot complete the request without them.
type ScopeError struct {
	Needed      []string
	Explanation string
}

func (s *ScopeError) Error() string {
	msg := "plan missed files the builder needs: " + strings.Join(s.Needed, ", ")
	if s.Explanation != "" {
		msg += " (" + s.Explanation + ")"
	}
	return msg
}

// Reversal is uncommitted work a revised plan abandons. Lathe saves the work,
// then reverts the path before the builder resumes.
type Reversal struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Dismissal is an outstanding adjudicator finding a revised plan makes moot.
// Request is the scope request whose plan dismissed it.
//
// From is the iteration whose validation raised the finding, set when that
// is an earlier iteration; nil means this iteration's own finding. Refs
// repeat across iterations, so a dismissal applies only to its own.
type Dismissal struct {
	Ref     string `json:"ref"`
	Reason  string `json:"reason"`
	Request int    `json:"scope_request,omitempty"`
	From    *int   `json:"from_iteration,omitempty"`
}

// Ticket is adjacent work a planner proposes for a separate run. It grants
// no scope and creates nothing.
type Ticket struct {
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// Outcomes of a scope request.
const (
	scopeReplanning = "replanning"
	scopeReplanned  = "replanned"
	scopeRejected   = "rejected"
	scopeExhausted  = "exhausted"
	scopeUnreviewed = "unreviewed"
	scopeBlocked    = "blocked"
)

// ScopeRequest is scope-<n>.json: one scope request and everything the
// planner was handed to answer it. Each request keeps its own file, so a
// later request never overwrites an earlier one's evidence.
type ScopeRequest struct {
	N           int             `json:"request"`
	Round       int             `json:"round"`
	During      string          `json:"during"`
	Needed      []string        `json:"needed"`
	Protected   []string        `json:"protected,omitempty"`
	Explanation string          `json:"explanation"`
	Problems    []string        `json:"problems,omitempty"`
	Outcome     string          `json:"outcome"`
	Report      ImplementOutput `json:"report"`
	Dirty       []string        `json:"dirty"`
	Diff        string          `json:"diff,omitempty"`
	Plan        PlanOutput      `json:"plan"`
	Scope       []ScopeAddition `json:"scope,omitempty"`
	Amendments  []Amendment     `json:"amendments,omitempty"`
	Outstanding []Finding       `json:"outstanding,omitempty"`
	Reversed    []string        `json:"reversed,omitempty"`
	Abandoned   string          `json:"abandoned,omitempty"`
	Dismissed   []Dismissal     `json:"dismissed,omitempty"`
}

// builderMode is why the builder is entered: the first implementation, an
// adjudicator repair, or a plan revised in answer to a scope request.
type builderMode int

const (
	buildInitial builderMode = iota
	buildRepair
	buildReplanned
)

// scopeLoop is the scope state the implement, plan and review nodes share.
type scopeLoop struct {
	// pending is the request being replanned; nil outside a scope cycle.
	pending *ScopeRequest
	// brief is set until the planner has been handed pending.
	brief bool
	// last is the most recent request the planner answered.
	last     *ScopeRequest
	requests int
}

// blocked handles a builder's scope request: it saves the report and the
// request, then sends planning back to the planner, or ends the iteration on
// a malformed request or once maxScopeSendBacks are spent.
func (c *code) blocked(e *run.Entry, r *run.Run, s *ScopeError) (string, error) {
	c.scope.requests++
	dirty, err := permit.Changed(r.Repo)
	if err != nil {
		return "", err
	}
	diff, err := workspace.WorkingDiff(r.Repo, diffLimit)
	if err != nil {
		diff = "unavailable: " + err.Error()
	}
	during := "implementation"
	if c.mode == buildRepair {
		during = "repair"
	}
	req := &ScopeRequest{
		N: c.scope.requests, Round: len(c.v.Rounds), During: during,
		Needed: s.Needed, Explanation: s.Explanation,
		Report: c.out, Dirty: dirty, Diff: diff, Plan: clonePlan(&c.plan),
		Scope: c.v.Scope, Amendments: c.v.Amendments, Outstanding: c.outstanding,
		Problems: neededProblems(r.Repo, s.Needed, *c.plan.Files),
	}
	for _, p := range s.Needed {
		if _, denied := permit.Denied(p, r.Protected()); denied {
			req.Protected = append(req.Protected, p)
		}
	}
	switch {
	case len(req.Problems) > 0:
		req.Outcome = scopeRejected
	case e.SendBacksLeft == 0:
		req.Outcome = scopeExhausted
	default:
		req.Outcome = scopeReplanning
	}
	c.v.ScopeRequests = c.scope.requests
	if err := writeResult(r.Dir, "implement.json", &c.out); err != nil {
		return "", err
	}
	if err := saveScope(r, req); err != nil {
		return "", err
	}
	if err := e.Log("scope", fmt.Sprintf("request %d (%s): %s", req.N, req.Outcome, strings.Join(req.Needed, ", "))); err != nil {
		return "", err
	}
	switch req.Outcome {
	case scopeRejected:
		return "", fmt.Errorf("the builder's scope request cannot be answered (%s); nothing was replanned, and the partial work is kept%s",
			strings.Join(req.Problems, "; "), patchNote(r))
	case scopeExhausted:
		return "", fmt.Errorf("scope: the builder asked for files the plan does not allow (%s) after %d scope send-backs were spent; nothing was published, and the partial work is kept%s%s",
			strings.Join(req.Needed, ", "), maxScopeSendBacks, patchNote(r), resumeHint(r))
	}
	e.Failed(s)
	c.scope.pending, c.scope.brief = req, true
	return "plan", nil
}

// replanned applies a reviewed plan's answer to the pending scope request:
// it saves and reverts abandoned work, records finding dismissals, saves the
// plan version, and resumes the builder without repeating any setup.
func (c *code) replanned(e *run.Entry, r *run.Run) (string, error) {
	req := c.scope.pending
	d, err := c.dispose(e, r, fmt.Sprintf("abandoned-%d.patch", req.N), req.N)
	if err != nil {
		return "", err
	}
	req.Reversed, req.Abandoned, req.Dismissed = d.Reversed, d.Abandoned, d.Dismissed
	req.Outcome = scopeReplanned
	if err := writeResult(r.Dir, fmt.Sprintf("plan-%d.json", req.N), &c.plan); err != nil {
		return "", err
	}
	if err := saveScope(r, req); err != nil {
		return "", err
	}
	c.scope.last, c.scope.pending = req, nil
	c.mode = buildReplanned
	// Forward past branch creation and builder routing: both are settled for
	// the iteration.
	return "implement", nil
}

// Disposition is what a reviewed plan did with work and findings it found:
// paths it abandoned, where that work was saved, and findings it dismissed.
type Disposition struct {
	Reversed  []string    `json:"reversed,omitempty"`
	Abandoned string      `json:"abandoned,omitempty"`
	Dismissed []Dismissal `json:"dismissed,omitempty"`
}

// dispose applies a reviewed plan's reversals and dismissals before the
// builder runs: the whole uncommitted tree is saved to name in the run
// directory, then the abandoned paths are reverted; dismissed findings leave
// c.outstanding and are recorded in validation with request. Enforcement
// drops reverted paths from retained work at its next sweep, since they are
// no longer dirty. Callers invoke it only after review forwards the plan.
func (c *code) dispose(e *run.Entry, r *run.Run, name string, request int) (Disposition, error) {
	var d Disposition
	if c.plan.Reversals != nil {
		for _, rv := range *c.plan.Reversals {
			d.Reversed = append(d.Reversed, permit.Norm(rv.Path))
		}
	}
	if len(d.Reversed) > 0 {
		dest := filepath.Join(r.Dir, name)
		saved, err := workspace.SavePatch(r.Repo, "HEAD", dest, r.Protected())
		if err != nil {
			return d, fmt.Errorf("saving abandoned work before reverting it: %w; nothing was reverted", err)
		}
		if saved {
			d.Abandoned = dest
		}
		if err := permit.Revert(r.Repo, d.Reversed); err != nil {
			return d, err
		}
		if err := e.Log("reverted", fmt.Sprintf("%s (saved to %s)", strings.Join(d.Reversed, ", "), d.Abandoned)); err != nil {
			return d, err
		}
	}
	if c.plan.Dismissals != nil && len(*c.plan.Dismissals) > 0 {
		dismissed := map[string]bool{}
		for _, dm := range *c.plan.Dismissals {
			dm.Request = request
			// The Dismissible gate admitted only outstanding refs; record
			// whose finding this was.
			for _, f := range c.outstanding {
				if f.Ref == dm.Ref {
					dm.From = f.From
				}
			}
			d.Dismissed = append(d.Dismissed, dm)
			c.v.Dismissals = append(c.v.Dismissals, dm)
			dismissed[dm.Ref] = true
		}
		var keep []Finding
		for _, f := range c.outstanding {
			if !dismissed[f.Ref] {
				keep = append(keep, f)
			}
		}
		c.outstanding = keep
		// Saved now: the builder that follows may fail before validation
		// writes the report again, and a recovery must not resurrect these.
		// dismissals.json keeps every dismissal of this iteration even when
		// it never validates, for later recoveries.
		if err := writeResult(r.Dir, "dismissals.json", c.v.Dismissals); err != nil {
			return d, err
		}
		if len(c.v.Rounds) > 0 {
			if err := writeResult(r.Dir, "validation.json", &c.v); err != nil {
				return d, err
			}
		}
	}
	return d, nil
}

// saveScope writes req to scope-<n>.json.
func saveScope(r *run.Run, req *ScopeRequest) error {
	return writeResult(r.Dir, fmt.Sprintf("scope-%d.json", req.N), req)
}

// neededProblems is why each needed path cannot be planned at all: empty,
// escaping, a glob, not exact, a directory, or already allowed. Protected
// paths are not problems here; the planner must find another way.
func neededProblems(repo string, needed, files []string) []string {
	var v []string
	for _, p := range needed {
		switch {
		case strings.TrimSpace(p) == "":
			v = append(v, "an empty path")
		case permit.Escapes(p):
			v = append(v, fmt.Sprintf("%q is not inside the repository", p))
		case globby(p):
			v = append(v, fmt.Sprintf("%q is a glob, not one exact file", p))
		case permit.Norm(p) != p:
			v = append(v, fmt.Sprintf("%q is not an exact repo-relative path", p))
		case slices.Contains(files, p):
			v = append(v, fmt.Sprintf("%q is already in the plan's files", p))
		default:
			if fi, err := os.Stat(filepath.Join(repo, p)); err == nil && fi.IsDir() {
				v = append(v, fmt.Sprintf("%q is a directory, not one exact file", p))
			}
		}
	}
	return v
}

// clonePlan is a deep copy of p, so later amendments to the live plan cannot
// alter recorded evidence.
func clonePlan(p *PlanOutput) PlanOutput {
	var out PlanOutput
	if b, err := json.Marshal(p); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

// planOf is the PlanOutput inside a plan or revision plan envelope.
func planOf(e run.Envelope) (*PlanOutput, bool) {
	if rp, ok := e.(*revisionPlan); ok {
		return (*PlanOutput)(rp), true
	}
	p, ok := e.(*PlanOutput)
	return p, ok
}

// Retained holds a plan to the uncommitted work in the tree: every dirty path
// must stay in files or be abandoned in reversals, and a reversal may name
// only dirty work.
func Retained(dirty []string) run.Gate {
	return func(e run.Envelope, _ *run.Run) []string {
		p, ok := planOf(e)
		if !ok || p.Files == nil {
			return nil
		}
		inTree := map[string]bool{}
		for _, d := range dirty {
			inTree[permit.Norm(d)] = true
		}
		kept, reversed := map[string]bool{}, map[string]bool{}
		for _, f := range *p.Files {
			kept[permit.Norm(f)] = true
		}
		var v []string
		if p.Reversals != nil {
			for _, rv := range *p.Reversals {
				path := permit.Norm(rv.Path)
				if !inTree[path] || permit.Escapes(rv.Path) {
					v = append(v, fmt.Sprintf(`reversal %q names no uncommitted work; "reversals" may only abandon paths listed as work in the tree`, rv.Path))
					continue
				}
				reversed[path] = true
			}
		}
		for _, d := range dirty {
			if path := permit.Norm(d); !kept[path] && !reversed[path] {
				v = append(v, fmt.Sprintf(`%q holds uncommitted work; keep it in "files" or abandon it in "reversals" with a reason`, d))
			}
		}
		return v
	}
}

// Dismissible lets a plan dismiss only findings still outstanding.
func Dismissible(outstanding []Finding) run.Gate {
	return func(e run.Envelope, _ *run.Run) []string {
		p, ok := planOf(e)
		if !ok || p.Dismissals == nil {
			return nil
		}
		known := map[string]bool{}
		for _, f := range outstanding {
			known[f.Ref] = true
		}
		var v []string
		seen := map[string]bool{}
		for _, d := range *p.Dismissals {
			switch {
			case !known[d.Ref]:
				v = append(v, fmt.Sprintf(`%q is not an outstanding finding; "dismissals" may only name findings listed as outstanding`, d.Ref))
			case seen[d.Ref]:
				v = append(v, fmt.Sprintf(`%q is dismissed more than once`, d.Ref))
			}
			seen[d.Ref] = true
		}
		return v
	}
}

// scopeBrief is the planner's request for a scope send-back: the original
// goal, the effective plan, the builder's evidence and the outstanding
// findings.
func scopeBrief(c *code, r *run.Run, dirty []string) string {
	req := c.scope.pending
	var b strings.Builder
	fmt.Fprintf(&b, "The builder stopped: the reviewed plan does not let it complete the request. This is scope request %d of %d this iteration; the one after the last ends the run.\n",
		req.N, maxScopeSendBacks)
	b.WriteString("Revise the plan. Return the complete plan, within the original request below; do not change what was asked for.\n")
	fmt.Fprintf(&b, "\n## Request\n\n%s\n", r.Request)
	b.WriteString("\n## Effective plan\n")
	printPlan(&b, &c.plan)
	var amendments, additions []string
	for _, a := range c.v.Amendments {
		amendments = append(amendments, a.Change+" (because "+a.Reason+")")
	}
	for _, s := range c.v.Scope {
		additions = append(additions, s.Path+": "+s.Reason)
	}
	section(&b, "### Plan amendments made by the adjudicator", amendments)
	section(&b, "### Files the adjudicator added", additions)
	scopeEvidence(&b, req, dirty)
	b.WriteString("\n## How to answer\n\n" +
		"- Grant the needed files by listing them in `files`, or plan another approach that does not need them. An unchanged file list is valid when the steps change the approach.\n" +
		"- Protected paths can never be granted; plan around them or say in `risks` why the request cannot be completed.\n" +
		"- Every path under work in the tree must stay in `files`, or be abandoned in `reversals` as {\"path\", \"reason\"}. Lathe saves abandoned work, then reverts it.\n" +
		"- `dismissals` may name outstanding findings the new approach makes moot, as {\"ref\", \"reason\"}. Findings not dismissed still have to be fixed.\n" +
		"- Adjacent work outside the request may go in `future_tickets` as {\"title\", \"reason\"}. It grants no scope.\n")
	return b.String()
}

// scopeEvidence renders what the builder reported and what the tree holds.
func scopeEvidence(w io.Writer, req *ScopeRequest, dirty []string) {
	fmt.Fprintf(w, "\n## Builder scope request (during %s)\n", req.During)
	if req.Report.Summary != nil {
		fmt.Fprintf(w, "\n%s\n", *req.Report.Summary)
	}
	if req.Explanation != "" {
		fmt.Fprintf(w, "\nWhy: %s\n", req.Explanation)
	}
	section(w, "### Files the builder says it needs", req.Needed)
	section(w, "### Of those, protected (never grantable)", req.Protected)
	section(w, "### Work in the tree", dirty)
	if strings.TrimSpace(req.Diff) != "" {
		fmt.Fprintf(w, "\n### Diff against HEAD (untracked files are listed above, not shown)\n\n```diff\n%s\n```\n", req.Diff)
	}
	if len(req.Outstanding) > 0 {
		fmt.Fprintf(w, "\n### Outstanding adjudicator findings\n")
		for _, f := range req.Outstanding {
			writeFinding(w, f)
		}
	}
}

// scopeReview tells the reviewer what the plan under review must answer.
func scopeReview(w io.Writer, c *code) {
	req := c.scope.pending
	fmt.Fprintf(w, "\n## Scope request %d\n\nThe builder stopped because the previous plan could not complete the request.\n", req.N)
	if req.Report.Summary != nil {
		fmt.Fprintf(w, "\n%s\n", *req.Report.Summary)
	}
	if req.Explanation != "" {
		fmt.Fprintf(w, "\nWhy: %s\n", req.Explanation)
	}
	section(w, "Files the builder says it needs", req.Needed)
	section(w, "Of those, protected (never grantable)", req.Protected)
	section(w, "Uncommitted work in the tree", req.Dirty)
	if len(req.Outstanding) > 0 {
		fmt.Fprintf(w, "\nOutstanding adjudicator findings\n")
		for _, f := range req.Outstanding {
			writeFinding(w, f)
		}
	}
	w.Write([]byte("\nBlock this plan if it neither grants what the builder needs nor gives a workable approach without it. " +
		"An unchanged file list can resolve the request through a different approach. Check that every dismissal is justified by the changed approach.\n"))
}

// replannedRequest is the builder's request after a scope send-back: the full
// refreshed handoff, the answer to its request, and the findings still to fix.
func replannedRequest(c *code) string {
	req := c.scope.last
	var b strings.Builder
	b.WriteString(c.handoff)
	fmt.Fprintf(&b, "\n## Revised plan (scope request %d of %d)\n\n", req.N, maxScopeSendBacks)
	fmt.Fprintf(&b, "You stopped because the plan did not allow %s. The planner revised the plan above and a reviewer checked it; "+
		"it is your whole write scope now. It may grant those files or take another approach without them. "+
		"Your earlier changes remain in the tree unless listed as reverted below; continue from them.\n",
		strings.Join(req.Needed, ", "))
	section(&b, "### Reverted by lathe (your work there is saved)", req.Reversed)
	var dismissed []string
	for _, d := range req.Dismissed {
		dismissed = append(dismissed, d.Ref+": "+d.Reason)
	}
	section(&b, "### Findings the revised plan dismissed", dismissed)
	if len(c.outstanding) > 0 {
		b.WriteString("\n### Findings still to fix\n")
		for _, f := range c.outstanding {
			writeFinding(&b, f)
		}
		if c.changes != "" {
			fmt.Fprintf(&b, "\n### The adjudicator's earlier requested changes\n\nThe revised plan takes precedence where they conflict.\n\n%s\n", c.changes)
		}
	}
	b.WriteString("\nReport what you changed and anything that stopped you.\n")
	return b.String()
}

// resumeHint is how an engineer continues a run that stopped here.
func resumeHint(r *run.Run) string {
	if r.ID == "" {
		return ""
	}
	return fmt.Sprintf("; continue it with: lathe resume %s \"guidance\"", r.ID)
}
