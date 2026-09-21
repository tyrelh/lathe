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

// Build plans, implements and verifies a change. Reports live in the run directory; the
// implementation stays in the target working tree for the user to review.
func Build(r *run.Run) int {
	request := r.Request
	err := r.Phase(run.Params{Name: "request", Kind: "engineer", Owner: "engineer"}, func(ph *run.Handle) error {
		if err := ph.Log("request", request); err != nil {
			return err
		}
		return r.EnsureClean()
	})
	var plan PlanOutput
	if err == nil {
		err = planPhase(r, request, &plan)
	}
	var out BuildOutput
	var builderSession, handoff string
	if err == nil {
		handoff = buildRequest(request, &plan)
		err = r.Phase(run.Params{Name: "build", Kind: "agent", Owner: "builder"}, func(ph *run.Handle) error {
			builderSession = ph.SessionID()
			ph.Scope(*plan.Files)
			err := ph.Call(&out, handoff, run.ArtifactsExist, run.FilesNonEmpty, ChangesMatchClaim)
			// Keep a terminal needed report alongside the plan for diagnosis.
			if err == nil || out.Failure() != nil {
				if saveErr := writeResult(r.Dir, "build.json", &out); saveErr != nil {
					return saveErr
				}
			}
			return err
		})
	}
	var tests TestOutput
	if err == nil {
		err = r.Phase(run.Params{Name: "test", Kind: "agent", Owner: "tester", RevertOnly: true}, func(ph *run.Handle) error {
			if err := ph.Call(&tests, handoff+"\n## Implementation\n\n"+*out.Summary); err != nil {
				return err
			}
			return writeResult(r.Dir, "test.json", &tests)
		})
	}
	if err == nil {
		for round := 0; ; round++ {
			var tail string
			err = r.Phase(run.Params{Name: "verify", Kind: "code", Owner: "engineer", RevertOnly: true}, func(ph *run.Handle) error {
				var verifyErr error
				tail, verifyErr = ph.Command(*tests.Command)
				return verifyErr
			})
			// Green, terminal, or out of rounds: the loop only continues for a
			// measured red it is still allowed to hand back to the builder.
			var red *run.CommandFailure
			if err == nil || !errors.As(err, &red) || round == maxFixRounds {
				break
			}
			var feedback strings.Builder
			fmt.Fprintf(&feedback, "%s\n## Fix failing tests\n\nCommand: %s\nResult: %s\n", handoff, *tests.Command, err)
			if round == 0 {
				section(&feedback, "### Tester observations", *tests.Failures)
			}
			fmt.Fprintf(&feedback, "\n### Latest output (last 4KB)\n\n%s\n", tail)
			err = r.Phase(run.Params{Name: "fix", Kind: "agent", Owner: "builder", SessionID: builderSession}, func(ph *run.Handle) error {
				ph.Scope(*plan.Files)
				callErr := ph.Call(&out, feedback.String(), run.ArtifactsExist, run.FilesNonEmpty, ChangesMatchClaim)
				if callErr == nil || out.Failure() != nil {
					if saveErr := writeResult(r.Dir, "build.json", &out); saveErr != nil {
						return saveErr
					}
				}
				return callErr
			})
			if err != nil {
				break
			}
		}
	}
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
