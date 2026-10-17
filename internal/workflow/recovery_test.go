package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
)

// executeRecovery is execute for a recovery iteration whose restored paths
// are already in repo.
func executeRecovery(t *testing.T, cfg config.Config, name, repo, request string, rec *run.Recovery) int {
	t.Helper()
	code, _ := recoverIn(t, cfg, name, repo, request, rec)
	return code
}

// recoverIn is executeRecovery returning the recovery run's report directory.
func recoverIn(t *testing.T, cfg config.Config, name, repo, request string, rec *run.Recovery) (int, string) {
	t.Helper()
	roster, err := cfg.Capture(Agents[name], config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe")
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.Submit(trace.Request{Workflow: name, Repo: repo, Request: request, Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, token := trace.NewAttemptID(id), trace.NewClaimToken()
	if err := db.Reserve(id, attempt, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Claim(attempt, token, "host", os.Getpid(), time.Minute); err != nil {
		t.Fatal(err)
	}
	r, err := run.Open(run.Options{
		ID: id, Workflow: name, Request: request, Repo: repo,
		Dir: filepath.Join(dataRoot, "runs", id), Snapshot: roster, DB: db, Attempt: attempt, Recovery: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Retain(rec.Restored)
	code := Graphs[name](r)
	if err := db.Complete(id, attempt, token, r.Status(), r.Reason()); err != nil {
		t.Fatal(err)
	}
	return code, r.Dir
}

// publishedStub is codeStub with a gh that reports one pull request from
// feat/x at origin's current head, in the state written to pr-state, records
// gh pr ready in gh-ready and counts creations in gh-creates.
func publishedStub(t *testing.T) string {
	t.Helper()
	stub := codeStub(t)
	gh := `#!/bin/sh
d=` + stub + `
case "$1 $2" in
"pr view") oid=$(git ls-remote origin refs/heads/feat/x | cut -f1); state=OPEN; [ ! -f $d/pr-state ] || state=$(cat $d/pr-state)
  printf '{"url":"%s","state":"%s","headRefName":"feat/x","baseRefName":"main","isDraft":true,"headRefOid":"%s","isCrossRepository":false}\n' "$3" "$state" "$oid" ;;
"pr ready") echo "$3" >> $d/gh-ready ;;
"pr create") echo created >> $d/gh-creates; echo https://github.com/acme/widget/pull/8 ;;
*) exit 1 ;;
esac
`
	write(t, filepath.Join(stub, "gh"), gh)
	os.Chmod(filepath.Join(stub, "gh"), 0o755)
	return stub
}

// publishedBranch is shipRepo with feat/x committed and on origin, checked
// out at that commit, as a delivered build leaves it.
func publishedBranch(t *testing.T) (config.Config, string, string, string) {
	t.Helper()
	cfg, repo, origin := shipRepo(t)
	gitBuild(t, repo, "checkout", "-qb", "feat/x")
	write(t, filepath.Join(repo, "hello.txt"), "hello world\n")
	gitBuild(t, repo, "commit", "-qam", "greet")
	gitBuild(t, repo, "push", "-q", "origin", "feat/x")
	return cfg, repo, origin, rev(t, repo, "HEAD")
}

func publishedRecovery(head string, lathe bool) *run.Recovery {
	return &run.Recovery{Guidance: "finish it", Failure: "fail: push", Publication: &run.Publication{
		Branch: "feat/x", Head: head, PR: shipURL, Draft: true, LatheDraft: lathe}}
}

// A commit-before-push recovery reuses its recorded branch: no brancher, a
// push that expects the branch absent, and one pull request.
func TestRecoveredBuildReusesItsUnpublishedBranch(t *testing.T) {
	cfg, repo, origin := shipRepo(t)
	stub := publishedStub(t)
	write(t, filepath.Join(repo, "hello.txt"), "hello world\n")
	rec := &run.Recovery{Guidance: "push it", Failure: "fail: push", Restored: []string{"hello.txt"},
		Publication: &run.Publication{Branch: "feat/old"}}
	if code := executeRecovery(t, cfg, "build", repo, "greet the world", rec); code != 0 {
		t.Fatalf("code = %d: %s", code, latestRun(t).Reason)
	}
	if n := spawnsOf(t, stub, "brancher"); n != 0 {
		t.Fatalf("brancher spawned %d times", n)
	}
	if got := strings.TrimSpace(gitBuild(t, origin, "rev-parse", "feat/old")); got != rev(t, repo, "HEAD") {
		t.Fatalf("origin/feat/old = %s", got)
	}
	if creates := stubFile(t, stub, "gh-creates"); strings.Count(creates, "created") != 1 {
		t.Fatalf("pull requests created: %q", creates)
	}
}

// Accepted work updates the existing pull request and readies lathe's own
// draft; a draft of unknown provenance stays a draft.
func TestRecoveredBuildUpdatesItsPullRequest(t *testing.T) {
	for _, lathe := range []bool{true, false} {
		t.Run(map[bool]string{true: "lathe draft", false: "unknown draft"}[lathe], func(t *testing.T) {
			cfg, repo, origin, head := publishedBranch(t)
			stub := publishedStub(t)
			write(t, filepath.Join(stub, "builder.sh"), "printf 'hello there\\n' > hello.txt\n")
			if code := executeRecovery(t, cfg, "build", repo, "greet the world", publishedRecovery(head, lathe)); code != 0 {
				t.Fatalf("code = %d: %s", code, latestRun(t).Reason)
			}
			if got := strings.TrimSpace(gitBuild(t, origin, "rev-parse", "feat/x")); got == head || got != rev(t, repo, "HEAD") {
				t.Fatalf("origin/feat/x = %s", got)
			}
			if stubFile(t, stub, "gh-creates") != "" || spawnsOf(t, stub, "brancher") != 0 {
				t.Fatal("a second branch or pull request was made")
			}
			if readied := stubFile(t, stub, "gh-ready") != ""; readied != lathe {
				t.Fatalf("readied = %v", readied)
			}
		})
	}
}

// Unaccepted work leaves an existing pull request unchanged.
func TestUnacceptedRecoveryLeavesThePullRequest(t *testing.T) {
	cfg, repo, origin, head := publishedBranch(t)
	stub := publishedStub(t)
	write(t, filepath.Join(stub, "builder.sh"), "printf 'hello there\\n' > hello.txt\n")
	write(t, filepath.Join(stub, "code-review-general"), piReply(t, "not an envelope"))
	if code := executeRecovery(t, cfg, "build", repo, "greet the world", publishedRecovery(head, true)); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if got := strings.TrimSpace(gitBuild(t, origin, "rev-parse", "feat/x")); got != head {
		t.Fatal("unaccepted work was pushed")
	}
	if stubFile(t, stub, "gh-ready") != "" || !strings.Contains(latestRun(t).Reason, "is unchanged") {
		t.Fatalf("pull request touched: %s", latestRun(t).Reason)
	}
}

// A pull request closed, or origin moved, while the recovery validates is
// caught immediately before delivery: nothing is pushed, and completing with
// no new code is refused.
func TestRecoveredDeliveryRechecksPublication(t *testing.T) {
	for _, tc := range []struct{ name, tester, builder string }{
		{"closed", "echo CLOSED > " + "$d/pr-state\n", "printf 'hello there\\n' > hello.txt\n"},
		{"moved", "c=$(git commit-tree HEAD^{tree} -p HEAD -m external); git push -q -f origin $c:refs/heads/feat/x\n", ":\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo, origin, head := publishedBranch(t)
			stub := publishedStub(t)
			write(t, filepath.Join(stub, "builder.sh"), tc.builder)
			if tc.name == "moved" {
				write(t, filepath.Join(stub, "builder"), jsonReply(t, map[string]any{
					"summary": "already done", "changed": []string{}, "needed": []string{}, "artifacts": []string{}}))
			}
			write(t, filepath.Join(stub, "tester.sh"), tc.tester)
			if code := executeRecovery(t, cfg, "build", repo, "greet the world", publishedRecovery(head, true)); code != 1 {
				t.Fatalf("code = %d", code)
			}
			if tc.name == "closed" {
				if got := strings.TrimSpace(gitBuild(t, origin, "rev-parse", "feat/x")); got != head {
					t.Fatal("pushed to a closed pull request")
				}
			}
			if reason := latestRun(t).Reason; !strings.Contains(reason, "nothing was published") {
				t.Fatalf("reason = %q", reason)
			}
			if stubFile(t, stub, "gh-ready") != "" {
				t.Fatal("readied a pull request after the recheck failed")
			}
		})
	}
}

// A run that fails during an adjudicator repair, after a scope replan
// dismissed one finding, leaves unsettled validation. Resuming it carries the
// finding still outstanding to the planner and builder, and the dismissed one
// stays dismissed.
func TestRecoveryCarriesFindingsFromAnUnsettledRepair(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "code-review-general.0"), reviewerReply(t, "MOOT-FINDING", "CARRIED-FINDING"))
	write(t, filepath.Join(stub, "adjudicator.0"), adjudicatorReply(t, "revise", "fix both",
		[]map[string]string{decide("general-1.1", "fix"), decide("general-1.2", "fix")}, nil))
	write(t, filepath.Join(stub, "builder.1"), neededReply(t, []string{"hello.txt"}, []string{"extra.txt"}, ""))
	write(t, filepath.Join(stub, "planner.1"), planWith(t, []string{"hello.txt"}, map[string]any{
		"dismissals": []map[string]string{{"ref": "general-1.1", "reason": "MOOT-REASON"}}}))
	for i := 2; i <= 4; i++ {
		write(t, filepath.Join(stub, fmt.Sprintf("builder.%d", i)), piReply(t, "not an envelope"))
	}
	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
		t.Fatalf("first iteration code = %d", code)
	}
	prior := latestRunDir(t)
	if v := readValidation(t); v.Summary != "" || len(v.Dismissals) != 1 {
		t.Fatalf("the repair's validation should be unsettled with the dismissal saved: %+v", v)
	}

	planners, builders := spawnsOf(t, stub, "planner"), spawnsOf(t, stub, "builder")
	rec := &run.Recovery{Guidance: "finish the repair", Failure: "fail: builder", Restored: []string{"hello.txt"},
		Prior: []run.Prior{{Iteration: 0, Dir: prior}}}
	if code := executeRecovery(t, cfg, "implement", repo, "greet the world", rec); code != 0 {
		t.Fatalf("recovery code = %d: %s", code, latestRun(t).Reason)
	}
	for name, args := range map[string]string{
		"planner": stubFile(t, stub, fmt.Sprintf("args.planner.%d", planners)),
		"builder": stubFile(t, stub, fmt.Sprintf("args.builder.%d", builders)),
	} {
		if !strings.Contains(args, "CARRIED-FINDING") || strings.Contains(args, "MOOT-FINDING") {
			t.Fatalf("%s was not handed exactly the outstanding finding: %s", name, args)
		}
	}
}

// failedRepair arranges the stub's next iteration: its first round raises
// explanation as general-1.1, sent back for repair, and its repair builder
// never reports.
func failedRepair(t *testing.T, stub, explanation string) {
	t.Helper()
	general, adjudicator, builder := spawnsOf(t, stub, "code-review-general"), spawnsOf(t, stub, "adjudicator"), spawnsOf(t, stub, "builder")
	write(t, filepath.Join(stub, fmt.Sprintf("code-review-general.%d", general)), reviewerReply(t, explanation))
	write(t, filepath.Join(stub, fmt.Sprintf("adjudicator.%d", adjudicator)), adjudicatorReply(t, "revise", "fix it",
		[]map[string]string{decide("general-1.1", "fix")}, nil))
	for i := builder + 1; i <= builder+3; i++ {
		write(t, filepath.Join(stub, fmt.Sprintf("builder.%d", i)), piReply(t, "not an envelope"))
	}
}

// A dismissal binds the finding of the iteration it was made against, not
// its ref: a recovery dismisses the original general-1.1, its own fresh
// validation raises a different general-1.1 whose repair fails, and the next
// recovery still carries the new finding to the planner and builder.
func TestRecoveryDismissalsDoNotSuppressReusedRefs(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	failedRepair(t, stub, "ORIGINAL-FINDING")
	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
		t.Fatalf("first iteration code = %d", code)
	}
	first := latestRunDir(t)

	write(t, filepath.Join(stub, fmt.Sprintf("planner.%d", spawnsOf(t, stub, "planner"))), planWith(t, []string{"hello.txt"}, map[string]any{
		"dismissals": []map[string]string{{"ref": "general-1.1", "reason": "OLD-REASON"}}}))
	failedRepair(t, stub, "NEW-FINDING")
	recovery := &run.Recovery{Guidance: "retry", Failure: "fail: builder", Restored: []string{"hello.txt"},
		Prior: []run.Prior{{Iteration: 0, Dir: first}}}
	code, second := recoverIn(t, cfg, "implement", repo, "greet the world", recovery)
	if code != 1 {
		t.Fatalf("first recovery code = %d", code)
	}

	planners, builders := spawnsOf(t, stub, "planner"), spawnsOf(t, stub, "builder")
	recovery = &run.Recovery{Guidance: "retry again", Source: 1, Failure: "fail: builder", Restored: []string{"hello.txt"},
		Prior: []run.Prior{{Iteration: 0, Dir: first}, {Iteration: 1, Dir: second}}}
	if code, _ := recoverIn(t, cfg, "implement", repo, "greet the world", recovery); code != 0 {
		t.Fatalf("second recovery code = %d", code)
	}
	for name, args := range map[string]string{
		"planner": stubFile(t, stub, fmt.Sprintf("args.planner.%d", planners)),
		"builder": stubFile(t, stub, fmt.Sprintf("args.builder.%d", builders)),
	} {
		if !strings.Contains(args, "NEW-FINDING") || strings.Contains(args, "ORIGINAL-FINDING") {
			t.Fatalf("%s was not handed the new finding alone: %s", name, args)
		}
	}
}

// A later recovery's dismissal of an inherited finding holds even though
// that recovery never validated: the next recovery does not carry it.
func TestLaterDispositionSuppressesItsInheritedFinding(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	failedRepair(t, stub, "ORIGINAL-FINDING")
	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 1 {
		t.Fatalf("first iteration code = %d", code)
	}
	first := latestRunDir(t)

	write(t, filepath.Join(stub, fmt.Sprintf("planner.%d", spawnsOf(t, stub, "planner"))), planWith(t, []string{"hello.txt"}, map[string]any{
		"dismissals": []map[string]string{{"ref": "general-1.1", "reason": "MOOT-REASON"}}}))
	for i, n := 0, spawnsOf(t, stub, "builder"); i < 3; i++ {
		write(t, filepath.Join(stub, fmt.Sprintf("builder.%d", n+i)), piReply(t, "not an envelope"))
	}
	recovery := &run.Recovery{Guidance: "retry", Failure: "fail: builder", Restored: []string{"hello.txt"},
		Prior: []run.Prior{{Iteration: 0, Dir: first}}}
	code, second := recoverIn(t, cfg, "implement", repo, "greet the world", recovery)
	if code != 1 {
		t.Fatalf("first recovery code = %d", code)
	}

	planners, builders := spawnsOf(t, stub, "planner"), spawnsOf(t, stub, "builder")
	recovery = &run.Recovery{Guidance: "retry again", Source: 1, Failure: "fail: builder", Restored: []string{"hello.txt"},
		Prior: []run.Prior{{Iteration: 0, Dir: first}, {Iteration: 1, Dir: second}}}
	if code, _ := recoverIn(t, cfg, "implement", repo, "greet the world", recovery); code != 0 {
		t.Fatalf("second recovery code = %d", code)
	}
	for _, args := range []string{
		stubFile(t, stub, fmt.Sprintf("args.planner.%d", planners)),
		stubFile(t, stub, fmt.Sprintf("args.builder.%d", builders)),
	} {
		if strings.Contains(args, "ORIGINAL-FINDING") {
			t.Fatalf("a dismissed inherited finding was carried again: %s", args)
		}
	}
}

// A recovery whose final review is unusable at the limit stops before its
// plan's reversals and dismissals apply: restored work and prior findings
// stay as they were, the candidate plan is saved, and nothing is built.
func TestUnreviewedRecoveryPlanAppliesNothing(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	prior := t.TempDir()
	finding := Finding{Ref: "general-1.1", Source: "code-review-general", Evidence: "e", Explanation: "OUTSTANDING", Outcome: "fix"}
	verdict, changes := verdictRevise, "fix it"
	v := Validation{Outcome: outcomeUnresolved, Rounds: []*Round{{
		Reports:      map[string]*CodeReviewOutput{"code-review-general": {Findings: &[]Finding{finding}}},
		Adjudication: &AdjudicationOutput{Verdict: &verdict, Changes: &changes, Decisions: &[]Decision{{Ref: finding.Ref, Action: actionFix, Reason: "r"}}},
	}}}
	if err := writeResult(prior, "validation.json", &v); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "hello.txt"), "RESTORED\n")
	write(t, filepath.Join(repo, "stale.txt"), "STALE-WORK\n")
	write(t, filepath.Join(stub, "planner"), planWith(t, []string{"hello.txt"}, map[string]any{
		"reversals":  []map[string]string{{"path": "stale.txt", "reason": "another approach"}},
		"dismissals": []map[string]string{{"ref": "general-1.1", "reason": "MOOT-REASON"}}}))
	write(t, filepath.Join(stub, "plan-reviewer"), piReply(t, "not an envelope"))
	for i := range maxReviewSendBacks {
		write(t, filepath.Join(stub, fmt.Sprintf("plan-reviewer.%d", i)), reviewReply(t, "tighten the steps"))
	}

	rec := &run.Recovery{Guidance: "retry", Failure: "fail: builder", Restored: []string{"hello.txt", "stale.txt"},
		Prior: []run.Prior{{Iteration: 0, Dir: prior}}}
	code, dir := recoverIn(t, cfg, "implement", repo, "greet the world", rec)
	if code != 1 || !strings.Contains(latestRun(t).Reason, "no usable review") {
		t.Fatalf("code = %d: %s", code, latestRun(t).Reason)
	}
	for path, want := range map[string]string{"hello.txt": "RESTORED\n", "stale.txt": "STALE-WORK\n"} {
		if b, _ := os.ReadFile(filepath.Join(repo, path)); string(b) != want {
			t.Fatalf("%s = %q; restored work changed", path, b)
		}
	}
	for _, name := range []string{"abandoned-recovery.patch", "disposition.json", "dismissals.json", "validation.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was written for an unreviewed plan", name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "plan.json")); err != nil || !strings.Contains(string(b), "stale.txt") {
		t.Fatalf("candidate plan not saved: %s %v", b, err)
	}
	if b, t2 := spawnsOf(t, stub, "builder"), spawnsOf(t, stub, "tester"); b != 0 || t2 != 0 {
		t.Fatalf("builder %d, tester %d after an unreviewed recovery plan", b, t2)
	}
	if got := outstandingOf(&v, nil); len(got) != 1 {
		t.Fatal("the prior finding is no longer outstanding")
	}
}

// A dismissal recorded in every artifact an iteration writes is read once;
// an iteration without dismissals.json falls back to its other artifacts.
func TestDismissalArtifacts(t *testing.T) {
	source := 0
	d := Dismissal{Ref: "general-1.1", Reason: "ONCE-REASON", From: &source}
	complete := t.TempDir()
	writeResult(complete, "dismissals.json", []Dismissal{d})
	writeResult(complete, "disposition.json", &Disposition{Dismissed: []Dismissal{d}})
	writeResult(complete, "validation.json", &Validation{Dismissals: []Dismissal{d}})
	if got := dismissalsIn(run.Prior{Iteration: 1, Dir: complete}); len(got) != 1 {
		t.Fatalf("complete artifact = %+v", got)
	}
	empty := t.TempDir()
	writeResult(empty, "dismissals.json", []Dismissal{})
	writeResult(empty, "disposition.json", &Disposition{Dismissed: []Dismissal{d}})
	if got := dismissalsIn(run.Prior{Iteration: 1, Dir: empty}); len(got) != 0 {
		t.Fatalf("an empty complete list was ignored: %+v", got)
	}
	legacy := t.TempDir()
	writeResult(legacy, "disposition.json", &Disposition{Dismissed: []Dismissal{d}})
	writeResult(legacy, "validation.json", &Validation{Dismissals: []Dismissal{d}})
	if got := dismissalsIn(run.Prior{Iteration: 1, Dir: legacy}); len(got) != 1 {
		t.Fatalf("legacy fallback = %+v", got)
	}

	// Rendered once in the recovery context, and the finding is not carried.
	first := t.TempDir()
	finding := Finding{Ref: "general-1.1", Source: "code-review-general", Evidence: "e", Explanation: "DISMISSED-FINDING", Outcome: "fix"}
	verdict, changes := verdictRevise, "fix it"
	writeResult(first, "validation.json", &Validation{Outcome: outcomeUnresolved, Rounds: []*Round{{
		Reports:      map[string]*CodeReviewOutput{"code-review-general": {Findings: &[]Finding{finding}}},
		Adjudication: &AdjudicationOutput{Verdict: &verdict, Changes: &changes, Decisions: &[]Decision{{Ref: finding.Ref, Action: actionFix, Reason: "r"}}},
	}}})
	later := t.TempDir()
	writeResult(later, "dismissals.json", []Dismissal{d})
	writeResult(later, "disposition.json", &Disposition{Dismissed: []Dismissal{d}})
	var c code
	got := recoveryContext(&run.Run{Recovery: &run.Recovery{Guidance: "g", Source: 1, Failure: "fail",
		Prior: []run.Prior{{Iteration: 0, Dir: first}, {Iteration: 1, Dir: later}}}}, &c)
	if n := strings.Count(got, "ONCE-REASON"); n != 1 || len(c.outstanding) != 0 {
		t.Fatalf("dismissal rendered %d times, outstanding %+v:\n%s", n, c.outstanding, got)
	}
}

// A recovery's reviewed plan abandons a restored file and dismisses a prior
// finding before the builder runs: the abandoned work is saved and absent
// from the delivered change, the dismissal and its reason are recorded, and
// the finding not dismissed is carried to the builder.
func TestRecoveryAppliesTheReviewedDisposition(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	prior := t.TempDir()
	moot := Finding{Ref: "general-1.1", Source: "code-review-general", Evidence: "e", Explanation: "MOOT-FINDING", Outcome: "fix"}
	carried := Finding{Ref: "general-1.2", Source: "code-review-general", Evidence: "e", Explanation: "CARRIED-FINDING", Outcome: "fix"}
	verdict, changes := verdictRevise, "fix both"
	v := Validation{Summary: "unresolved", Outcome: outcomeUnresolved, Rounds: []*Round{{
		Reports: map[string]*CodeReviewOutput{"code-review-general": {Findings: &[]Finding{moot, carried}}},
		Adjudication: &AdjudicationOutput{Verdict: &verdict, Changes: &changes, Decisions: &[]Decision{
			{Ref: moot.Ref, Action: actionFix, Reason: "r"}, {Ref: carried.Ref, Action: actionFix, Reason: "r"}}},
	}}}
	if err := writeResult(prior, "validation.json", &v); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "hello.txt"), "RESTORED\n")
	write(t, filepath.Join(repo, "stale.txt"), "STALE-WORK\n")
	write(t, filepath.Join(stub, "planner"), planWith(t, []string{"hello.txt"}, map[string]any{
		"reversals":  []map[string]string{{"path": "stale.txt", "reason": "another approach"}},
		"dismissals": []map[string]string{{"ref": "general-1.1", "reason": "MOOT-REASON"}}}))

	rec := &run.Recovery{Guidance: "use the feedback", Source: 0, Failure: "fail: scope",
		Restored: []string{"hello.txt", "stale.txt"}, Prior: []run.Prior{{Iteration: 0, Dir: prior}}}
	if code := executeRecovery(t, cfg, "implement", repo, "greet the world", rec); code != 0 {
		t.Fatalf("code = %d: %s", code, latestRun(t).Reason)
	}
	if _, err := os.Stat(filepath.Join(repo, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("abandoned work survived into the change")
	}
	dir := latestRunDir(t)
	if b, err := os.ReadFile(filepath.Join(dir, "abandoned-recovery.patch")); err != nil || !strings.Contains(string(b), "STALE-WORK") {
		t.Fatalf("abandoned work not saved: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "disposition.json")); err != nil || !strings.Contains(string(b), "MOOT-REASON") {
		t.Fatalf("disposition not recorded: %s %v", b, err)
	}
	got := readValidation(t)
	if len(got.Dismissals) != 1 || got.Dismissals[0].Ref != "general-1.1" || got.Dismissals[0].Reason != "MOOT-REASON" {
		t.Fatalf("dismissals = %+v", got.Dismissals)
	}
	builder := stubFile(t, stub, "args.builder.0")
	if !strings.Contains(builder, "CARRIED-FINDING") || strings.Contains(builder, "MOOT-FINDING") {
		t.Fatalf("findings handed to the builder: %s", builder)
	}
	if planner := stubFile(t, stub, "args.planner.0"); !strings.Contains(planner, "use the feedback") || !strings.Contains(planner, "MOOT-FINDING") {
		t.Fatalf("planner context: %s", planner)
	}
}

// A recovery's planner sees the guidance, the source failure, prior reports
// labelled as prior, the restored work and outstanding findings, which the
// plan may then dismiss.
func TestRecoveryContext(t *testing.T) {
	dir := t.TempDir()
	summary, files, empty := "PRIOR-PLAN", []string{"hello.txt"}, []string{}
	steps := []string{"edit hello.txt"}
	if err := writeResult(dir, "plan.json", &PlanOutput{Summary: &summary, Steps: &steps, Files: &files, Risks: &empty, Wrote: &empty}); err != nil {
		t.Fatal(err)
	}
	impl, needed := "BLOCKED-FOR-SCOPE", []string{"extra.txt"}
	if err := writeResult(dir, "implement.json", &ImplementOutput{Summary: &impl, Changed: &files, Needed: &needed, Wrote: &empty}); err != nil {
		t.Fatal(err)
	}
	finding := Finding{Ref: "general-1.1", Source: "code-review-general", Evidence: "e", Explanation: "STILL-LOUD", Outcome: "fix"}
	rep := &CodeReviewOutput{Findings: &[]Finding{finding}}
	verdict, changes := verdictRevise, "soften"
	adj := &AdjudicationOutput{Verdict: &verdict, Changes: &changes, Decisions: &[]Decision{{Ref: "general-1.1", Action: actionFix, Reason: "r"}}}
	v := Validation{Summary: "unresolved after 1 round(s)", Outcome: outcomeUnresolved,
		Rounds: []*Round{{Reports: map[string]*CodeReviewOutput{"code-review-general": rep}, Adjudication: adj}}}
	if err := writeResult(dir, "validation.json", &v); err != nil {
		t.Fatal(err)
	}

	r := &run.Run{Recovery: &run.Recovery{Guidance: "USE-THE-FEEDBACK", Source: 0, Failure: "fail: scope",
		Restored: []string{"hello.txt"}, Prior: []run.Prior{{Iteration: 0, Dir: dir}}}}
	var c code
	got := recoveryContext(r, &c)
	for _, want := range []string{"USE-THE-FEEDBACK", "fail: scope", "Prior plan", "PRIOR-PLAN", "BLOCKED-FOR-SCOPE", "extra.txt", "STILL-LOUD", "hello.txt", "are prior"} {
		if !strings.Contains(got, want) {
			t.Fatalf("context missing %q:\n%s", want, got)
		}
	}
	if len(c.outstanding) != 1 || c.outstanding[0].Ref != "general-1.1" {
		t.Fatalf("outstanding = %+v", c.outstanding)
	}
	if recoveryContext(&run.Run{}, nil) != "" {
		t.Fatal("context for an ordinary iteration")
	}
}
