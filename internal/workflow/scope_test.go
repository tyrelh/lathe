package workflow

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// planWith is a planner reply with extra keys, such as reversals or dismissals.
func planWith(t *testing.T, files []string, extra map[string]any) string {
	t.Helper()
	report := map[string]any{"summary": "wrap fetch in a retry", "steps": []string{"edit fetch.go"},
		"files": files, "risks": []string{}, "artifacts": []string{}}
	maps.Copy(report, extra)
	return jsonReply(t, report)
}

// neededReply is a builder stopping for files its plan does not allow.
func neededReply(t *testing.T, changed, needed []string, explanation string) string {
	t.Helper()
	report := map[string]any{"summary": "stopped for scope", "changed": changed, "needed": needed, "artifacts": []string{}}
	if explanation != "" {
		report["explanation"] = explanation
	}
	return jsonReply(t, report)
}

func readScope(t *testing.T, n string) ScopeRequest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(latestRunDir(t), "scope-"+n+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var req ScopeRequest
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

// An omission after an allowed edit goes back to the planner. The edit
// survives read-only planning, the builder resumes its session with the
// refreshed scope, and one full validation checks the finished work.
func TestScopeRequestReplansAndKeepsPartialWork(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "builder.sh"), "printf 'hello world\\n' > hello.txt\nif [ \"$n\" = 1 ]; then echo extra > extra.txt; fi\n")
	write(t, filepath.Join(stub, "builder.0"), neededReply(t, []string{"hello.txt"}, []string{"extra.txt"}, "THE-GREETING-LIVES-IN-EXTRA"))
	write(t, filepath.Join(stub, "builder.1"), jsonReply(t, map[string]any{"summary": "done",
		"changed": []string{"extra.txt", "hello.txt"}, "needed": []string{}, "artifacts": []string{"extra.txt", "hello.txt"}}))
	write(t, filepath.Join(stub, "planner.1"), planReply(t, `"hello.txt", "extra.txt"`))

	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	for agent, want := range map[string]int{"builder": 2, "planner": 2, "plan-reviewer": 2, "tester": 1, "adjudicator": 1} {
		if n := spawnsOf(t, stub, agent); n != want {
			t.Fatalf("%s spawned %d times, want %d", agent, n, want)
		}
	}
	brief := stubFile(t, stub, "args.planner.1")
	for _, want := range []string{"scope request 1 of 2", "extra.txt", "THE-GREETING-LIVES-IN-EXTRA", "Work in the tree", "+hello world"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("planner brief missing %q: %s", want, brief)
		}
	}
	if review := stubFile(t, stub, "args.plan-reviewer.1"); !strings.Contains(review, "Scope request 1") {
		t.Fatalf("reviewer not told of the scope request: %s", review)
	}
	resumed := stubFile(t, stub, "args.builder.1")
	if !strings.Contains(resumed, "Revised plan (scope request 1 of 2)") || session(t, resumed) != session(t, stubFile(t, stub, "args.builder.0")) {
		t.Fatalf("builder handoff after replanning: %s", resumed)
	}
	if scope := stubFile(t, stub, "scope.builder.1"); !strings.Contains(scope, `"allow":["hello.txt","extra.txt"]`) {
		t.Fatalf("refreshed scope = %s", scope)
	}
	if req := readScope(t, "1"); req.Outcome != scopeReplanned || strings.Join(req.Needed, ",") != "extra.txt" || strings.Join(req.Dirty, ",") != "hello.txt" {
		t.Fatalf("scope request evidence: %+v", req)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "plan-1.json")); err != nil {
		t.Fatal(err)
	}
	v := readValidation(t)
	if v.Outcome != outcomeAccepted || len(v.Rounds) != 1 || v.Rounds[0].Round != 0 || v.ScopeRequests != 1 {
		t.Fatalf("validation: %+v", v)
	}
	var implement []string
	for _, ph := range runPhases(t) {
		if ph.Name == "implement" {
			implement = append(implement, ph.Status)
		}
	}
	if strings.Join(implement, ",") != "fail,success" {
		t.Fatalf("implement phases: %v", implement)
	}
}

// A build that replans creates its branch once and ships the finished work.
func TestBuildScopeRequestCreatesTheBranchOnce(t *testing.T) {
	cfg, repo, _ := shipRepo(t)
	stub := shipStub(t, "feat/greet-the-world")
	write(t, filepath.Join(stub, "builder.0"), neededReply(t, []string{"hello.txt"}, []string{"extra.txt"}, ""))
	write(t, filepath.Join(stub, "planner.1"), planReply(t, `"hello.txt", "extra.txt"`))

	if code := execute(t, cfg, "build", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if n := spawnsOf(t, stub, "brancher"); n != 1 {
		t.Fatalf("brancher spawned %d times", n)
	}
	assertShipped(t, repo)
}

// A planner that drops uncommitted work without abandoning it is corrected;
// an explicit reversal saves the work, then reverts it.
func TestReplanReversalsPreserveAbandonedWork(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "builder.sh"), "if [ \"$n\" = 0 ]; then echo ABANDONED > hello.txt; else echo other > other.txt; fi\n")
	write(t, filepath.Join(stub, "builder.0"), neededReply(t, []string{"hello.txt"}, []string{"extra.txt"}, ""))
	write(t, filepath.Join(stub, "builder.1"), jsonReply(t, map[string]any{"summary": "another way",
		"changed": []string{"other.txt"}, "needed": []string{}, "artifacts": []string{"other.txt"}}))
	write(t, filepath.Join(stub, "planner.1"), planReply(t, `"other.txt"`))
	write(t, filepath.Join(stub, "planner.2"), planWith(t, []string{"other.txt"}, map[string]any{
		"reversals": []map[string]string{{"path": "hello.txt", "reason": "another approach"}}}))

	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if correction := stubFile(t, stub, "args.planner.2"); !strings.Contains(correction, `"hello.txt" holds uncommitted work`) {
		t.Fatalf("dropped work was not corrected: %s", correction)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "hello.txt")); string(b) != "hello\n" {
		t.Fatalf("abandoned work not reverted: %q", b)
	}
	patch, err := os.ReadFile(filepath.Join(latestRunDir(t), "abandoned-1.patch"))
	if err != nil || !strings.Contains(string(patch), "ABANDONED") {
		t.Fatalf("abandoned work not saved: %s %v", patch, err)
	}
	if req := readScope(t, "1"); strings.Join(req.Reversed, ",") != "hello.txt" {
		t.Fatalf("reversal not recorded: %+v", req)
	}
}

// A request during a repair carries the outstanding findings to the planner,
// which may dismiss one with a reason; fresh validation follows, and rounds
// count completed implementations.
func TestScopeRequestDuringRepair(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "code-review-general.0"), reviewerReply(t, "GREETING-TOO-LOUD"))
	write(t, filepath.Join(stub, "adjudicator.0"), adjudicatorReply(t, "revise", "SOFTEN-IT",
		[]map[string]string{decide("general-1.1", "fix")}, nil))
	write(t, filepath.Join(stub, "builder.1"), neededReply(t, []string{"hello.txt"}, []string{"extra.txt"}, ""))
	write(t, filepath.Join(stub, "planner.1"), planWith(t, []string{"hello.txt"}, map[string]any{
		"dismissals": []map[string]string{{"ref": "general-1.1", "reason": "MOOT-UNDER-NEW-APPROACH"}}}))

	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if brief := stubFile(t, stub, "args.planner.1"); !strings.Contains(brief, "GREETING-TOO-LOUD") || !strings.Contains(brief, "during repair") {
		t.Fatalf("planner not given the outstanding findings: %s", brief)
	}
	if got := stubFile(t, stub, "args.builder.2"); !strings.Contains(got, "MOOT-UNDER-NEW-APPROACH") {
		t.Fatalf("builder not told of the dismissal: %s", got)
	}
	if got := stubFile(t, stub, "args.code-review-general.1"); !strings.Contains(got, "MOOT-UNDER-NEW-APPROACH") {
		t.Fatalf("reviewers not shown the dismissal: %s", got)
	}
	v := readValidation(t)
	if v.Outcome != outcomeAccepted || len(v.Rounds) != 2 || v.Rounds[1].Round != 1 ||
		len(v.Dismissals) != 1 || v.Dismissals[0].Ref != "general-1.1" || v.Dismissals[0].Request != 1 {
		t.Fatalf("validation: %+v", v)
	}
}

// A protected path can be asked for and replanned around, but never granted.
func TestProtectedScopeRequestReplansWithoutGranting(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "builder.0"), neededReply(t, []string{"hello.txt"}, []string{".env"}, ""))
	bad := planReply(t, `"hello.txt", ".env"`)
	write(t, filepath.Join(stub, "planner.1"), bad)

	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if got := stubFile(t, stub, "args.planner.2"); !strings.Contains(got, `".env" is protected`) {
		t.Fatalf("granting a protected path was not refused: %s", got)
	}
	if scope := stubFile(t, stub, "scope.builder.1"); !strings.Contains(scope, `"allow":["hello.txt"]`) {
		t.Fatalf("protected path granted: %s", scope)
	}
	if req := readScope(t, "1"); strings.Join(req.Protected, ",") != ".env" {
		t.Fatalf("protected request not recorded: %+v", req)
	}
}

// Malformed or escaping paths end the run without replanning.
func TestMalformedScopeRequestIsTerminal(t *testing.T) {
	for _, needed := range []string{"../outside.txt", "src/*.go", "./hello2.txt", "hello.txt"} {
		t.Run(needed, func(t *testing.T) {
			cfg, repo := buildRepo(t)
			stub := codeStub(t)
			write(t, filepath.Join(stub, "builder"), neededReply(t, []string{"hello.txt"}, []string{needed}, ""))
			if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
				t.Fatalf("code = %d", code)
			}
			if p, b := spawnsOf(t, stub, "planner"), spawnsOf(t, stub, "builder"); p != 1 || b != 1 {
				t.Fatalf("planner %d, builder %d; want no replanning", p, b)
			}
			if req := readScope(t, "1"); req.Outcome != scopeRejected || len(req.Problems) != 1 {
				t.Fatalf("scope request: %+v", req)
			}
			if reason := latestRun(t).Reason; !strings.Contains(reason, "cannot be answered") {
				t.Fatalf("reason = %q", reason)
			}
		})
	}
}

// A third scope request ends the iteration after two send-backs, keeps every
// request's evidence and points at manual recovery.
func TestThirdScopeRequestFails(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "builder"), neededReply(t, []string{"hello.txt"}, []string{"extra.txt"}, ""))
	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	for n, want := range map[string]string{"1": scopeReplanned, "2": scopeReplanned, "3": scopeExhausted} {
		if req := readScope(t, n); req.Outcome != want {
			t.Fatalf("scope-%s outcome %s, want %s", n, req.Outcome, want)
		}
	}
	if spawnsOf(t, stub, "tester") != 0 {
		t.Fatal("a blocked builder was validated")
	}
	if reason := latestRun(t).Reason; !strings.Contains(reason, "lathe resume") {
		t.Fatalf("reason lacks recovery guidance: %q", reason)
	}
	if got := gitBuild(t, repo, "status", "--porcelain"); got != " M hello.txt\n" {
		t.Fatalf("partial work not kept: %q", got)
	}
}

// Review that still blocks at its limit after a scope request stops the run.
func TestUnresolvedScopeBlockerStopsAtReviewExhaustion(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "builder.0"), neededReply(t, []string{"hello.txt"}, []string{"extra.txt"}, ""))
	write(t, filepath.Join(stub, "plan-reviewer"), blockedReply(t, "the plan still does not reach extra.txt"))
	write(t, filepath.Join(stub, "plan-reviewer.0"), reviewOK(t))
	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if n := spawnsOf(t, stub, "builder"); n != 1 {
		t.Fatalf("builder spawned %d times", n)
	}
	if n := spawnsOf(t, stub, "plan-reviewer"); n != maxSendBacks+2 {
		t.Fatalf("reviews = %d", n)
	}
	if reason := latestRun(t).Reason; !strings.Contains(reason, "scope request 1 is unresolved") {
		t.Fatalf("reason = %q", reason)
	}
}

// A report that did not decode is an ordinary invalid report, even when its
// needed list happened to decode: it is corrected, never replanned.
func TestUndecodableNeededReportIsNotAScopeRequest(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "builder"), piReply(t,
		"```json\n"+`{"summary":42,"changed":["hello.txt"],"needed":["extra.txt"],"artifacts":[]}`+"\n```"))
	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if p, b := spawnsOf(t, stub, "planner"), spawnsOf(t, stub, "builder"); p != 1 || b != 3 {
		t.Fatalf("planner %d, builder %d; want one plan and a corrected builder", p, b)
	}
	for i := 1; i < 3; i++ {
		if args := stubFile(t, stub, fmt.Sprintf("args.builder.%d", i)); !strings.Contains(args, "did not satisfy the output contract") {
			t.Fatalf("builder spawn %d was not a correction", i)
		}
		if scope := stubFile(t, stub, fmt.Sprintf("scope.builder.%d", i)); !strings.Contains(scope, `"allow":["hello.txt"]`) {
			t.Fatalf("scope widened: %s", scope)
		}
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "scope-1.json")); !os.IsNotExist(err) {
		t.Fatalf("an undecodable report was recorded as a scope request: %v", err)
	}
}

// A scope cycle whose last review is unusable at the limit stops: nothing
// established that the request was answered, so the builder never resumes.
func TestUnusableFinalScopeReviewStopsTheRun(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "builder.0"), neededReply(t, []string{"hello.txt"}, []string{"extra.txt"}, ""))
	write(t, filepath.Join(stub, "plan-reviewer"), piReply(t, "not an envelope"))
	write(t, filepath.Join(stub, "plan-reviewer.0"), reviewOK(t))
	for i := 1; i <= maxSendBacks; i++ {
		write(t, filepath.Join(stub, fmt.Sprintf("plan-reviewer.%d", i)), reviewReply(t, "tighten the steps"))
	}
	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if b, v := spawnsOf(t, stub, "builder"), spawnsOf(t, stub, "tester"); b != 1 || v != 0 {
		t.Fatalf("builder %d, tester %d; the builder resumed without a usable review", b, v)
	}
	if req := readScope(t, "1"); req.Outcome != scopeUnreviewed {
		t.Fatalf("scope request: %+v", req)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "plan-1.json")); err != nil {
		t.Fatal("candidate plan not kept")
	}
	if reason := latestRun(t).Reason; !strings.Contains(reason, "no usable review") {
		t.Fatalf("reason = %q", reason)
	}
	if got := gitBuild(t, repo, "status", "--porcelain"); got != " M hello.txt\n" {
		t.Fatalf("partial work not kept: %q", got)
	}
}

func TestDismissibleAndRetainedGates(t *testing.T) {
	files := []string{"a.go"}
	p := &PlanOutput{Files: &files, Dismissals: &[]Dismissal{{Ref: "x-1.1", Reason: "moot"}}}
	if v := Dismissible(nil)(p, nil); len(v) != 1 {
		t.Fatalf("dismissal without outstanding findings: %v", v)
	}
	if v := Dismissible([]Finding{{Ref: "x-1.1"}})(p, nil); len(v) != 0 {
		t.Fatalf("outstanding dismissal refused: %v", v)
	}
	p.Reversals = &[]Reversal{{Path: "c.go", Reason: "gone"}}
	if v := Retained([]string{"a.go", "b.go"})(p, nil); len(v) != 2 {
		t.Fatalf("retained: %v", v)
	}
}
