package workflow

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/jev"
)

var routedTiers = map[string][]config.TierConfig{
	"planner": {
		{When: "Small, well-specified change", Model: "p-cheap", Default: true},
		{When: "Open-ended change", Model: "p-top"},
	},
	"builder": {
		{When: "Mechanical change", Model: "b-cheap"},
		{When: "Behaviour change", Model: "b-mid", Default: true},
		{When: "Cross-cutting change", Model: "b-top"},
	},
}

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
	cfg.Tiers = routedTiers
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

	// The routing phases the dashboard lists by name are the graph nodes that run.
	prompts, err := cfg.Prompts(false)
	if err != nil {
		t.Fatal(err)
	}
	routed := 0
	for _, p := range prompts {
		if p.Kind != "routing" {
			continue
		}
		routed++
		if !slices.Contains(names, p.Name) {
			t.Errorf("routing phase %q (routes %s) is not a node of the routed graph: %v", p.Name, p.Routes, names)
		}
	}
	if routed != 2 {
		t.Fatalf("Prompts() reported %d routing phases; want 2", routed)
	}
}

func TestFileCount(t *testing.T) {
	for n, want := range map[int]string{1: "one file", 2: "a few files", 5: "a few files", 6: "many files", 40: "many files"} {
		if got := fileCount(n); got != want {
			t.Errorf("fileCount(%d) = %q", n, got)
		}
	}
}
