package config

import (
	"reflect"
	"strings"
	"testing"
)

var builderTiers = []TierConfig{
	{When: "Mechanical change", Model: "b-cheap"},
	{When: "Behaviour change", Default: true},
	{When: "Cross-cutting change", Model: "b-top", Thinking: "high"},
}

// fixture is every known agent with distinguishable settings and prompts, and
// tiers for the builder.
func fixture(t *testing.T) Config {
	t.Helper()
	var agents []Agent
	files := map[string]File{}
	off := false
	for _, p := range Permissions {
		a := Agent{Name: p.Name, Provider: "p", Model: "m-" + p.Name, Thinking: "medium", Timeout: "10m"}
		if p.Name == "brancher" {
			a.RepoInstructions = &off
		}
		agents = append(agents, a)
		files[AgentFile(p.Name, "config.yaml")] = File{Path: "/h/" + AgentFile(p.Name, "config.yaml"), Body: "provider: p\n"}
		files[AgentFile(p.Name, "system.md")] = File{Path: "/h/" + AgentFile(p.Name, "system.md"), Body: "You are the " + p.Name}
		files[AgentFile(p.Name, "user.md")] = File{Path: "/h/" + AgentFile(p.Name, "user.md"), Body: "{{request}} for " + p.Name}
	}
	for _, r := range Routes {
		files[r.Key] = File{Path: "/h/" + r.Key, Body: "route " + r.Agent}
	}
	// An agent the binary does not know is dropped.
	agents = append(agents, Agent{Name: "stranger", Provider: "x", Model: "x", Thinking: "low", Timeout: "1m"})
	c, err := New(agents, map[string][]TierConfig{"builder": builderTiers}, files)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewMergesPermissions(t *testing.T) {
	c := fixture(t)
	if len(c.Agents) != len(Permissions) {
		t.Fatalf("agents = %d; want %d", len(c.Agents), len(Permissions))
	}
	for i, a := range c.Agents {
		p := Permissions[i]
		if a.Name != p.Name || !reflect.DeepEqual(a.Tools, p.Tools) || a.System != AgentFile(p.Name, "system.md") {
			t.Errorf("agent %d = %+v", i, a)
		}
	}
	if _, err := New(nil, nil, nil); err == nil || !strings.Contains(err.Error(), "scout") {
		t.Fatalf("missing agent: %v", err)
	}
}

func TestResolveAppliesFlagsOverConfig(t *testing.T) {
	c := fixture(t)
	a, err := c.Resolve("scout", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Provider != "p" || a.Model != "m-scout" || a.Thinking != "medium" || a.Deadline.Minutes() != 10 {
		t.Fatalf("scout = %+v", a)
	}
	if a.SystemPrompt != "You are the scout" || a.UserPrompt != "{{request}} for scout" {
		t.Fatalf("prompts = %q %q", a.SystemPrompt, a.UserPrompt)
	}
	for _, banned := range []string{"bash", "write", "edit"} {
		for _, got := range a.Tools {
			if got == banned {
				t.Fatalf("scout was granted %q", banned)
			}
		}
	}
	a, err = c.Resolve("scout", Overrides{Provider: "q", Model: "kimi-k3", Thinking: "high"})
	if err != nil || a.Provider != "q" || a.Model != "kimi-k3" || a.Thinking != "high" {
		t.Fatalf("flags ignored: %+v %v", a, err)
	}
}

func TestResolveUnknownAgent(t *testing.T) {
	_, err := fixture(t).Resolve("unknown-agent", Overrides{})
	if err == nil || !strings.Contains(err.Error(), "scout") {
		t.Fatalf("err = %v; want an unknown-agent error listing scout", err)
	}
	if _, err := fixture(t).Resolve("stranger", Overrides{}); err == nil {
		t.Fatal("an agent the binary does not know resolved")
	}
}

func TestResolveRefusesAnUnloadedPrompt(t *testing.T) {
	c := fixture(t)
	delete(c.Files, AgentFile("scout", "user.md"))
	if _, err := c.Resolve("scout", Overrides{}); err == nil || !strings.Contains(err.Error(), "agents/scout/user.md") {
		t.Fatalf("err = %v", err)
	}
}

func TestPromptSubstitutesTheRequest(t *testing.T) {
	a, err := fixture(t).Resolve("scout", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if p := a.Prompt("where is the router"); p != "where is the router for scout" {
		t.Fatalf("prompt = %q", p)
	}
}

func TestRepoInstructionsDefaultOn(t *testing.T) {
	c := fixture(t)
	for name, want := range map[string]bool{"builder": true, "brancher": false} {
		a, err := c.Resolve(name, Overrides{})
		if err != nil || a.WantsRepoInstructions() != want {
			t.Errorf("%s: repo instructions = %v; want %v (%v)", name, a.WantsRepoInstructions(), want, err)
		}
	}
}

func TestCaptureCarriesCommandTimeout(t *testing.T) {
	s, err := fixture(t).Capture([]string{"scout"}, Overrides{})
	if err != nil || s.CommandDeadline.Minutes() != 10 {
		t.Fatalf("command deadline = %v, %v", s.CommandDeadline, err)
	}
	if !reflect.DeepEqual(s.Protected, Protected) || !reflect.DeepEqual(s.BashDenied, BashDenied) {
		t.Fatalf("permissions not carried: %+v", s)
	}
}

// The snapshot is frozen: changing the loaded files does not reach it.
func TestCaptureFreezesPromptText(t *testing.T) {
	c := fixture(t)
	s, err := c.Capture([]string{"builder"}, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	c.Files[AgentFile("builder", "system.md")] = File{Body: "changed"}
	if got := s.Agents["builder"].SystemPrompt; got != "You are the builder" {
		t.Fatalf("captured system prompt = %q", got)
	}
}

func TestCaptureResolvesTiers(t *testing.T) {
	s, err := fixture(t).Capture([]string{"planner", "builder"}, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Routing["planner"]; ok || len(s.Routing) != 1 || s.ConfidenceFloor != 0.5 || s.RoutingOff {
		t.Fatalf("routing = %+v floor %v off %v", s.Routing, s.ConfidenceFloor, s.RoutingOff)
	}
	rt := s.Routing["builder"]
	if rt.Instructions != "route builder" || rt.Default() != 1 ||
		strings.Join(rt.Criteria(), "|") != "Mechanical change|Behaviour change|Cross-cutting change" {
		t.Fatalf("routing = %+v", rt)
	}
	for i, want := range []string{"p/b-cheap/medium", "p/m-builder/medium", "p/b-top/high"} {
		a := rt.Tiers[i].Resolved
		if got := a.Provider + "/" + a.Model + "/" + a.Thinking; got != want || a.SystemPrompt != "You are the builder" {
			t.Errorf("tier %d = %s, want %s", i, got, want)
		}
	}
}

func TestCaptureFlagsTurnRoutingOff(t *testing.T) {
	for _, ov := range []Overrides{{Provider: "p"}, {Model: "m"}, {Thinking: "low"}} {
		s, err := fixture(t).Capture([]string{"builder"}, ov)
		if err != nil || len(s.Routing) != 0 || !s.RoutingOff {
			t.Fatalf("%+v: routing = %+v off %v (%v)", ov, s.Routing, s.RoutingOff, err)
		}
	}
	s, err := fixture(t).Capture([]string{"scout"}, Overrides{Model: "m"})
	if err != nil || s.Routing != nil || s.RoutingOff || s.ConfidenceFloor != 0 {
		t.Fatalf("untiered: %+v %v", s, err)
	}
}

func TestCheckTiers(t *testing.T) {
	two := []TierConfig{{When: "a", Default: true}, {When: "b"}}
	if err := CheckTiers("planner", two); err != nil {
		t.Fatal(err)
	}
	eleven := []TierConfig{{When: "a", Default: true}}
	for range 10 {
		eleven = append(eleven, TierConfig{When: "b"})
	}
	for name, tc := range map[string]struct {
		agent string
		tiers []TierConfig
	}{
		"unroutable agent": {"tester", two},
		"one tier":         {"builder", two[:1]},
		"eleven tiers":     {"builder", eleven},
		"no default":       {"planner", []TierConfig{{When: "a"}, {When: "b"}}},
		"two defaults":     {"planner", []TierConfig{{When: "a", Default: true}, {When: "b", Default: true}}},
		"empty when":       {"planner", []TierConfig{{When: " ", Default: true}, {When: "b"}}},
		"bad thinking":     {"planner", []TierConfig{{When: "a", Default: true}, {When: "b", Thinking: "max"}}},
	} {
		if err := CheckTiers(tc.agent, tc.tiers); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPrompts(t *testing.T) {
	c := fixture(t)
	f := c.Files[AgentFile("builder", "system.md")]
	f.Seed, f.Edited, f.Drift = "seed text", true, true
	c.Files[AgentFile("builder", "system.md")] = f
	got, err := c.Prompts(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(Permissions)+len(Routes) {
		t.Fatalf("%d entries", len(got))
	}
	var builder AgentPrompts
	for _, a := range got {
		if a.Name == "builder" {
			builder = a
		}
	}
	sys := builder.Prompts[0]
	if builder.Config == nil || builder.Config.Model != "m-builder" || builder.Config.Key != "agents/builder/config.yaml" ||
		sys.Role != "system" || sys.Body != "You are the builder" || sys.Seed != "seed text" || !sys.Edited || !sys.Drift ||
		sys.Hash != Hash("You are the builder") || sys.Path != "/h/agents/builder/system.md" {
		t.Fatalf("builder = %+v %+v", builder.Config, sys)
	}
	last := got[len(got)-1]
	if last.Name != "route-build" || last.Kind != "routing" || last.Routes != "builder" || last.Config != nil ||
		last.Prompts[0].Role != "instructions" || last.Prompts[0].Key != "routing/builder.md" {
		t.Fatalf("route-build = %+v", last)
	}
	bare, err := c.Prompts(false)
	if err != nil || bare[0].Prompts[0].Body != "" || bare[0].Prompts[0].Hash != "" {
		t.Fatalf("bodies without asking: %+v %v", bare[0], err)
	}
	delete(c.Files, "routing/builder.md")
	if _, err := c.Prompts(false); err == nil || !strings.Contains(err.Error(), "route-build") {
		t.Fatalf("missing routing file: %v", err)
	}
}
