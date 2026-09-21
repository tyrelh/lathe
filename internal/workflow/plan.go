package workflow

import (
	"context"
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
// struct and the `## The file list` block of prompts/planner/user.md are one
// contract in two files.
type PlanOutput struct {
	Summary *string   `json:"summary"`
	Steps   *[]string `json:"steps"`
	Files   *[]string `json:"files"`
	Risks   *[]string `json:"risks"`
	Wrote   *[]string `json:"artifacts"`
}

// Validate returns what is missing, phrased as the agent will read it. An empty
// files list is a violation rather than an empty scope: a plan that permits no
// writes is a plan nothing can be built from.
func (p *PlanOutput) Validate() []string {
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
	case len(*p.Files) == 0:
		v = append(v, `"files" is empty; it is the only list of files the builder may write, so an empty one leaves nothing to build`)
	}
	if p.Risks == nil {
		v = append(v, `"risks" is missing; use [] if you see none`)
	}
	if p.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
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

// FilesPermitted rejects a plan that would hand a builder a path it must never
// have. The check belongs here rather than at the first blocked write: a plan
// naming .env costs one planner turn to reject and a whole run to discover.
func FilesPermitted(protected []string) run.Gate {
	return func(e run.Envelope, _ *run.Run) []string {
		p, ok := e.(*PlanOutput)
		if !ok || p.Files == nil {
			return nil
		}
		var v []string
		for _, f := range *p.Files {
			switch {
			case permit.Escapes(f):
				v = append(v, fmt.Sprintf(`%q is not inside the repository; every path in "files" is repo-relative`, f))
			case globby(f):
				v = append(v, fmt.Sprintf(`%q looks like a glob; every path in "files" is one exact file`, f))
			default:
				if pattern, denied := permit.Denied(f, protected); denied {
					v = append(v, fmt.Sprintf(`%q is protected (it matches %s) and may never be written; plan the change without it`, f, pattern))
				}
			}
		}
		return v
	}
}

// globby is why the permission check downstream can be string equality rather
// than a glob engine.
func globby(path string) bool {
	for _, c := range path {
		if c == '*' || c == '?' || c == '[' {
			return true
		}
	}
	return false
}

// Plan reads and reviews the plan a builder will implement. Build hands the
// same reviewed envelope to the builder.
func Plan(r *run.Run) int {
	request := r.Request
	err := r.Phase(run.Params{Name: "request", Kind: "engineer", Owner: "engineer"},
		func(ph *run.Handle) error { return ph.Log("request", request) })

	var out PlanOutput
	if err == nil {
		err = planPhase(r, request, &out)
	}
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

// maxSendBacks permits four revisions to close omissions before the fifth
// review hands any remaining objections to the builder as risks.
const maxSendBacks = 4

// planPhase shares planning, review and file-scope checks across plan and build.
func planPhase(r *run.Run, request string, out *PlanOutput) error {
	var plannerSession, reviewerSession string
	if err := r.Phase(run.Params{Name: "plan", Kind: "agent", Owner: "planner"},
		func(ph *run.Handle) error {
			plannerSession = ph.SessionID()
			return ph.Call(out, request, run.ArtifactsExist, run.FilesNonEmpty, FilesPermitted(r.Protected()))
		}); err != nil {
		return err
	}

	for review := 1; review <= maxSendBacks+1; review++ {
		var message strings.Builder
		fmt.Fprintf(&message, "Review round %d of %d.\nYou may send this plan back %d more times.\n",
			review, maxSendBacks+1, maxSendBacks+1-review)
		if review > 1 {
			message.WriteString("\nThe planner returned the following complete plan.\n")
		}
		fmt.Fprintf(&message, "\n## Request\n\n%s\n", request)
		printPlan(&message, out)
		var report ReviewOutput
		var callErr error
		err := r.Phase(run.Params{Name: "review", Kind: "agent", Owner: "plan-reviewer", SessionID: reviewerSession},
			func(ph *run.Handle) error {
				reviewerSession = ph.SessionID()
				callErr = ph.Call(&report, message.String(), run.ArtifactsExist, run.FilesNonEmpty)
				if callErr != nil && !errors.Is(callErr, context.Canceled) && !errors.Is(callErr, context.DeadlineExceeded) {
					return &run.RecoverableError{Err: callErr}
				}
				return callErr
			})
		var recoverable *run.RecoverableError
		if err != nil && !errors.As(err, &recoverable) {
			return err
		}
		var feedback []string
		if callErr != nil {
			reviewerSession = ""
			feedback = []string{"review failed: reviewer returned nothing usable: " + callErr.Error()}
		} else {
			feedback = *report.Feedback
		}
		if len(feedback) == 0 || review == maxSendBacks+1 {
			for _, objection := range feedback {
				*out.Risks = append(*out.Risks, "unresolved review: "+objection)
			}
			if err := writeResult(r.Dir, "plan.json", out); err != nil {
				return err
			}
			if review > 1 {
				if len(feedback) == 0 {
					fmt.Fprintf(r.Out, "plan revised %d times; reviewer accepted\n", review-1)
				} else {
					fmt.Fprintf(r.Out, "plan revised %d times; %d objections unresolved\n", review-1, len(feedback))
				}
			}
			return nil
		}

		var revision strings.Builder
		revision.WriteString("Revise the plan using the review feedback below. Return the complete plan.\n")
		printPlan(&revision, out)
		section(&revision, "Review feedback", feedback)
		if callErr != nil {
			revision.WriteString("\nThe reviewer returned nothing usable. You may reply with the same plan unchanged.\n")
		}
		if err := r.Phase(run.Params{Name: "replan", Kind: "agent", Owner: "planner", SessionID: plannerSession},
			func(ph *run.Handle) error {
				return ph.Call(out, revision.String(), run.ArtifactsExist, run.FilesNonEmpty, FilesPermitted(r.Protected()))
			}); err != nil {
			return err
		}
	}
	return nil
}

// printPlan renders an envelope Validate has already accepted, so every list is
// non-nil by the time it gets here.
func printPlan(w io.Writer, p *PlanOutput) {
	fmt.Fprintf(w, "\n%s\n", *p.Summary)
	section(w, "steps", *p.Steps)
	section(w, "files the builder may write", *p.Files)
	section(w, "risks", *p.Risks)
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
