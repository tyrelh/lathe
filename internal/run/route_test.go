package run

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/jev"
)

func TestChoose(t *testing.T) {
	for _, tc := range []struct {
		p           []float64
		confidence  float64
		tier        int
		reason      string
		description string
	}{
		{[]float64{0.7, 0.2, 0.1}, 0.8, 0, "routed", "top level"},
		{[]float64{0.1, 0.45, 0.45}, 0.8, 2, "routed", "tie goes up"},
		{[]float64{0.7, 0.2, 0.1}, 0.3, 1, "low-confidence", "low confidence moves up one"},
		{[]float64{0.1, 0.2, 0.7}, 0.3, 2, "low-confidence", "capped at the top"},
		{[]float64{0.6, 0.4}, 0.5, 0, "routed", "floor itself is enough"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			tier, reason := choose(tc.p, tc.confidence, 0.5)
			if tier != tc.tier || reason != tc.reason {
				t.Fatalf("choose = %d %s", tier, reason)
			}
		})
	}
}

// jevServer points jev at a server that always answers body, and returns how
// to read the last request it got.
func jevServer(t *testing.T, status int, body string) *string {
	t.Helper()
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last = string(b)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := jev.Endpoint
	jev.Endpoint = srv.URL
	t.Cleanup(func() { jev.Endpoint = old })
	return &last
}

func answer(p0, p1, p2, confidence float64) string {
	return fmt.Sprintf(`{"model":"jev-1.13.0","answers":{"level":{"type":"score","score":1,"confidence":%v,
		"probabilities":{"0":%v,"1":%v,"2":%v}}},"usage":{"input_tokens":2000,"output_tokens":1}}`, confidence, p0, p1, p2)
}

func tiers(r *Run, floor float64) {
	rt := config.Routing{Instructions: "How demanding?"}
	for i, m := range []string{"luna", "sol", "astra"} {
		rt.Tiers = append(rt.Tiers, config.Tier{When: "when " + m, Default: i == 1,
			Resolved: config.Resolved{Agent: config.Agent{Name: "builder", Provider: "p-" + m, Model: m}}})
	}
	r.cfg.Routing = map[string]config.Routing{"builder": rt}
	r.cfg.ConfidenceFloor = floor
}

func TestRoute(t *testing.T) {
	for _, tc := range []struct {
		name, key, body string
		status          int
		model, reason   string
		charged         bool
	}{
		{"routed", "k", answer(0.8, 0.1, 0.1, 0.9), 200, "luna", "routed", true},
		{"low confidence", "k", answer(0.8, 0.1, 0.1, 0.2), 200, "sol", "low-confidence", true},
		{"bad response", "k", `{"model":"jev-1.12.0"}`, 200, "sol", "error", false},
		{"api error", "k", "nope", 401, "sol", "error", false},
		{"no key", "", answer(0.8, 0.1, 0.1, 0.9), 200, "sol", "error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := jevServer(t, tc.status, tc.body)
			t.Setenv("TYPESAFE_API_KEY", tc.key)
			r := newRun(t, "")
			defer r.Finish(true, "")
			captured := r.cfg.Agents
			before := captured["builder"].Model
			tiers(r, 0.5)
			if !r.Routes("builder") || r.Routes("planner") {
				t.Fatal("Routes")
			}
			err := r.Phase(Params{Name: "route-build", Owner: "engineer"}, func(h *Handle) error {
				return h.Route("builder", map[string]string{"request": "add retry"})
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := r.cfg.Agents["builder"].Model; got != tc.model {
				t.Fatalf("builder model = %s, want %s", got, tc.model)
			}
			if captured["builder"].Model != before {
				t.Fatal("routing changed the captured snapshot")
			}
			if tc.key != "" && !strings.Contains(*sent, `"criteria":["when luna","when sol","when astra"]`) {
				t.Fatalf("request = %s", *sent)
			}
			db := readDB(t)
			input := scalar[string](t, db, "SELECT payload FROM events WHERE type='input' AND name='jev'")
			if !strings.Contains(input, `"request":"add retry"`) || !strings.Contains(input, `"criteria":["when luna"`) {
				t.Errorf("input event = %s", input)
			}
			var output struct {
				Report RouteReport `json:"report"`
				Text   string      `json:"text"`
			}
			raw := scalar[string](t, db, "SELECT payload FROM events WHERE type='output' AND name='jev'")
			if err := json.Unmarshal([]byte(raw), &output); err != nil {
				t.Fatal(err)
			}
			rep := output.Report
			if rep.Reason != tc.reason || rep.Levels[rep.Tier].Model != tc.model || len(rep.Levels) != 3 || rep.Floor != 0.5 {
				t.Errorf("report = %+v", rep)
			}
			if (rep.Error != "") != (tc.reason == "error") || (rep.Confidence != nil) != tc.charged ||
				(rep.Levels[0].Probability != nil) != tc.charged {
				t.Errorf("report = %+v", rep)
			}
			// Jev's body is kept whenever one arrived, including a rejected one.
			if want := tc.key != ""; (output.Text == tc.body) != want {
				t.Errorf("raw = %q", output.Text)
			}
			n := scalar[int](t, db, "SELECT count(*) FROM usage WHERE provider='typesafe' AND model='jev-1.13.0'")
			if (n == 1) != tc.charged {
				t.Fatalf("usage rows = %d", n)
			}
		})
	}
}

func TestAuthPreflightRouting(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		off       bool
		want      string
	}{
		{"no key", "", false, "routing will use the default tiers"},
		{"key", "k", false, ""},
		{"flags", "k", true, "off: a provider, model or thinking flag was passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TYPESAFE_API_KEY", tc.key)
			dir := t.TempDir()
			bin, calls := filepath.Join(dir, "pi"), filepath.Join(dir, "calls")
			script := fmt.Sprintf("#!/bin/sh\necho \"$4\" >> %q\nprintf '{\"provider\":\"%%s\",\"status\":\"ready\"}\\n' \"$4\"\n", calls)
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			r := newRun(t, bin)
			defer r.Finish(true, "")
			r.cfg.Agents = map[string]config.Resolved{"builder": {Agent: config.Agent{Name: "builder", Provider: "p-sol"}}}
			if tc.off {
				r.cfg.RoutingOff = true
			} else {
				tiers(r, 0.5)
			}
			if err := r.Phase(Params{Name: "auth", Owner: "engineer"}, func(h *Handle) error { return h.AuthPreflight() }); err != nil {
				t.Fatal(err)
			}
			want := "p-astra\np-luna\np-sol\n"
			if tc.off {
				want = "p-sol\n"
			}
			if b, _ := os.ReadFile(calls); string(b) != want {
				t.Fatalf("providers checked = %q", b)
			}
			got := ""
			if n := scalar[int](t, readDB(t), "SELECT count(*) FROM events WHERE name='routing'"); n > 0 {
				got = scalar[string](t, readDB(t), "SELECT payload FROM events WHERE name='routing'")
			}
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("routing log = %q, want %q", got, tc.want)
			}
		})
	}
}
