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

// scopeLoop is the scope state the implement, plan and review nodes share.
type scopeLoop struct {
	// pending is the request being replanned; nil outside a scope cycle.
	pending *ScopeRequest
	// brief is set until the planner has been handed pending.
	brief bool
	// last is the most recent request the planner answered.
	last *ScopeRequest
}

// pendingScope is the scope request being replanned, or nil outside a scope
// cycle and for a plan-only run's nil c.
func (c *code) pendingScope() *ScopeRequest {
	if c == nil {
		return nil
	}
	return c.scope.pending
}

// awaitingDisposition reports whether r is a recovery whose reviewed plan has
// not yet been applied to the restored work. A nil c never is.
func (c *code) awaitingDisposition(r *run.Run) bool {
	return c != nil && r.Recovery != nil && !c.disposed
}

// blocked handles a builder's scope request: it saves the report and the
// request, then sends planning back to the planner, or ends the iteration on
// a malformed request or once maxScopeSendBacks are spent.
func (c *code) blocked(e *run.Entry, r *run.Run, s *ScopeError) (string, error) {
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
	c.v.ScopeRequests++
	req := &ScopeRequest{
		N: c.v.ScopeRequests, Round: len(c.v.Rounds), During: during,
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
	if err := writeResult(r.Dir, run.ImplementFile, &c.out); err != nil {
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
	if err := writeResult(r.Dir, run.PlanVersionFile(req.N), &c.plan); err != nil {
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

// Spent reports whether the request used one of the run's scope send-backs.
func (s ScopeRequest) Spent() bool {
	return s.Outcome != scopeRejected && s.Outcome != scopeExhausted
}

// ReadScopeRequests reads dir's scope requests in file-name order, skipping
// unreadable ones. found counts every scope request file, readable or not.
func ReadScopeRequests(dir string) (reqs []ScopeRequest, found int) {
	paths, _ := filepath.Glob(filepath.Join(dir, run.ScopeGlob))
	for _, path := range paths {
		var req ScopeRequest
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &req) == nil {
			reqs = append(reqs, req)
		}
	}
	return reqs, len(paths)
}

// saveScope writes req to scope-<n>.json.
func saveScope(r *run.Run, req *ScopeRequest) error {
	return writeResult(r.Dir, run.ScopeFile(req.N), req)
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
	findingSection(w, "### Outstanding adjudicator findings", req.Outstanding)
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
	findingSection(w, "Outstanding adjudicator findings", req.Outstanding)
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
		findingSection(&b, "### Findings still to fix", c.outstanding)
		if c.changes != "" {
			fmt.Fprintf(&b, "\n### The adjudicator's earlier requested changes\n\nThe revised plan takes precedence where they conflict.\n\n%s\n", c.changes)
		}
	}
	b.WriteString("\nReport what you changed and anything that stopped you.\n")
	return b.String()
}
