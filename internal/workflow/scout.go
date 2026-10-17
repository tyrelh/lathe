// Package workflow holds the phase graphs, the output envelopes and gates each
// agent's report must satisfy, and the briefs given to agents.
package workflow

import "github.com/tyrelh/lathe/internal/run"

// ScoutOutput is what the scout must return. Required fields are pointers
// because encoding/json zero-fills anything absent — a plain string cannot
// tell "the agent said nothing" from "the agent said empty".
//
// This struct and the `## Report` block of seed/agents/scout/user.md are one
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
		// A recovery rescouts read-only, with the prior evidence and guidance.
		if err := e.Call(&out, r.Request+recoveryContext(r, nil), run.ArtifactsExist, run.FilesNonEmpty); err != nil {
			return "", err
		}
		return "", writeResult(r.Dir, run.ResultFile, &out)
	})

	if err := g.Run(); err != nil {
		return r.Finish(false, err.Error())
	}
	return r.Finish(true, "")
}
