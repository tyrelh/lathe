package workflow

import "strings"

// ReviewOutput accepts a plan with empty feedback and blocking lists. Required
// pointers distinguish acceptance from a reviewer that omitted the decision
// entirely.
//
// Blocking is the objections that mean the plan cannot succeed as written: a
// file it needs and does not list, a step it concedes it cannot perform. They
// go back to the planner like any feedback, but unlike feedback they are never
// forwarded to the builder as risks: a plan still blocked at the review limit
// fails the run, because building it spends a builder turn on work that says
// it will not land.
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
