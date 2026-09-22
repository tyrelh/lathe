package workflow

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tyrelh/lathe/internal/permit"
	"github.com/tyrelh/lathe/internal/run"
)

// BuildOutput describes the work left in the tree and any missing permission.
type BuildOutput struct {
	Summary *string   `json:"summary"`
	Changed *[]string `json:"changed"`
	Needed  *[]string `json:"needed"`
	Wrote   *[]string `json:"artifacts"`
}

func (b *BuildOutput) Validate() []string {
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

func (b *BuildOutput) Artifacts() []string {
	if b.Wrote == nil {
		return nil
	}
	return *b.Wrote
}

// Failure makes BuildOutput a run.Failing: a missing permission is terminal even
// when the rest of the report needs correction, because correcting it would turn
// a plan that was never sufficient into an apparently successful run.
func (b *BuildOutput) Failure() error {
	if b.Needed != nil && len(*b.Needed) > 0 {
		return fmt.Errorf("plan missed files the builder needs: %s", strings.Join(*b.Needed, ", "))
	}
	return nil
}

// ChangesMatchClaim compares both directions against Git, not the plan: files
// permitted to change are not necessarily files the builder actually changed.
func ChangesMatchClaim(e run.Envelope, r *run.Run) []string {
	b, ok := e.(*BuildOutput)
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

// buildRequest is the handoff: the request, plus the plan the builder is held
// to. It renders through the same section helper printPlan uses, so what the
// builder is told and what the engineer was shown cannot drift apart. Every
// list is non-nil because the plan phase's Validate has already accepted it.
func buildRequest(request string, plan *PlanOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Request\n\n%s\n\n## Accepted plan\n\n%s\n", request, *plan.Summary)
	section(&b, "### Steps", *plan.Steps)
	section(&b, "### Allowed files", *plan.Files)
	section(&b, "### Risks", *plan.Risks)
	return b.String()
}

// maxFixRounds gives the builder four attempts to repair measured test failures
// after its initial implementation, bounding the cost of a persistently red suite.
const maxFixRounds = 4

// fixRequest is what the builder is sent back with: the original handoff, the
// command lathe measured, and the tail of its output. The tester's own
// observations seed the first round only — after that the measurement is the
// better evidence and the discovery is stale.
func fixRequest(handoff string, tests *TestOutput, red error, tail string, first bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n## Fix failing tests\n\nCommand: %s\nResult: %s\n", handoff, *tests.Command, red)
	if first {
		section(&b, "### Tester observations", *tests.Failures)
	}
	fmt.Fprintf(&b, "\n### Latest output (last 4KB)\n\n%s\n", tail)
	return b.String()
}

// Build plans, implements and verifies a change. Reports live in the run directory; the
// implementation stays in the target working tree for the user to review.
func Build(r *run.Run) int {
	var plan PlanOutput
	var out BuildOutput
	var tests TestOutput
	var handoff, feedback string

	g := run.NewGraph(r)
	g.Add(run.Node{Name: "request", Owner: "engineer"}, func(e *run.Entry) (string, error) {
		if err := e.Log("request", r.Request); err != nil {
			return "", err
		}
		return "", r.EnsureClean()
	})
	addPlanNodes(g, r, &plan)

	g.Add(run.Node{Name: "build", Owner: "builder"}, func(e *run.Entry) (string, error) {
		request := feedback
		if e.Round == 0 {
			// The plan is settled by the time anything reaches here: review only
			// ever sends back to the planner, never past this node.
			handoff = buildRequest(r.Request, &plan)
			request = handoff
		}
		e.Scope(*plan.Files)
		err := e.Call(&out, request, run.ArtifactsExist, run.FilesNonEmpty, ChangesMatchClaim)
		// Keep a terminal needed report alongside the plan for diagnosis.
		if err == nil || out.Failure() != nil {
			if saveErr := writeResult(r.Dir, "build.json", &out); saveErr != nil {
				return "", saveErr
			}
		}
		return "", err
	})

	// Discovery is a claim; green and red are a measurement. Both live in this
	// node, but only the exit code from Command decides where it goes: a tester
	// that could route on its own report of the suite could end a run green by
	// saying so.
	g.Add(run.Node{Name: "test", Owner: "tester", SendBacks: maxFixRounds, RevertOnly: true}, func(e *run.Entry) (string, error) {
		if e.Round == 0 {
			if err := e.Call(&tests, handoff+"\n## Implementation\n\n"+*out.Summary); err != nil {
				return "", err
			}
			if err := writeResult(r.Dir, "test.json", &tests); err != nil {
				return "", err
			}
		}
		// ponytail: the command discovered on the first entry is reused for the
		// rest of the run, so a fix that changes how the suite is invoked leaves
		// a stale command behind. Re-discover per round if that ever bites.
		tail, err := e.Command(*tests.Command)
		var red *run.CommandFailure
		switch {
		case err == nil:
			return "", nil
		case !errors.As(err, &red):
			return "", err // denied, timeout, infrastructure: terminal
		case e.SendBacksLeft == 0:
			return "", err // out of budget: the run fails red
		}
		e.Failed(err)
		feedback = fixRequest(handoff, &tests, err, tail, e.Round == 0)
		return "build", nil
	})

	err := g.Run()
	if err == nil {
		fmt.Fprintf(r.Out, "\n%s\n", *out.Summary)
		section(r.Out, "changed (uncommitted)", *out.Changed)
	}
	reason := ""
	if err != nil {
		reason = err.Error()
	}
	return r.Finish(err == nil, reason)
}
