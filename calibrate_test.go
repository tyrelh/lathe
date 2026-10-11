package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/agentconfig"
	"github.com/tyrelh/lathe/internal/jev"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/worker"
	"github.com/tyrelh/lathe/internal/workspace"
)

func TestCalibrate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A repository's lathe.toml configures nothing, tiers included.
	repo := gitRepo(t, "[[agents.planner.tiers]]\nwhen = \"x\"\ndefault = true\n")
	dot := filepath.Join(home, ".lathe")
	if _, err := agentconfig.Seed(dot, Seed, version); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dot, "agents", "builder", "config.yaml"), []byte(`provider: moonshotai
model: kimi-k2.7-code
thinking: medium
timeout: 10m
tiers:
  - when: Mechanical change
    model: b-cheap
    default: true
  - when: Behaviour change
    model: b-top
`), 0o644)
	os.WriteFile(filepath.Join(dot, "agents", "planner", "config.yaml"),
		[]byte("provider: moonshotai\nmodel: kimi-k3\nthinking: medium\ntimeout: 10m\n"), 0o644)

	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		states = append(states, string(b))
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"level":{"type":"score","score":0.4,"confidence":0.4,
			"probabilities":{"0":0.6,"1":0.4}}},"usage":{"input_tokens":1000,"output_tokens":1}}`))
	}))
	defer srv.Close()
	old := jev.Endpoint
	jev.Endpoint = srv.URL
	defer func() { jev.Endpoint = old }()
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	dataRoot, err := trace.DataRoot()
	if err != nil {
		t.Fatal(err)
	}
	db, err := trace.Open(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ws, err := workspace.Local(repo)
	if err != nil {
		t.Fatal(err)
	}
	submit := func(workflow, request string, phases ...string) string {
		id, err := db.Submit(trace.Request{Workflow: workflow, Repo: ws.Path, Request: request})
		if err != nil {
			t.Fatal(err)
		}
		for i, name := range phases {
			if err := db.PhaseUpsert(trace.NewPhase(id, i+1, name, "x")); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	submit("scout", "what is here")
	submit("build", "no plan was saved", "plan")
	built := submit("build", "add retry", "plan", "review", "plan", "review", "implement", "implement", "implement")
	dir := worker.ReportDir(dataRoot, built, 0)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "plan.json"),
		[]byte(`{"summary":"wrap fetch","steps":["edit"],"files":["fetch.go","retry.go"],"risks":[],"artifacts":[]}`), 0o644)

	var out bytes.Buffer
	if err := calibrate(context.Background(), &out, db, dataRoot, repo, "builder", 30); err != nil {
		t.Fatal(err)
	}
	// Confidence under the floor picks the stronger of the two likeliest levels;
	// only the run with a plan is asked about.
	if len(states) != 1 || !strings.Contains(states[0], `"file_count":"a few files"`) {
		t.Fatalf("requests = %q", states)
	}
	var row string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, built) {
			row = strings.Join(strings.Fields(line), " ")
		}
	}
	if want := built + " build 1 b-top 0.40 low-confidence 1 2 add retry"; row != want {
		t.Fatalf("row = %q, want %q\n%s", row, want, out.String())
	}
	if !strings.Contains(out.String(), "\n1 runs, $0.00004") {
		t.Fatalf("summary missing:\n%s", out.String())
	}

	if err := calibrate(context.Background(), &out, db, dataRoot, repo, "planner", 30); err == nil ||
		!strings.Contains(err.Error(), "no tiers") {
		t.Fatalf("planner without tiers: %v", err)
	}
}
