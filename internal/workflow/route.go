package workflow

// PlanRouteState is what route-plan sends Jev: the request the planner will
// be given.
func PlanRouteState(request string) any {
	return struct {
		Request string `json:"request"`
	}{request}
}

// BuildRouteState is what route-build sends Jev: the request and the accepted
// plan, with the file count bucketed into words. plan's lists are non-nil.
func BuildRouteState(request string, plan *PlanOutput) any {
	return struct {
		Request   string   `json:"request"`
		Summary   string   `json:"summary"`
		Steps     []string `json:"steps"`
		Files     []string `json:"files"`
		FileCount string   `json:"file_count"`
		Risks     []string `json:"risks"`
	}{request, *plan.Summary, *plan.Steps, *plan.Files, fileCount(len(*plan.Files)), *plan.Risks}
}

// fileCount is n in words, because Jev counts poorly.
func fileCount(n int) string {
	switch {
	case n <= 1:
		return "one file"
	case n <= 5:
		return "a few files"
	default:
		return "many files"
	}
}
