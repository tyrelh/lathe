package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/trace"
)

// codeStub puts a `pi` on PATH that works out which agent it is from the system
// prompt and serves dir/<agent>, or dir/<agent>.<k> on that agent's kth spawn
// counting from zero when the file exists. dir/<agent>.sh, when present, runs
// first in the target repository. Counters, arguments and scope copies are per
// agent, because the validation group spawns its workers at once and one shared
// counter would race.
//
// With dir/barrier present, each validation worker waits on its first spawn
// until every worker has started, and records dir/serial if they never do; the
// adjudicator records dir/early if it starts before every worker has finished.
//
// Every agent's default reply is the happy path, and a `gh` beside it records
// its arguments and prints a URL.
func codeStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "planner"), planReply(t, `"hello.txt"`))
	write(t, filepath.Join(dir, "plan-reviewer"), reviewOK(t))
	write(t, filepath.Join(dir, "brancher"), jsonReply(t, map[string]any{
		"summary": "the history prefixes with feat/", "branch": "feat/greet-the-world", "artifacts": []string{}}))
	write(t, filepath.Join(dir, "builder.sh"), "printf 'hello world\\n' > hello.txt\n")
	write(t, filepath.Join(dir, "builder"), builderReply(t, "updated greeting"))
	write(t, filepath.Join(dir, "tester"), testerReply(t, "true"))
	for _, name := range reviewers {
		write(t, filepath.Join(dir, name), reviewerReply(t))
	}
	write(t, filepath.Join(dir, "adjudicator"), acceptReply(t))
	write(t, filepath.Join(dir, "committer"), jsonReply(t, map[string]any{
		"summary":   "conventional commits, as the history does",
		"message":   "feat: greet the world\n\nThe greeting now names its audience.",
		"artifacts": []string{}}))
	write(t, filepath.Join(dir, "pr-author"), jsonReply(t, map[string]any{
		"summary": "filled in the template", "title": "feat: greet the world",
		"body": "## What\n\nGreets the world.\n", "artifacts": []string{}}))

	// gh pr view answers from gh-states, one state per view in order (the last
	// repeats; OPEN without the file), so a test can close a pull request at a
	// chosen moment. Every gh pr create is counted in gh-creates.
	gh := fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = "pr view" ]; then
  n=0; [ ! -f %[1]q/gh-views ] || n=$(cat %[1]q/gh-views); echo $((n+1)) > %[1]q/gh-views
  state=OPEN
  if [ -f %[1]q/gh-states ]; then
    state=$(sed -n "$((n+1))p" %[1]q/gh-states); [ -n "$state" ] || state=$(tail -n 1 %[1]q/gh-states)
  fi
  base=main; [ ! -f %[1]q/gh-base ] || base=$(cat %[1]q/gh-base)
  printf '{"url":"%[2]s","state":"%%s","headRefName":"feat/greet-the-world","baseRefName":"%%s"}\n' "$state" "$base"
  exit 0
fi
echo created >> %[1]q/gh-creates
printf '%%s\n' "$@" > %[1]q/gh-args
for a in "$@"; do [ ! -f "$a" ] || cp "$a" %[1]q/gh-body; done
if [ -f %[1]q/gh-fail ]; then echo "no git remote found" >&2; exit 1; fi
echo "Creating pull request for feat/x into main"
echo %[2]q
`, dir, shipURL)
	write(t, filepath.Join(dir, "gh"), gh)
	if err := os.Chmod(filepath.Join(dir, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}

	onPath(t, dir, fmt.Sprintf(`#!/bin/sh
d=%[1]q
a=pr-author
for name in planner plan-reviewer brancher builder tester code-review-general code-review-security code-review-slop code-review-adversarial adjudicator committer; do
  case "$*" in *"You are the $name "*|*"You are the $name."*|*"You are the $name,"*) a=$name; break ;; esac
done
n=0
[ ! -f "$d/count.$a" ] || n=$(cat "$d/count.$a")
echo $((n+1)) > "$d/count.$a"
printf '%%s\n' "$@" > "$d/args.$a.$n"
cp "$LATHE_PERMIT" "$d/scope.$a.$n"
if [ -f "$d/barrier" ] && [ "$n" = 0 ]; then
  case $a in tester|code-review-*)
    touch "$d/in.$a"; i=0
    while [ "$(ls "$d" | grep -c '^in\.')" -lt %[2]d ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
    [ $i -lt 200 ] || touch "$d/serial" ;;
  esac
fi
if [ "$a" = adjudicator ] && [ "$(ls "$d" | grep -c '^done\.')" -lt %[2]d ]; then touch "$d/early"; fi
[ ! -f "$d/$a.sh" ] || . "$d/$a.sh"
if [ -f "$d/$a.$n" ]; then cat "$d/$a.$n"; else cat "$d/$a"; fi
case $a in tester|code-review-*) touch "$d/done.$a" ;; esac
`, dir, len(validators)))
	return dir
}

func builderReply(t *testing.T, summary string) string {
	return jsonReply(t, map[string]any{
		"summary": summary, "changed": []string{"hello.txt"}, "needed": []string{}, "artifacts": []string{"hello.txt"}})
}

func testerReply(t *testing.T, command string) string {
	return jsonReply(t, map[string]any{
		"summary": "discovered", "command": command, "failures": []string{"TESTER-OBSERVATION"},
		"coverage": "unchanged", "artifacts": []string{}})
}

// reviewerReply approves, or raises one finding per explanation given.
func reviewerReply(t *testing.T, explanations ...string) string {
	findings := []map[string]string{}
	for _, e := range explanations {
		findings = append(findings, map[string]string{
			"location": "hello.txt:1", "evidence": "hello.txt reads hello world",
			"explanation": e, "outcome": "fix it"})
	}
	return jsonReply(t, map[string]any{"summary": "reviewed", "findings": findings, "artifacts": []string{}})
}

func acceptReply(t *testing.T, decisions ...map[string]string) string {
	return adjudicatorReply(t, "accept", "", decisions, nil)
}

func adjudicatorReply(t *testing.T, verdict, changes string, decisions []map[string]string, scope []map[string]string) string {
	if decisions == nil {
		decisions = []map[string]string{}
	}
	if scope == nil {
		scope = []map[string]string{}
	}
	return jsonReply(t, map[string]any{
		"summary": "adjudicated", "verdict": verdict, "decisions": decisions, "changes": changes,
		"scope": scope, "amendments": []map[string]string{}, "artifacts": []string{}})
}

func decide(ref, action string) map[string]string {
	return map[string]string{"ref": ref, "action": action, "reason": "because " + ref}
}

// spawnsOf is how many times one agent was spawned.
func spawnsOf(t *testing.T, dir, agent string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "count."+agent))
	if os.IsNotExist(err) {
		return 0
	}
	var n int
	fmt.Sscan(string(b), &n)
	return n
}

func stubFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}

func readValidation(t *testing.T) Validation {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(latestRunDir(t), "validation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v Validation
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func runPhases(t *testing.T) []trace.PhaseRow {
	t.Helper()
	db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	phases, err := db.Phases(latestRun(t).ID)
	if err != nil {
		t.Fatal(err)
	}
	return phases
}

func assertEnvironmentFailure(t *testing.T, stub, dir, command string, exit, iteration int) Validation {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "validation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v Validation
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Outcome != outcomeEnvironmentFailed || v.Accepted() || len(v.Rounds) != 1 || !strings.Contains(v.Reason, fmt.Sprintf("exited %d", exit)) {
		t.Fatalf("validation = %+v", v)
	}
	round := v.Rounds[0]
	if round.Tests == nil || round.Tests.Command == nil || *round.Tests.Command != command || round.Measured == nil ||
		round.Measured.Command != command || round.Measured.Green || round.Measured.Tail != "ENV-TAIL\n" ||
		!strings.Contains(round.Measured.Result, fmt.Sprintf("exited %d", exit)) || round.Adjudication != nil {
		t.Fatalf("lost failed measurement or adjudicated it: %+v", round)
	}
	found := false
	for _, failure := range round.Failures {
		if failure.Worker == "test" && strings.Contains(failure.Error, fmt.Sprintf("exited %d", exit)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing tester failure: %+v", round.Failures)
	}
	for _, name := range []string{"tester", "builder"} {
		if got := spawnsOf(t, stub, name); got != iteration+1 {
			t.Fatalf("%s attempts = %d; wanted one this iteration", name, got)
		}
	}
	for _, name := range []string{"adjudicator", "committer", "pr-author"} {
		if got := spawnsOf(t, stub, name); got != iteration {
			t.Fatalf("environment failure reached %s: %d turns", name, got)
		}
	}
	if got := strings.Count(stubFile(t, stub, "gh-creates"), "created"); got != iteration {
		t.Fatalf("environment failure opened a PR: %d creates", got)
	}
	tests := 0
	for _, ph := range runPhases(t) {
		if ph.Iteration != iteration {
			continue
		}
		switch ph.Name {
		case "test":
			tests++
			if ph.Status != "fail" {
				t.Fatalf("tester status = %s", ph.Status)
			}
		case "adjudicate", "commit", "pr", "publish":
			t.Fatalf("terminal failure forwarded to %s", ph.Name)
		}
	}
	if tests != 1 || latestRun(t).Status != "fail" {
		t.Fatalf("tester phases = %d, run = %+v", tests, latestRun(t))
	}
	return v
}

func TestEnvironmentFailureRetainsJoinedEvidence(t *testing.T) {
	for _, exit := range []int{126, 127} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			cfg, repo := buildRepo(t)
			stub := codeStub(t)
			command := fmt.Sprintf("echo mutated > hello.txt; echo ENV-TAIL; exit %d", exit)
			write(t, filepath.Join(stub, "tester"), testerReply(t, command))
			write(t, filepath.Join(stub, "code-review-security"), piReply(t, "no report"))
			if got := execute(t, cfg, "implement", repo, "greet the world"); got != 1 {
				t.Fatalf("exit = %d", got)
			}
			v := assertEnvironmentFailure(t, stub, latestRunDir(t), command, exit, 0)
			round := v.Rounds[0]
			if strings.Join(round.Mutated, ",") != "hello.txt" || len(round.Failures) != 2 || len(round.Reports) != len(reviewers)-1 {
				t.Fatalf("lost joined evidence: %+v", round)
			}
			if spawnsOf(t, stub, "code-review-security") != 5 {
				t.Fatal("ordinary failed peer was not joined through its retries")
			}
		})
	}
}

func TestOrdinaryRedSuiteStillAdjudicatesAndRepairs(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "tester.0"), testerReply(t, "echo RED-TAIL; exit 7"))
	write(t, filepath.Join(stub, "adjudicator.0"), adjudicatorReply(t, "revise", "fix the suite", nil, nil))
	if got := execute(t, cfg, "implement", repo, "greet the world"); got != 0 {
		t.Fatalf("exit = %d", got)
	}
	v := readValidation(t)
	if v.Outcome != outcomeAccepted || len(v.Rounds) != 2 || v.Rounds[0].Measured.Green || !v.Rounds[1].Measured.Green ||
		!strings.Contains(v.Rounds[0].Measured.Result, "exited 7") || v.Rounds[0].Measured.Tail != "RED-TAIL\n" || len(v.Rounds[0].Failures) != 0 {
		t.Fatalf("ordinary red suite = %+v", v)
	}
	for _, name := range []string{"tester", "builder", "adjudicator"} {
		if spawnsOf(t, stub, name) != 2 {
			t.Fatalf("ordinary red suite did not pass through %s twice", name)
		}
	}
}

func session(t *testing.T, args string) string {
	t.Helper()
	lines := strings.Split(args, "\n")
	for i, v := range lines {
		if v == "--session-id" {
			return lines[i+1]
		}
	}
	t.Fatal("missing session")
	return ""
}

var validators = append([]string{"tester"}, reviewers...)

// The workers overlap, each in its own session and read-only scope, and the
// adjudicator starts only once all of them have finished.
func TestValidationRunsWorkersInParallel(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "barrier"), "")

	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, marker := range []string{"serial", "early"} {
		if _, err := os.Stat(filepath.Join(stub, marker)); err == nil {
			t.Fatalf("%s: the workers did not overlap, or adjudication did not wait for them", marker)
		}
	}
	sessions := map[string]bool{}
	for _, agent := range append(validators, "builder", "adjudicator") {
		if n := spawnsOf(t, stub, agent); n != 1 {
			t.Fatalf("%s spawned %d times", agent, n)
		}
		sessions[session(t, stubFile(t, stub, "args."+agent+".0"))] = true
	}
	if len(sessions) != len(validators)+2 {
		t.Fatalf("sessions shared between agents: %v", sessions)
	}
	for _, agent := range validators {
		if scope := stubFile(t, stub, "scope."+agent+".0"); !strings.Contains(scope, `"allow":[]`) {
			t.Fatalf("%s scope = %s", agent, scope)
		}
	}
	if args := stubFile(t, stub, "args.code-review-slop.0"); !strings.Contains(args, "read,grep,find,ls,web_read\n") ||
		!strings.Contains(args, "Inspect the code as it is now") {
		t.Fatalf("slop reviewer arguments: %s", args)
	}

	v := readValidation(t)
	if v.Outcome != outcomeAccepted || len(v.Rounds) != 1 || !v.Rounds[0].Measured.Green || len(v.Rounds[0].Reports) != len(reviewers) {
		t.Fatalf("validation: %+v", v)
	}
	if row := latestRun(t); row.Status != "ok" {
		t.Fatalf("status = %s", row.Status)
	}
	names := map[string]int{}
	for _, ph := range runPhases(t) {
		names[ph.Name]++
		if ph.Status != "success" {
			t.Fatalf("phase %s: %s", ph.Name, ph.Status)
		}
	}
	for _, want := range append(validators[1:], "test", "validate", "adjudicate", "implement") {
		if names[want] != 1 {
			t.Fatalf("phases: %v", names)
		}
	}
}

// A revise sends the builder one set of changes, every worker reruns against
// the repair with the previous decision in front of it, and a scope addition
// reaches the builder once the gate has refused the protected one.
func TestAdjudicatorSendsBackAndEveryWorkerReruns(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "code-review-general.0"), reviewerReply(t, "GREETING-TOO-LOUD"))
	write(t, filepath.Join(stub, "code-review-slop.0"), reviewerReply(t, "GREETING-OVERWROUGHT"))
	decisions := []map[string]string{decide("general-1.1", "fix"), decide("slop-1.1", "dismiss")}
	write(t, filepath.Join(stub, "adjudicator.0"), adjudicatorReply(t, "revise", "SOFTEN-THE-GREETING",
		decisions, []map[string]string{{"path": ".env", "reason": "config"}}))
	write(t, filepath.Join(stub, "adjudicator.1"), adjudicatorReply(t, "revise", "SOFTEN-THE-GREETING",
		decisions, []map[string]string{{"path": "extra.txt", "reason": "the fix needs it"}}))

	if code := execute(t, cfg, "implement", repo, "greet the world"); code != 0 {
		t.Fatalf("code = %d", code)
	}
	if got := stubFile(t, stub, "args.adjudicator.1"); !strings.Contains(got, `".env" is protected`) {
		t.Fatalf("protected scope addition was not refused: %s", got)
	}
	if n := spawnsOf(t, stub, "builder"); n != 2 {
		t.Fatalf("builder spawns = %d", n)
	}
	repair := stubFile(t, stub, "args.builder.1")
	for _, want := range []string{"SOFTEN-THE-GREETING", "general-1.1", "GREETING-TOO-LOUD", "extra.txt", fmt.Sprintf("send-back 1 of %d", maxRepairs)} {
		if !strings.Contains(repair, want) {
			t.Fatalf("repair request missing %q: %s", want, repair)
		}
	}
	if session(t, repair) != session(t, stubFile(t, stub, "args.builder.0")) {
		t.Fatal("repair lost the builder's session")
	}
	if scope := stubFile(t, stub, "scope.builder.1"); !strings.Contains(scope, `"allow":["hello.txt","extra.txt"]`) {
		t.Fatalf("amended scope = %s", scope)
	}
	for _, agent := range validators {
		if n := spawnsOf(t, stub, agent); n != 2 {
			t.Fatalf("%s spawned %d times; every worker reruns after a repair", agent, n)
		}
		second := stubFile(t, stub, "args."+agent+".1")
		if !strings.Contains(second, "SOFTEN-THE-GREETING") || !strings.Contains(second, "Implementation (round 2)") {
			t.Fatalf("%s's second round lacks the previous decision: %s", agent, second)
		}
	}
	if got := stubFile(t, stub, "args.tester.1"); !strings.Contains(got, "Previous measurement") {
		t.Fatalf("tester was not asked to reassess: %s", got)
	}
	// Every reviewer sees a dismissed finding in full, not only its own.
	if got := stubFile(t, stub, "args.code-review-adversarial.1"); !strings.Contains(got, "Dismissed findings") ||
		!strings.Contains(got, "GREETING-OVERWROUGHT") || strings.Contains(got[strings.Index(got, "Dismissed findings"):], "GREETING-TOO-LOUD") {
		t.Fatalf("dismissed finding not shown in full: %s", got)
	}
	v := readValidation(t)
	if v.Outcome != outcomeAccepted || len(v.Rounds) != 2 || len(v.Scope) != 1 || v.Scope[0].Path != "extra.txt" {
		t.Fatalf("validation: %+v", v)
	}
	if f := (*v.Rounds[0].Reports["code-review-general"].Findings)[0]; f.Ref != "general-1.1" || f.Source != "code-review-general" {
		t.Fatalf("finding not stamped: %+v", f)
	}
}

// A red suite cannot be accepted however the adjudicator words it, and work
// still unaccepted after the last repair is handed off with its findings — after
// that repair got a full round of its own. Implement keeps it local.
func TestSendBacksAreBoundedAndRedIsNeverAccepted(t *testing.T) {
	cfg, repo := buildRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "tester"), testerReply(t, "echo RED-TAIL; false"))
	write(t, filepath.Join(stub, "adjudicator.0"), acceptReply(t))
	write(t, filepath.Join(stub, "adjudicator"), adjudicatorReply(t, "revise", "make the suite pass", nil, nil))

	var out strings.Builder
	if code := executeWith(t, cfg, "implement", repo, "greet the world", context.Background(), &out); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if got := stubFile(t, stub, "args.adjudicator.1"); !strings.Contains(got, "cannot accept while the measured test suite is red") {
		t.Fatalf("red acceptance was not refused: %s", got)
	}
	if n := spawnsOf(t, stub, "builder"); n != maxRepairs+1 {
		t.Fatalf("builder spawns = %d; want the initial implementation and %d repairs", n, maxRepairs)
	}
	for _, agent := range validators {
		if n := spawnsOf(t, stub, agent); n != maxRepairs+1 {
			t.Fatalf("%s spawned %d times; the last repair still gets a full round", agent, n)
		}
	}
	v := readValidation(t)
	if v.Outcome != outcomeUnresolved || len(v.Rounds) != maxRepairs+1 || !strings.Contains(v.Reason, "exited 1") {
		t.Fatalf("validation: %+v", v)
	}
	row := latestRun(t)
	if row.Status != "fail" || !strings.Contains(row.Reason, "validation unresolved") || !strings.Contains(row.Reason, "uncommitted") {
		t.Fatalf("run: %s %q", row.Status, row.Reason)
	}
	if !strings.Contains(out.String(), "validation unresolved") {
		t.Fatalf("terminal: %s", &out)
	}
	if got := gitBuild(t, repo, "status", "--porcelain"); got != " M hello.txt\n" {
		t.Fatalf("implement left %q; want the change local and uncommitted", got)
	}
	if _, err := os.Stat(filepath.Join(stub, "gh-args")); err == nil {
		t.Fatal("implement published")
	}
}

// A worker that fails outright is retried without disturbing its peers or
// spending a repair, and one that recovers leaves the run healthy. Retries
// share the correction allowance: three failed attempts cost five turns, not nine.
func TestWorkerRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recovers bool
	}{{"recovers", true}, {"exhausted", false}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo := buildRepo(t)
			stub := codeStub(t)
			write(t, filepath.Join(stub, "code-review-security"), piReply(t, "no json here"))
			if tc.recovers {
				write(t, filepath.Join(stub, "code-review-security.4"), reviewerReply(t))
			}
			want := 0
			if !tc.recovers {
				want = 1
			}
			if code := execute(t, cfg, "implement", repo, "greet the world"); code != want {
				t.Fatalf("code = %d, want %d", code, want)
			}
			if n := spawnsOf(t, stub, "code-review-security"); n != 5 {
				t.Fatalf("security spawns = %d; want 1 + 2 corrections + 2 retries", n)
			}
			for _, agent := range []string{"tester", "code-review-general", "code-review-slop", "code-review-adversarial", "builder"} {
				if n := spawnsOf(t, stub, agent); n != 1 {
					t.Fatalf("%s spawned %d times", agent, n)
				}
			}
			var statuses []string
			for _, ph := range runPhases(t) {
				if ph.Name == "code-review-security" {
					statuses = append(statuses, ph.Status)
				}
			}
			v := readValidation(t)
			row := latestRun(t)
			if tc.recovers {
				if strings.Join(statuses, ",") != "fail,fail,success" || row.Status != "ok" || v.Outcome != outcomeAccepted {
					t.Fatalf("phases %v, run %s, outcome %s", statuses, row.Status, v.Outcome)
				}
				return
			}
			if strings.Join(statuses, ",") != "fail,fail,fail" || v.Outcome != outcomeIncomplete ||
				len(v.Rounds[0].Failures) != 1 || v.Rounds[0].Failures[0].Worker != "code-review-security" {
				t.Fatalf("phases %v, validation %+v", statuses, v)
			}
			if n := spawnsOf(t, stub, "adjudicator"); n != 0 {
				t.Fatal("incomplete validation reached the adjudicator")
			}
			if !strings.Contains(row.Reason, "validation incomplete") {
				t.Fatalf("reason = %q", row.Reason)
			}
		})
	}
}

// Source changed while the workers ran invalidates the round and publishes
// nothing, and the evidence survives cleanup. New files a suite leaves behind,
// staged or not, are test output: cleaned up, and no mutation.
func TestSourceMutationInvalidatesTheRound(t *testing.T) {
	for _, tc := range []struct {
		name, script, mutated string
	}{
		{"test output is cleaned up", "echo junk > coverage.out; git add coverage.out; echo more > other.out\n", ""},
		{"implementation file", "echo rewritten > hello.txt\n", "hello.txt"},
		{"committed file", "echo rewritten > fixture.txt\n", "fixture.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo, _ := shipRepo(t)
			write(t, filepath.Join(repo, "fixture.txt"), "original\n")
			gitBuild(t, repo, "add", ".")
			gitBuild(t, repo, "commit", "-qm", "fixture")
			stub := codeStub(t)
			write(t, filepath.Join(stub, "tester.sh"), tc.script)

			code := execute(t, cfg, "build", repo, "greet the world")
			if tc.mutated == "" {
				if code != 0 {
					t.Fatalf("code = %d", code)
				}
				if got := gitBuild(t, repo, "status", "--porcelain"); got != "" {
					t.Fatalf("test output survived: %q", got)
				}
				return
			}
			if code != 1 {
				t.Fatalf("code = %d", code)
			}
			v := readValidation(t)
			if v.Outcome != outcomeInvalidated || strings.Join(v.Rounds[0].Mutated, ",") != tc.mutated {
				t.Fatalf("validation: %+v", v)
			}
			if n := spawnsOf(t, stub, "adjudicator") + spawnsOf(t, stub, "committer"); n != 0 {
				t.Fatal("an invalidated round went on")
			}
			if _, err := os.Stat(filepath.Join(stub, "gh-args")); err == nil {
				t.Fatal("published after a source mutation")
			}
			if !strings.Contains(latestRun(t).Reason, "source changed while validation ran") {
				t.Fatalf("reason = %q", latestRun(t).Reason)
			}
		})
	}
}

// Build publishes unaccepted work only as a draft carrying the validation
// report, and the run still fails, so the draft cannot pass for acceptance.
func TestBuildDraftsUnacceptedWork(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		setup         func(t *testing.T, stub string)
		want          []string
	}{
		{"unresolved", outcomeUnresolved, func(t *testing.T, stub string) {
			write(t, filepath.Join(stub, "tester"), testerReply(t, "echo RED-TAIL; false"))
			write(t, filepath.Join(stub, "code-review-slop"), reviewerReply(t, "TOO-CLEVER"))
			for round := 1; round <= maxRepairs+1; round++ {
				write(t, filepath.Join(stub, fmt.Sprintf("adjudicator.%d", round-1)), adjudicatorReply(t, "revise", "simplify it",
					[]map[string]string{decide(fmt.Sprintf("slop-%d.1", round), "fix")}, nil))
			}
		}, []string{"Unresolved findings", "TOO-CLEVER", "RED-TAIL", "echo RED-TAIL; false", "unfinished work"}},
		{"incomplete", outcomeIncomplete, func(t *testing.T, stub string) {
			write(t, filepath.Join(stub, "code-review-general"), piReply(t, "no json here"))
		}, []string{"Missing validation", "code-review-general"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo, origin := shipRepo(t)
			stub := codeStub(t)
			tc.setup(t, stub)

			var out strings.Builder
			if code := executeWith(t, cfg, "build", repo, "greet the world", context.Background(), &out); code != 1 {
				t.Fatalf("code = %d: %s", code, &out)
			}
			if args := stubFile(t, stub, "gh-args"); !strings.Contains(args, "--draft") {
				t.Fatalf("gh arguments: %s", args)
			}
			body := stubFile(t, stub, "gh-body")
			for _, want := range append(tc.want, "## What", "Validation not accepted") {
				if !strings.Contains(body, want) {
					t.Errorf("draft body missing %q: %s", want, body)
				}
			}
			if got := strings.TrimSpace(gitBuild(t, origin, "rev-parse", "feat/greet-the-world")); got != strings.TrimSpace(gitBuild(t, repo, "rev-parse", "HEAD")) {
				t.Fatalf("the draft's branch was not pushed: %s", got)
			}
			if !strings.Contains(stubFile(t, stub, "args.committer.0"), "do not describe it as tested") {
				t.Fatal("committer was not told the change is unaccepted")
			}
			row := latestRun(t)
			if row.Status != "fail" || !strings.Contains(row.Reason, "draft pull request "+shipURL) ||
				!strings.Contains(row.Reason, "validation "+tc.outcome) {
				t.Fatalf("run: %s %q", row.Status, row.Reason)
			}
			var pr struct {
				URL, Validation string
				Draft           bool
			}
			b, _ := os.ReadFile(filepath.Join(latestRunDir(t), "pr.json"))
			if err := json.Unmarshal(b, &pr); err != nil || !pr.Draft || pr.Validation != tc.outcome || pr.URL != shipURL {
				t.Fatalf("pr.json = %s", b)
			}
		})
	}
}

// Cancelling while the workers run stops them all and publishes nothing.
func TestCancelDuringValidationPublishesNothing(t *testing.T) {
	cfg, repo, _ := shipRepo(t)
	stub := codeStub(t)
	write(t, filepath.Join(stub, "code-review-slop.sh"), "touch "+filepath.Join(stub, "reviewing")+"; sleep 30\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.After(10 * time.Second)
		for {
			select {
			case <-deadline:
				cancel()
				return
			case <-time.After(10 * time.Millisecond):
				if _, err := os.Stat(filepath.Join(stub, "reviewing")); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	start := time.Now()
	if code := executeWith(t, cfg, "build", repo, "greet the world", ctx, &strings.Builder{}); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("cancellation waited out the sleeping worker")
	}
	for _, agent := range []string{"adjudicator", "committer", "pr-author"} {
		if n := spawnsOf(t, stub, agent); n != 0 {
			t.Fatalf("%s ran after cancellation", agent)
		}
	}
	if _, err := os.Stat(filepath.Join(stub, "gh-args")); err == nil {
		t.Fatal("published after cancellation")
	}
	if n := spawnsOf(t, stub, "code-review-slop"); n != 1 {
		t.Fatalf("cancellation was retried: %d spawns", n)
	}
}

func TestAdjudicatedGate(t *testing.T) {
	findings := []Finding{{Ref: "general-1.1"}, {Ref: "slop-1.1"}}
	green, red := &Measured{Green: true}, &Measured{}
	accept, revise := verdictAccept, verdictRevise
	for _, tc := range []struct {
		name      string
		verdict   *string
		decisions []Decision
		scope     []ScopeAddition
		tests     *Measured
		want      []string
	}{
		{"complete", &revise, []Decision{{"general-1.1", "fix", "x"}, {"slop-1.1", "dismiss", "y"}}, nil, red, nil},
		{"missing and invented", &revise, []Decision{{"general-1.1", "fix", "x"}, {"bug-9.9", "fix", "x"}}, nil, green,
			[]string{`"bug-9.9" is not a finding`, `"slop-1.1" has no decision`}},
		{"twice", &revise, []Decision{{"general-1.1", "fix", "x"}, {"general-1.1", "fix", "x"}, {"slop-1.1", "fix", "x"}}, nil, green,
			[]string{"decided more than once"}},
		{"red waiver", &accept, []Decision{{"general-1.1", "dismiss", "x"}, {"slop-1.1", "dismiss", "x"}}, nil, red,
			[]string{"cannot accept while the measured test suite is red"}},
		{"bad scope", &revise, []Decision{{"general-1.1", "fix", "x"}, {"slop-1.1", "fix", "x"}},
			[]ScopeAddition{{"../x", "r"}, {"*.go", "r"}, {"keys/a.pem", "r"}}, green,
			[]string{"not inside the repository", "looks like a glob", "is protected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := tc.scope
			if scope == nil {
				scope = []ScopeAddition{}
			}
			a := &AdjudicationOutput{Verdict: tc.verdict, Decisions: &tc.decisions, Scope: &scope}
			got := Adjudicated(findings, tc.tests, []string{"*.pem"})(a, nil)
			if len(got) != len(tc.want) {
				t.Fatalf("violations %v, want %v", got, tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Fatalf("violation %d = %q, want %q", i, got[i], w)
				}
			}
		})
	}
}

func TestAdjudicationOutputValidate(t *testing.T) {
	if v := (&AdjudicationOutput{}).Validate(); len(v) != 7 {
		t.Fatalf("violations: %v", v)
	}
	summary, empty := "s", ""
	decisions := []Decision{{"general-1.1", "fix", "r"}}
	none, noScope, noAmend := []string{}, []ScopeAddition{}, []Amendment{}
	for _, tc := range []struct {
		verdict, changes, want string
	}{
		{verdictRevise, "", `"revise" verdict needs "changes"`},
		{verdictAccept, "", `decided "general-1.1" must be fixed`},
		{verdictRevise, "do it", ""},
	} {
		a := &AdjudicationOutput{Summary: &summary, Verdict: &tc.verdict, Decisions: &decisions,
			Changes: &tc.changes, Scope: &noScope, Amendments: &noAmend, Wrote: &none}
		v := a.Validate()
		if (tc.want == "") != (len(v) == 0) || (tc.want != "" && !strings.Contains(v[0], tc.want)) {
			t.Fatalf("%s %q: %v", tc.verdict, tc.changes, v)
		}
	}
	_ = empty
}

func TestCodeReviewOutputValidate(t *testing.T) {
	if v := (&CodeReviewOutput{}).Validate(); len(v) != 3 {
		t.Fatalf("violations: %v", v)
	}
	summary, none := "s", []string{}
	findings := []Finding{{Location: "", Evidence: "e", Explanation: "x", Outcome: ""}}
	if v := (&CodeReviewOutput{Summary: &summary, Findings: &findings, Wrote: &none}).Validate(); len(v) != 1 || !strings.Contains(v[0], `"outcome"`) {
		t.Fatalf("violations: %v", v)
	}
}
