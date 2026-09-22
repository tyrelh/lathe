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

func reviewReply(t *testing.T, feedback ...string) string {
	t.Helper()
	if feedback == nil {
		feedback = []string{}
	}
	b, err := json.Marshal(map[string]any{"summary": "reviewed", "feedback": feedback, "artifacts": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	return piReply(t, "```json\n"+string(b)+"\n```")
}

func reviewOK(t *testing.T) string {
	t.Helper()
	return reviewReply(t)
}

func TestReviewOutput(t *testing.T) {
	if got := (&ReviewOutput{}).Validate(); len(got) != 3 {
		t.Fatalf("missing fields: %v", got)
	}
	for _, body := range []string{
		`{"summary":"reviewed","feedback":[],"artifacts":[]}`,
		`{"summary":"reviewed","feedback":null,"artifacts":[]}`,
		`{"summary":" ","feedback":[],"artifacts":[]}`,
	} {
		var out ReviewOutput
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		wantOK := strings.Contains(body, `"summary":"reviewed","feedback":[]`)
		if (len(out.Validate()) == 0) != wantOK {
			t.Fatalf("validation of %s: %v", body, out.Validate())
		}
	}
}

func TestPlanReviewLoop(t *testing.T) {
	initial := planReply(t, `"fetch.go"`)
	revised := planReply(t, `"fetch.go", "fetch_test.go"`)
	bad := piReply(t, "not an envelope")
	objections := reviewReply(t, "include fetch_test.go", "check the retry limit")
	for _, tc := range []struct {
		name     string
		replies  []string
		reviews  int
		failed   int
		risks    int
		wantLine string
	}{
		{"first acceptance", []string{initial, reviewOK(t)}, 1, 0, 0, ""},
		{"revision", []string{initial, objections, revised, reviewOK(t)}, 2, 0, 0, "plan revised 1 times; reviewer accepted"},
		{"ceiling", []string{initial, objections, revised, objections, revised, objections, revised, objections, revised, objections}, 5, 0, 2, "plan revised 4 times; 2 objections unresolved"},
		{"failed review", []string{initial, bad, bad, bad, revised, reviewOK(t)}, 2, 1, 0, "plan revised 1 times; reviewer accepted"},
		{"failed final review", []string{initial, objections, revised, objections, revised, objections, revised, objections, revised, bad, bad, bad}, 5, 1, 1, "plan revised 4 times; 1 objections unresolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				if !strings.HasPrefix(risk, "unresolved review: ") || !strings.Contains(terminal.String(), risk) || !strings.Contains(implementRequest("add retry", &plan), risk) {
					t.Fatalf("risk missing from terminal or handoff: %s", risk)
				}
				if tc.failed > 0 && !strings.Contains(risk, "review failed") {
					t.Fatalf("failed review disguised as objection: %s", risk)
				}
			}

			db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			id := filepath.Base(latestRunDir(t))
			phases, err := db.Phases(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(phases) != 1+2*tc.reviews {
				t.Fatalf("unexpected phase count: %v", phases)
			}
			failed := 0
			for i, ph := range phases[2:] {
				want := "review"
				if i%2 == 1 {
					want = "plan"
				}
				if ph.Name != want {
					t.Fatalf("phase %d: %s, want %s", i+2, ph.Name, want)
				}
				if ph.Status == "fail" {
					failed++
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
					if tc.name == "failed review" && round == 1 && !strings.Contains(input.Prompt, "same plan unchanged") {
						t.Fatal("failed review did not permit unchanged plan")
					}
				} else if ev.Name == "plan-reviewer" {
					round++
					want := fmt.Sprintf("Review round %d of 5.\nYou may send this plan back %d more times.", round, 5-round)
					if !strings.HasPrefix(input.Prompt, want) {
						t.Fatalf("review budget missing: %s", input.Prompt)
					}
					if input.Session == plannerSession {
						t.Fatal("reviewer shares planner session")
					}
					if round > 1 && (input.Session == reviewerSession) == (tc.name == "failed review") {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				cancel()
				return
			case <-ticker.C:
				if _, err := os.Stat(filepath.Join(dir, "reviewing")); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	var output strings.Builder
	if code := executeWith(t, cfg, "plan", repo, "retry", ctx, &output); code != 1 {
		t.Fatalf("exit %d: %s", code, &output)
	}
	<-done
	if !strings.Contains(output.String(), "context canceled") {
		t.Fatalf("lost cancellation: %s", &output)
	}
	if _, err := os.Stat(filepath.Join(latestRunDir(t), "plan.json")); !os.IsNotExist(err) {
		t.Fatalf("saved plan after cancellation: %v", err)
	}
	db, err := trace.Open(filepath.Join(os.Getenv("XDG_DATA_HOME"), "lathe"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	phases, err := db.Phases(filepath.Base(latestRunDir(t)))
	if err != nil || len(phases) != 3 || phases[2].Name != "review" || phases[2].Status != "fail" {
		t.Fatalf("continued after cancellation: %v, %v", phases, err)
	}
}
