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
	addPlanNodes(g, r, &out)

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

// maxSendBacks permits four revisions to close omissions before the fifth
// review hands any remaining objections to the builder as risks.
const maxSendBacks = 4

// addPlanNodes appends the plan and review nodes both workflows start with.
// They are shared rather than repeated because the graph, not the helper, is
// what decides where review forwards to — so nothing here has to know whether
// a builder comes next.
func addPlanNodes(g *run.Graph, r *run.Run, out *PlanOutput) {
	if r.Issue != "" {
		g.Add(run.Node{Name: "issue", Owner: "engineer"}, func(e *run.Entry) (string, error) {
			request, err := e.IssueRequest()
			if err != nil {
				return "", err
			}
			r.Request = request
			return "", nil
		})
	}
	// The reviewer's objections, which of them block the plan outright, and
	// whether they are a real review or the reviewer having returned nothing
	// usable. All three are read by the plan node on its way back round.
	var feedback, blocking []string
	var unusable bool

	g.Add(run.Node{Name: "plan", Owner: "planner"}, func(e *run.Entry) (string, error) {
		request := r.Request
		if e.Round > 0 {
			var revision strings.Builder
			revision.WriteString("Revise the plan using the review feedback below. Return the complete plan.\n")
			printPlan(&revision, out)
			section(&revision, "Blocking objections: the plan cannot succeed until these are resolved", blocking)
			section(&revision, "Review feedback", feedback)
			if unusable {
				revision.WriteString("\nThe reviewer returned nothing usable. You may reply with the same plan unchanged.\n")
			}
			request = revision.String()
		}
		return "", e.Call(out, request, run.ArtifactsExist, run.FilesNonEmpty, FilesPermitted(r.Protected()))
	})

	g.Add(run.Node{Name: "review", Owner: "plan-reviewer", SendBacks: maxSendBacks}, func(e *run.Entry) (string, error) {
		round := e.Round + 1
		var message strings.Builder
		fmt.Fprintf(&message, "Review round %d of %d.\nYou may send this plan back %d more times.\n",
			round, maxSendBacks+1, e.SendBacksLeft)
		if round > 1 {
			message.WriteString("\nThe planner returned the following complete plan.\n")
		}
		fmt.Fprintf(&message, "\n## Request\n\n%s\n", r.Request)
		printPlan(&message, out)

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
			return "plan", nil
		}
		if len(blocking) > 0 {
			// Saved for diagnosis, but never handed on: the reviewer said this
			// plan cannot succeed, and nothing after this node could change that.
			if err := writeResult(r.Dir, "plan.json", out); err != nil {
				return "", err
			}
			section(r.Out, "blocking objections", blocking)
			return "", fmt.Errorf("plan review: the plan cannot succeed as written after %d revisions: %s",
				round-1, strings.Join(blocking, "; "))
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
		return "", nil
	})
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
