package workflow

import "testing"

func TestTestOutputRequiredFields(t *testing.T) {
	if v := (&TestOutput{}).Validate(); len(v) != 5 {
		t.Fatalf("violations %v", v)
	}
	summary, command, coverage, empty := "suite", "go test ./...", "unchanged", []string{}
	out := &TestOutput{Summary: &summary, Command: &command, Failures: &empty, Coverage: &coverage, Wrote: &empty}
	if v := out.Validate(); len(v) != 0 {
		t.Fatal(v)
	}
	command = "  "
	if v := out.Validate(); len(v) != 1 {
		t.Fatal(v)
	}
}
