package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planReply is one Pi stream carrying a planner's whole answer. files is raw
// JSON so a test can put anything in it, including what the gate must reject.
func planReply(t *testing.T, files string) string {
	t.Helper()
	return piReply(t, "Read the client.\n\n```json\n"+
		`{"summary": "wrap fetch in a retry", "steps": ["edit fetch.go"], "files": [`+files+
		`], "risks": [], "artifacts": []}`+"\n```\n")
}

// Phase 1's done-when: plan.json lands in the run directory with the file list
// intact.
func TestPlanWritesPlanJSON(t *testing.T) {
	stubPi(t, planReply(t, `"fetch.go", "fetch_test.go"`))
	cfg, repo := load(t)

	if code := execute(t, cfg, "plan", repo, "add retry to the fetch client"); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}

	b, err := os.ReadFile(filepath.Join(latestRunDir(t), "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got PlanOutput
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Files == nil || strings.Join(*got.Files, ",") != "fetch.go,fetch_test.go" {
		t.Fatalf("plan.json did not round-trip the file list: %s", b)
	}
}

// A protected path dies at the gate rather than reaching a builder that would
// then be permitted to write it.
func TestPlanRejectsProtectedPath(t *testing.T) {
	bad := planReply(t, `"fetch.go", ".env"`)
	stubPi(t, bad, bad, bad)
	cfg, repo := load(t)

	if code := execute(t, cfg, "plan", repo, "add retry to the fetch client"); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "plan.json")); err == nil {
		t.Fatal("plan.json was written for a rejected plan")
	}
}

// The other three ways a file list stops being a usable write scope: a path
// outside the repo, an absolute path, and a pattern instead of a file.
func TestFilesPermitted(t *testing.T) {
	gate := FilesPermitted([]string{".git/", ".env*", "*.pem", "*.key"})
	for _, tc := range []struct{ file, want string }{
		{"fetch.go", ""},
		{"internal/run/run.go", ""},
		{"../outside.go", "not inside the repository"},
		{"/etc/passwd", "not inside the repository"},
		{"internal/*.go", "looks like a glob"},
		{".git/config", "is protected"},
		{"deploy/server.pem", "is protected"},
	} {
		v := gate(&PlanOutput{Files: &[]string{tc.file}}, nil)
		switch {
		case tc.want == "" && len(v) > 0:
			t.Errorf("%q was rejected: %s", tc.file, strings.Join(v, "; "))
		case tc.want != "" && (len(v) != 1 || !strings.Contains(v[0], tc.want)):
			t.Errorf("%q gave %v; want one violation mentioning %q", tc.file, v, tc.want)
		}
	}
}

// An empty file list is a violation, not an empty scope: it would leave the
// builder nothing it is allowed to write.
func TestPlanOutputValidateRejectsAnEmptyFileList(t *testing.T) {
	summary, steps, empty := "wrap fetch", []string{"edit fetch.go"}, []string{}
	out := &PlanOutput{Summary: &summary, Steps: &steps, Files: &empty, Risks: &empty, Wrote: &empty}
	v := out.Validate()
	if len(v) != 1 || !strings.Contains(v[0], `"files" is empty`) {
		t.Fatalf("violations = %v; want one about an empty file list", v)
	}
}

// The plan is this workflow's product, so it has to be readable without opening
// the run directory.
func TestPrintPlan(t *testing.T) {
	summary := "wrap fetch in a retry"
	steps, files, risks := []string{"edit fetch.go"}, []string{"fetch.go", "fetch_test.go"}, []string{"the backoff is untested"}
	out := &strings.Builder{}
	printPlan(out, &PlanOutput{Summary: &summary, Steps: &steps, Files: &files, Risks: &risks})
	for _, want := range []string{summary, "edit fetch.go", "fetch_test.go", "the backoff is untested"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("printed plan is missing %q:\n%s", want, out)
		}
	}
}
