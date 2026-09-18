package workflow

import (
	"fmt"
	"io"
	"os"

	"github.com/tyrelh/lathe/internal/config"
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

// Plan reads the repo and produces the plan a builder will implement. Same
// shape as Scout — one read-only agent, one phase — because that is all it is
// when invoked on its own. Build hands the same envelope to the builder.
func Plan(cfg config.Config, ov config.Overrides, repo, request string) int {
	r, err := run.New(cfg, ov, "plan", repo, request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lathe:", err)
		return 1
	}

	err = r.Phase(run.Params{Name: "request", Kind: "engineer", Owner: "engineer"},
		func(ph *run.Handle) error { return ph.Log("request", request) })

	var out PlanOutput
	if err == nil {
		err = planPhase(r, cfg, request, &out)
	}
	// The plan is the product of this workflow, so it goes to the terminal as
	// well as to disk; a failed phase has nothing to print.
	if err == nil {
		printPlan(r.Out, &out)
	}
	return r.Finish(true, "")
}

// planPhase is the planner's whole contract — its gates and the name it reports
// under — in one place. `lathe build` runs this same phase before handing the
// plan to the builder, and a second copy is how the two would start planning
// under different rules without anything failing to compile.
func planPhase(r *run.Run, cfg config.Config, request string, out *PlanOutput) error {
	return r.Phase(run.Params{Name: "plan", Kind: "agent", Owner: "planner"},
		func(ph *run.Handle) error {
			if err := ph.Call(out, request,
				run.ArtifactsExist, run.FilesNonEmpty, FilesPermitted(cfg.Protected)); err != nil {
				return err
			}
			return writeResult(r.Dir, "plan.json", out)
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
