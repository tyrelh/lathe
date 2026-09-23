package workflow

import "strings"

// TestOutput records discovery; the command's exit status decides acceptance.
// Coverage is the tester's account of any test the round no longer runs, which
// the adjudicator weighs: a green suite reached by skipping the red tests is not
// a pass.
type TestOutput struct {
	Summary  *string   `json:"summary"`
	Command  *string   `json:"command"`
	Failures *[]string `json:"failures"`
	Coverage *string   `json:"coverage"`
	Wrote    *[]string `json:"artifacts"`
}

func (t *TestOutput) Validate() []string {
	var v []string
	if t.Summary == nil || strings.TrimSpace(*t.Summary) == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if t.Command == nil || strings.TrimSpace(*t.Command) == "" {
		v = append(v, `"command" is missing or empty`)
	}
	if t.Failures == nil {
		v = append(v, `"failures" is missing; use [] if none`)
	}
	if t.Coverage == nil || strings.TrimSpace(*t.Coverage) == "" {
		v = append(v, `"coverage" is missing or empty; say "unchanged" if no test was removed, skipped or narrowed`)
	}
	if t.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if none`)
	}
	return v
}

func (t *TestOutput) Artifacts() []string {
	if t.Wrote == nil {
		return nil
	}
	return *t.Wrote
}
