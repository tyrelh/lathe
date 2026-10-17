package run

import "fmt"

// Report file names in a run directory, shared by the workflows that write
// them and the commands that read them back.
const (
	IssueFile       = "issue.json"
	ResultFile      = "result.json"
	PlanFile        = "plan.json"
	ImplementFile   = "implement.json"
	ValidationFile  = "validation.json"
	DismissalsFile  = "dismissals.json"
	DispositionFile = "disposition.json"
	PRFile          = "pr.json"
	PatchFile       = "result.patch"
	ScopeGlob       = "scope-*.json"
)

// PlanVersionFile is the name of revised plan n.
func PlanVersionFile(n int) string { return fmt.Sprintf("plan-%d.json", n) }

// ScopeFile is the name of scope request n.
func ScopeFile(n int) string { return fmt.Sprintf("scope-%d.json", n) }

// Reports are the structured results a workflow leaves in its run directory,
// in the order they are produced.
var Reports = []string{
	IssueFile,
	ResultFile,
	PlanFile,
	ImplementFile,
	"build.json", // legacy, kept so older runs still read
	"test.json",  // legacy, kept so older runs still read
	ValidationFile,
	PRFile,
}
