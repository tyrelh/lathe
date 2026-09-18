package workflow

import (
	"fmt"
	"os"
	"strings"

	"github.com/tyrelh/lathe/internal/config"
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

// Build plans and implements a change. Reports live in the run directory; the
// implementation stays in the target working tree for the user to review.
func Build(cfg config.Config, ov config.Overrides, repo, request string) int {
	r, err := run.New(cfg, ov, "build", repo, request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
	}
	err = r.Phase(run.Params{Name: "request", Kind: "engineer", Owner: "engineer"}, func(ph *run.Handle) error {
		if err := ph.Log("request", request); err != nil {
			return err
		}
		return r.EnsureClean()
	})
	var plan PlanOutput
	if err == nil {
		err = planPhase(r, cfg, request, &plan)
	}
	var out BuildOutput
	if err == nil {
		err = r.Phase(run.Params{Name: "build", Kind: "agent", Owner: "builder"}, func(ph *run.Handle) error {
			ph.Scope(*plan.Files)
			err := ph.Call(&out, buildRequest(request, &plan), run.ArtifactsExist, run.FilesNonEmpty, ChangesMatchClaim)
			// Keep a terminal needed report alongside the plan for diagnosis.
			if err == nil || out.Failure() != nil {
				if saveErr := writeResult(r.Dir, "build.json", &out); saveErr != nil {
					return saveErr
				}
			}
			return err
		})
	}
	if err == nil {
		fmt.Fprintf(r.Out, "\n%s\n", *out.Summary)
		section(r.Out, "changed (uncommitted)", *out.Changed)
	}
	return r.Finish(err == nil, "")
}
