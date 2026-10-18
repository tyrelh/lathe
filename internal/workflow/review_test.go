package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/trace"
)

func reviewReply(t *testing.T, feedback ...string) string {
	t.Helper()
	if feedback == nil {
		feedback = []string{}
	}
	return jsonReply(t, map[string]any{"summary": "reviewed", "feedback": feedback, "blocking": []string{}, "artifacts": []string{}})
}

// blockedReply is a review that says the plan cannot succeed as written.
func blockedReply(t *testing.T, blocking ...string) string {
	t.Helper()
	return jsonReply(t, map[string]any{"summary": "blocked", "feedback": []string{}, "blocking": blocking, "artifacts": []string{}})
}

func reviewOK(t *testing.T) string {
	t.Helper()
	return reviewReply(t)
}

func TestReviewOutput(t *testing.T) {
	if got := (&ReviewOutput{}).Validate(); len(got) != 4 {
		t.Fatalf("missing fields: %v", got)
	}
	for _, body := range []string{
		`{"summary":"reviewed","feedback":[],"blocking":[],"artifacts":[]}`,
		`{"summary":"reviewed","feedback":[],"artifacts":[]}`,
		`{"summary":"reviewed","feedback":null,"blocking":[],"artifacts":[]}`,
		`{"summary":" ","feedback":[],"blocking":[],"artifacts":[]}`,
	} {
		var out ReviewOutput
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		wantOK := strings.Contains(body, `"summary":"reviewed","feedback":[],"blocking":[]`)
		if (len(out.Validate()) == 0) != wantOK {
			t.Fatalf("validation of %s: %v", body, out.Validate())
		}
	}
}

func TestPlanReviewLoop(t *testing.T) {
	t.Parallel()
	initial := planReply(t, `"fetch.go"`)
	revised := planReply(t, `"fetch.go", "fetch_test.go"`)
	bad := piReply(t, "not an envelope")
	objections := reviewReply(t, "include fetch_test.go", "check the retry limit")
	ceiling := []string{initial}
	for range maxReviewSendBacks {
		ceiling = append(ceiling, objections, revised)
	}
	failedFinal := append(append([]string{}, ceiling...), bad, bad, bad)
	ceiling = append(ceiling, objections)
	last := maxReviewSendBacks + 1
	for _, tc := range []struct {
		name                string
		replies             []string
		reviews             int
		failed              int
		risks               int
		wantLine            string
		retriedAfterFailure bool
	}{
		{"first acceptance", []string{initial, reviewOK(t)}, 1, 0, 0, "", false},
		{"revision", []string{initial, objections, revised, reviewOK(t)}, 2, 0, 0, "plan revised 1 times; reviewer accepted", false},
		{"ceiling", ceiling, last, 0, 2, fmt.Sprintf("plan revised %d times; 2 objections unresolved", maxReviewSendBacks), false},
		{"failed review", []string{initial, bad, bad, bad, revised, reviewOK(t)}, 2, 1, 0, "plan revised 1 times; reviewer accepted", true},
		{"failed final review", failedFinal, last, 1, 1, fmt.Sprintf("plan revised %d times; 1 objections unresolved", maxReviewSendBacks), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stubPi(t, tc.replies...)
			cfg, repo := load(t)
			var terminal strings.Builder
			if code := executeWith(t, cfg, "plan", repo, "add retry", context.Background(), &terminal); code != 0 {
				t.Fatalf("exit %d: %s", code, &terminal)
			}
			if tc.wantLine == "" {
				if strings.Contains(terminal.String(), "plan revised") {
					t.Fatal("revision line on first acceptance")
				}
			} else if !strings.Contains(terminal.String(), tc.wantLine) {
				t.Fatalf("missing %q: %s", tc.wantLine, &terminal)
			}
			b, err := os.ReadFile(filepath.Join(latestRunDir(t), "plan.json"))
			if err != nil {
				t.Fatal(err)
			}
			var plan PlanOutput
			if err := json.Unmarshal(b, &plan); err != nil {
				t.Fatal(err)
			}
			if len(*plan.Risks) != tc.risks {
				t.Fatalf("risks: %v", *plan.Risks)
			}
			if tc.reviews > 1 && strings.Join(*plan.Files, ",") != "fetch.go,fetch_test.go" {
				t.Fatalf("saved initial plan: %s", b)
			}
			for _, risk := range *plan.Risks {
				if !strings.HasPrefix(risk, "unresolved review: ") || !strings.Contains(terminal.String(), risk) || !strings.Contains(implementRequest("add retry", &plan, nil), risk) {
					t.Fatalf("risk missing from terminal or handoff: %s", risk)
				}
				if tc.failed > 0 && !strings.Contains(risk, "review failed") {
					t.Fatalf("failed review disguised as objection: %s", risk)
				}
			}

			db := openTrace(t)
			id := latestRun(t).ID
			phases := runPhases(t)
			first := slices.IndexFunc(phases, func(p trace.PhaseRow) bool { return p.Name == "review" })
			if first < 1 || phases[first-1].Name != "plan" || len(phases) != first+2*tc.reviews-1 {
				t.Fatalf("unexpected phases: %v", phases)
			}
			failed := 0
			for i, ph := range phases[first:] {
				want := "review"
				if i%2 == 1 {
					want = "plan"
				}
				if ph.Name != want {
					t.Fatalf("phase %d: %s, want %s", first+i, ph.Name, want)
				}
				if ph.Status == "fail" {
					failed++
					continue
				}
				status := trace.PhaseSuccess
				if ph.Name == "review" && i < 2*(tc.reviews-1) {
					status = trace.PhaseRequestedChanges
				}
				if ph.Status != status || ph.Error != "" {
					t.Fatalf("phase = %+v, want %s", ph, status)
				}
			}
			if failed != tc.failed {
				t.Fatalf("failed phases %d, want %d", failed, tc.failed)
			}
			events, err := db.Events(id, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			var plannerSession, reviewerSession string
			round := 0
			for _, ev := range events {
				if ev.Type != "input" {
					continue
				}
				var input struct {
					Attempt int    `json:"attempt"`
					Prompt  string `json:"prompt"`
					Session string `json:"session_id"`
				}
				if err := json.Unmarshal(ev.Payload, &input); err != nil {
					t.Fatal(err)
				}
				if input.Attempt != 0 {
					continue
				}
				if ev.Name == "planner" {
					if plannerSession == "" {
						plannerSession = input.Session
					} else if plannerSession != input.Session {
						t.Fatal("replan lost planner session")
					}
					if tc.retriedAfterFailure && round == 1 && !strings.Contains(input.Prompt, "same plan unchanged") {
						t.Fatal("failed review did not permit unchanged plan")
					}
				} else if ev.Name == "plan-reviewer" {
					round++
					want := fmt.Sprintf("Review round %d of %d.\nYou may send this plan back %d more times.", round, last, last-round)
					if !strings.HasPrefix(input.Prompt, want) {
						t.Fatalf("review budget missing: %s", input.Prompt)
					}
					if input.Session == plannerSession {
						t.Fatal("reviewer shares planner session")
					}
					if round > 1 && (input.Session == reviewerSession) == (tc.retriedAfterFailure) {
						t.Fatal("incorrect reviewer session reuse/reset")
					}
					if round > 1 && !strings.Contains(input.Prompt, "fetch_test.go") {
						t.Fatal("reviewer did not receive revised plan")
					}
					reviewerSession = input.Session
				}
			}
			if round != tc.reviews {
				t.Fatalf("review count %d, want %d", round, tc.reviews)
			}
		})
	}
}

func TestReplanRejectsProtectedPath(t *testing.T) {
	t.Parallel()
	bad := planReply(t, `".env"`)
	stubPi(t, planReply(t, `"fetch.go"`), reviewReply(t, "include configuration"), bad, bad, bad)
	cfg, repo := load(t)
	if code := execute(t, cfg, "plan", repo, "retry"); code != 1 {
		t.Fatalf("exit %d, want failure", code)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "plan.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected plan was saved: %v", err)
	}
}

func TestReviewCancellationStopsRun(t *testing.T) {
	cfg, repo := load(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "plan"), planReply(t, `"fetch.go"`))
	onPath(t, dir, fmt.Sprintf(`#!/bin/sh
if [ ! -f %[1]q/planned ]; then
  touch %[1]q/planned
  cat %[1]q/plan
else
  touch %[1]q/reviewing
  sleep 30
fi
`, dir))
	ctx, cancel := cancelOnFile(t, filepath.Join(dir, "reviewing"), 5*time.Second, nil)
	defer cancel()
	var output strings.Builder
	if code := executeWith(t, cfg, "plan", repo, "retry", ctx, &output); code != 1 {
		t.Fatalf("exit %d: %s", code, &output)
	}
	if !strings.Contains(output.String(), "context canceled") {
		t.Fatalf("lost cancellation: %s", &output)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "plan.json")); !os.IsNotExist(err) {
		t.Fatalf("saved plan after cancellation: %v", err)
	}
	phases := runPhases(t)
	if len(phases) != 4 || phases[3].Name != "review" || phases[3].Status != "fail" {
		t.Fatalf("continued after cancellation: %v", phases)
	}
}

// A plan the reviewer says cannot succeed never reaches the builder: blocking
// objections go back to the planner like feedback, and any still standing at
// the review limit fail the run instead of becoming risks. One the planner
// resolves is forgotten like any other objection.
func TestBlockingReviewNeverReachesTheBuilder(t *testing.T) {
	t.Parallel()
	const blocker = "pages/blog/[slug].tsx needs editing but is not in files; the plan says the run will fail"
	for _, tc := range []struct {
		name    string
		clears  bool
		want    int
		reviews int
	}{{"blocked at the limit", false, 1, maxReviewSendBacks + 1}, {"blocker resolved", true, 0, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, repo := buildRepo(t)
			stub := codeStubNoPath(t)
			write(t, filepath.Join(stub, "plan-reviewer"), blockedReply(t, blocker))
			if tc.clears {
				write(t, filepath.Join(stub, "plan-reviewer.1"), reviewOK(t))
			}
			var out strings.Builder
			if code := executeWith(t, cfg, "implement", repo, "restyle the metadata", context.Background(), &out); code != tc.want {
				t.Fatalf("code = %d: %s", code, &out)
			}
			if n := spawnsOf(t, stub, "plan-reviewer"); n != tc.reviews {
				t.Fatalf("reviews = %d", n)
			}
			if got := stubFile(t, stub, "args.planner.1"); !strings.Contains(got, "Blocking objections") || !strings.Contains(got, blocker) {
				t.Fatalf("planner was not told what blocks it: %s", got)
			}
			var statuses []string
			for _, ph := range runPhases(t) {
				if ph.Name == "review" {
					statuses = append(statuses, ph.Status)
				}
			}
			for i, status := range statuses {
				want := trace.PhaseRequestedChanges
				if i == len(statuses)-1 {
					want = trace.PhaseFail
					if tc.clears {
						want = trace.PhaseSuccess
					}
				}
				if status != want {
					t.Fatalf("review statuses = %v", statuses)
				}
			}
			builds := spawnsOf(t, stub, "builder")
			if tc.clears {
				if builds != 1 {
					t.Fatalf("a resolved blocker still stopped the run: %d builds", builds)
				}
				return
			}
			if builds != 0 {
				t.Fatal("a blocked plan reached the builder")
			}
			if reason := latestRun(t).Reason; !strings.Contains(reason, "cannot succeed as written") || !strings.Contains(reason, blocker) {
				t.Fatalf("reason = %q", reason)
			}
			b, err := os.ReadFile(filepath.Join(latestRunDir(t), "plan.json"))
			if err != nil {
				t.Fatal("blocked plan not saved for diagnosis")
			}
			if strings.Contains(string(b), "unresolved review") {
				t.Fatalf("a blocker was forwarded as a risk: %s", b)
			}
		})
	}
}

// A reviewer that never ran — Pi could not reach its provider — has assessed
// nothing. The run stops there rather than sending the plan round four more
// times and forwarding it unreviewed at the limit.
func TestReviewerThatNeverRanStopsTheRun(t *testing.T) {
	t.Parallel()
	cfg, repo := buildRepo(t)
	stub := codeStubNoPath(t)
	write(t, filepath.Join(stub, "plan-reviewer.sh"), "echo 'No API key found for anthropic.' >&2; exit 1\n")

	if code := execute(t, cfg, "implement", repo, "restyle the metadata"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if p, r, b := spawnsOf(t, stub, "planner"), spawnsOf(t, stub, "plan-reviewer"), spawnsOf(t, stub, "builder"); p != 1 || r != 1 || b != 0 {
		t.Fatalf("planner %d, reviewer %d, builder %d; want one of each before stopping and no builder", p, r, b)
	}
	if reason := latestRun(t).Reason; !strings.Contains(reason, "No API key found for anthropic") {
		t.Fatalf("reason = %q", reason)
	}
}
