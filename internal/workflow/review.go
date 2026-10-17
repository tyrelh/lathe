package workflow

import "strings"

// ReviewOutput accepts a plan with empty feedback and blocking lists. Required
// pointers distinguish acceptance from a reviewer that omitted the decision
// entirely.
//
// Blocking lists objections that block the plan; see ADR 0001 for how they are
// handled at the review limit.
type ReviewOutput struct {
	Summary  *string   `json:"summary"`
	Feedback *[]string `json:"feedback"`
	Blocking *[]string `json:"blocking"`
	Wrote    *[]string `json:"artifacts"`
}

func (r *ReviewOutput) Validate() []string {
	var v []string
	if r.Summary == nil || strings.TrimSpace(*r.Summary) == "" {
		v = append(v, `"summary" is missing or empty`)
	}
	if r.Feedback == nil {
		v = append(v, `"feedback" is missing; use [] to accept the plan`)
	}
	if r.Blocking == nil {
		v = append(v, `"blocking" is missing; use [] if nothing stops the plan from succeeding`)
	}
	if r.Wrote == nil {
		v = append(v, `"artifacts" is missing; use [] if you wrote no files`)
	}
	return v
}

func (r *ReviewOutput) Artifacts() []string {
	if r.Wrote == nil {
		return nil
	}
	return *r.Wrote
}
