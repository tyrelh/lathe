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
	"scout": Scout,
	"plan":  Plan,
	"build": Build,
}

// Agents is every agent each graph will spawn. Submission resolves all of
// them before recording a run, so an unknown agent or a bad prompt fails in
// under a second rather than after an agent turn — and the resolved roster is
// what the run executes from.
var Agents = map[string][]string{
	"scout": {"scout"},
	"plan":  {"planner"},
	"build": {"planner", "builder", "tester"},
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
	request := r.Request

	// The request is a phase so the trace starts with what was asked, in the
	// same table as everything that followed from it.
	err := r.Phase(run.Params{Name: "request", Kind: "engineer", Owner: "engineer"},
		func(ph *run.Handle) error { return ph.Log("request", request) })

	// A failed phase does not stop a run on its own; with two phases the
	// workflow is what decides, and there is nothing to scout for if the run
	// could not even record the request.
	if err == nil {
		var out ScoutOutput
		r.Phase(run.Params{Name: "scout", Kind: "agent", Owner: "scout"},
			func(ph *run.Handle) error {
				if err := ph.Call(&out, request, run.ArtifactsExist, run.FilesNonEmpty); err != nil {
					return err
				}
				return writeResult(r.Dir, "result.json", &out)
			})
	}

	return r.Finish(true, "")
}

// writeResult puts the accepted envelope beside raw.jsonl, under the name the
// workflow's readers expect. It is written inside the phase so a disk failure
// fails the phase rather than being discovered later by whatever wanted the file.
func writeResult(dir, name string, out any) error {
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644)
}
