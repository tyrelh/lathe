package workflow

import "testing"

func TestTestOutputRequiredFields(t *testing.T) {
	if v := (&TestOutput{}).Validate(); len(v) != 7 {
		t.Fatalf("violations %v", v)
	}
	summary, command, output, coverage, empty := "suite", "go test ./...", "ok", "unchanged", []string{}
	exit := 0
	out := &TestOutput{Summary: &summary, Command: &command, ExitCode: &exit, Output: &output, Failures: &empty, Coverage: &coverage, Wrote: &empty}
	if v := out.Validate(); len(v) != 0 {
		t.Fatal(v)
	}
	command = "  "
	if v := out.Validate(); len(v) != 1 {
		t.Fatal(v)
	}
}
