// Package workflow holds the phase graphs. A graph is deliberately thin: all
// the logic lives in internal/run, and a file here is only the order things
// happen in and the contract the agent has to satisfy.
package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/tyrelh/lathe/internal/run"
)

// Graphs is every workflow by name: what the worker executes, and the list a
// submission validates a command against.
var Graphs = map[string]func(*run.Run) int{
	"scout":     Scout,
	"plan":      Plan,
	"implement": Implement,
	"build":     Build,
}

// Writes is every workflow that changes the checkout it runs in. It is what
// decides which runs need a clean tree, which own the checkout while they run,
// and which exclude each other — so those three questions are asked of the
// workflow list rather than of a literal workflow name in three packages.
var Writes = map[string]bool{"implement": true, "build": true}

// Agents is every agent each graph will spawn. Submission resolves all of
// them before recording a run, so an unknown agent or a bad prompt fails in
// under a second rather than after an agent turn — and the resolved roster is
// what the run executes from.
var Agents = map[string][]string{
	"scout":     {"scout"},
	"plan":      {"planner", "plan-reviewer"},
	"implement": {"planner", "plan-reviewer", "builder", "tester"},
	"build": {"planner", "plan-reviewer", "brancher",
		"builder", "tester", "committer", "pr-author"},
}

// ScoutOutput is what the scout must return. Required fields are pointers
// because encoding/json zero-fills anything absent — a plain string cannot
// tell "the agent said nothing" from "the agent said empty".
//
// This struct and the `## Report` block of prompts/scout/user.md are one
// contract in two files. Changing either alone is the usual way this breaks.
type ScoutOutput struct {
	Summary  *string   `json:"summary"`
	Findings *[]string `json:"findings"`
	Wrote    *[]string `json:"artifacts"`
}

// Validate returns what is missing, phrased as the agent will read it: the
// violations go straight back into its own session as the correction.
func (s *ScoutOutput) Validate() []string {
	var v []string
	if s.Summary == nil || *s.Summary == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if s.Findings == nil {
		v = append(v, `"findings" is missing; use [] if you found nothing`)
	}
	if s.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	return v
}

// Artifacts is what the gates check. A scout writes nothing, so this is
// normally empty — the gates exist to establish the shape before a writing
// agent gives them something to catch.
func (s *ScoutOutput) Artifacts() []string {
	if s.Wrote == nil {
		return nil
	}
	return *s.Wrote
}

// Scout investigates the run's repository and reports. It returns the exit
// code: Finish settles the status, the banner and the code together so the
// three cannot disagree.
func Scout(r *run.Run) int {
	var out ScoutOutput

	g := run.NewGraph(r)
	// The request is a phase so the trace starts with what was asked, in the
	// same table as everything that followed from it.
	g.Add(run.Node{Name: "request", Owner: "engineer"},
		func(e *run.Entry) (string, error) { return "", e.Log("request", r.Request) })
	g.Add(run.Node{Name: "scout", Owner: "scout"}, func(e *run.Entry) (string, error) {
		if err := e.Call(&out, r.Request, run.ArtifactsExist, run.FilesNonEmpty); err != nil {
			return "", err
		}
		return "", writeResult(r.Dir, "result.json", &out)
	})

	// A failed phase already settles the run's status, so there is nothing for
	// this workflow to decide on top of it.
	g.Run()
	return r.Finish(true, "")
}

// writeResult puts the accepted envelope beside raw.jsonl, under the name the
// workflow's readers expect. Callers must propagate disk failures to the workflow.
func writeResult(dir, name string, out any) error {
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644)
}
