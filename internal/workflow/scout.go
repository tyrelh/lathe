// Package workflow holds the phase graphs. In v0 there is one, and this file
// carries only its output contract; the graph itself lands with the CLI.
package workflow

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
