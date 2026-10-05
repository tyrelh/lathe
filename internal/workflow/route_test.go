package workflow

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tyrelh/lathe/internal/jev"
)

const routedProject = `
[[agents.planner.tiers]]
when    = "Small, well-specified change"
model   = "p-cheap"
default = true

[[agents.planner.tiers]]
when  = "Open-ended change"
model = "p-top"

[[agents.builder.tiers]]
when  = "Mechanical change"
model = "b-cheap"

[[agents.builder.tiers]]
when    = "Behaviour change"
model   = "b-mid"
default = true

[[agents.builder.tiers]]
when  = "Cross-cutting change"
model = "b-top"
`

// jevStub answers every Score question with its top level, confidently, and
// keeps each request's state.
func jevStub(t *testing.T) *[]string {
	t.Helper()
	var mu sync.Mutex
	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     json.RawMessage `json:"state"`
			Questions map[string]struct {
				Criteria []string `json:"criteria"`
			} `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		states = append(states, string(req.State))
		mu.Unlock()
		n := len(req.Questions["level"].Criteria)
		p := map[string]float64{}
		for i := range n {
			p[fmt.Sprint(i)] = 0
		}
		p[fmt.Sprint(n-1)] = 1
		json.NewEncoder(w).Encode(map[string]any{
			"model": jev.Model,
			"answers": map[string]any{"level": map[string]any{
				"type": "score", "score": n - 1, "confidence": 1, "probabilities": p}},
			"usage": map[string]int{"input_tokens": 500, "output_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)
	old := jev.Endpoint
	jev.Endpoint = srv.URL
	t.Cleanup(func() { jev.Endpoint = old })
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	return &states
}

func TestImplementRoutesPlannerAndBuilder(t *testing.T) {
	cfg, repo := buildRepo(t)
	// Outside the repo, which implement needs clean.
	dir := t.TempDir()
	write(t, filepath.Join(dir, "lathe.toml"), routedProject)
	if _, err := cfg.LoadProject(dir); err != nil {
		t.Fatal(err)
	}
	states := jevStub(t)
	stub := codeStub(t)
	if got := execute(t, cfg, "implement", repo, "greet the world"); got != 0 {
		t.Fatalf("code = %d", got)
	}

	var names []string
	for _, ph := range runPhases(t) {
		names = append(names, ph.Name)
	}
	if got := strings.Join(names[:7], ","); got != "request,auth,route-plan,plan,review,route-build,implement" {
		t.Fatalf("phases = %v", names)
	}
	for agent, model := range map[string]string{"planner": "p-top", "builder": "b-top"} {
		if args := stubFile(t, stub, "args."+agent+".0"); !strings.Contains(args, "\n"+model+"\n") {
			t.Errorf("%s ran without %s: %s", agent, model, args)
		}
	}
	// The plan-reviewer keeps its own model.
	if args := stubFile(t, stub, "args.plan-reviewer.0"); strings.Contains(args, "p-top") {
		t.Errorf("plan-reviewer was routed: %s", args)
	}
	if len(*states) != 2 || (*states)[0] != `{"request":"greet the world"}` ||
		!strings.Contains((*states)[1], `"files":["hello.txt"],"file_count":"one file"`) {
		t.Fatalf("states = %q", *states)
	}
}

func TestFileCount(t *testing.T) {
	for n, want := range map[int]string{1: "one file", 2: "a few files", 5: "a few files", 6: "many files", 40: "many files"} {
		if got := fileCount(n); got != want {
			t.Errorf("fileCount(%d) = %q", n, got)
		}
	}
}
