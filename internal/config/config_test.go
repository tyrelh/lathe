package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The shipped roster is the thing that actually has to decode, so the test
// loads it rather than a fixture that could drift away from it.
func assets(t *testing.T) Config {
	t.Helper()
	c, err := Load(os.DirFS(filepath.Join("..", "..")))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResolveAppliesDefaultsAndFlags(t *testing.T) {
	c := assets(t)
	a, err := c.Resolve("scout", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Provider != "moonshotai" || a.Model != "kimi-k2.7-code" {
		t.Fatalf("defaults not applied: %+v", a)
	}
	if a.Deadline.Minutes() != 10 {
		t.Fatalf("timeout = %v; want 10m", a.Deadline)
	}
	if strings.Join(a.Tools, ",") != "read,grep,find,ls" {
		t.Fatalf("tools = %v", a.Tools)
	}
	// No bash, no write, no edit: this is the line that makes a v0 run unable
	// to touch the repo it was pointed at.
	for _, banned := range []string{"bash", "write", "edit"} {
		for _, got := range a.Tools {
			if got == banned {
				t.Fatalf("scout was granted %q", banned)
			}
		}
	}
	if !strings.Contains(a.SystemPrompt, "You are the scout") || !strings.Contains(a.UserPrompt, "{{request}}") {
		t.Fatal("prompts were not read out of the embedded FS")
	}

	// A flag beats the roster and the defaults both.
	a, err = c.Resolve("scout", Overrides{Model: "kimi-k3", Thinking: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Model != "kimi-k3" || a.Thinking != "high" {
		t.Fatalf("overrides ignored: %+v", a)
	}
}

// An unknown agent fails before anything is spawned, and says what does exist.
func TestResolveUnknownAgent(t *testing.T) {
	_, err := assets(t).Resolve("unknown-agent", Overrides{})
	if err == nil || !strings.Contains(err.Error(), "scout") {
		t.Fatalf("err = %v; want an unknown-agent error listing scout", err)
	}
}

func TestPromptSubstitutesTheRequest(t *testing.T) {
	a, err := assets(t).Resolve("scout", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	p := a.Prompt("where is the HTTP router")
	if !strings.Contains(p, "where is the HTTP router") || strings.Contains(p, "{{request}}") {
		t.Fatalf("prompt not substituted:\n%s", p)
	}
}

// repo_instructions is on when nobody sets it, an agent's own false wins, and
// [defaults] reaches every agent that sets nothing.
func TestRepoInstructionsDefaultOn(t *testing.T) {
	c := assets(t)
	for name, want := range map[string]bool{"builder": true, "tester": true, "adjudicator": true, "code-review-security": true, "brancher": false, "pr-author": false} {
		a, err := c.Resolve(name, Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		if got := a.WantsRepoInstructions(); got != want {
			t.Errorf("%s: repo instructions = %v; want %v", name, got, want)
		}
	}
	off := false
	c.Defaults.RepoInstructions = &off
	if a, err := c.Resolve("builder", Overrides{}); err != nil || a.WantsRepoInstructions() {
		t.Fatalf("builder under [defaults] false: repo instructions on (err %v)", err)
	}
}

// The target root is discovered, and a directory outside a repo is an error
// rather than a silent fallback that would point an agent at $HOME.
func TestTargetRoot(t *testing.T) {
	got, err := TargetRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if eval(t, got) != eval(t, want) {
		t.Fatalf("target root = %q; want %q", got, want)
	}
	if _, err := TargetRoot(t.TempDir()); err == nil {
		t.Fatal("a directory outside any git repo resolved to a target root")
	}
}

func eval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The planner is read-only too: it is the builder's write scope that it
// produces, not one of its own.
func TestPlannerIsReadOnly(t *testing.T) {
	a, err := assets(t).Resolve("planner", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"bash", "write", "edit"} {
		for _, got := range a.Tools {
			if got == banned {
				t.Fatalf("planner was granted %q", banned)
			}
		}
	}
	if !strings.Contains(a.UserPrompt, "{{request}}") || !strings.Contains(a.UserPrompt, `"files"`) {
		t.Fatal("the planner prompt was not read out of the embedded FS")
	}
}

// The deny list is one list in one file, because the Go gates and the guard
// extension both have to be holding the same one.
// A code phase is bounded by the roster, not by the tester it used to borrow
// from, so a workflow that captures no tester still gets a deadline.
func TestCaptureCarriesCommandTimeout(t *testing.T) {
	s, err := assets(t).Capture([]string{"scout"}, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if s.CommandDeadline.Minutes() != 10 {
		t.Fatalf("command deadline = %v; want 10m", s.CommandDeadline)
	}
}

func TestCaptureRejectsBadCommandTimeout(t *testing.T) {
	c := assets(t)
	c.CommandTimeout = "ten minutes"
	if _, err := c.Capture([]string{"scout"}, Overrides{}); err == nil || !strings.Contains(err.Error(), "command_timeout") {
		t.Fatalf("error = %v", err)
	}
}

func TestProtectedDecodes(t *testing.T) {
	got := strings.Join(assets(t).Protected, ",")
	if !strings.Contains(got, ".git/") || !strings.Contains(got, ".env*") {
		t.Fatalf("protected = %q; want at least .git/ and .env*", got)
	}
}

// project writes body as root's lathe.toml and loads it into the shipped roster.
func project(t *testing.T, body string) (Config, bool, error) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lathe.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c := assets(t)
	loaded, err := c.LoadProject(root)
	return c, loaded, err
}

func resolve(t *testing.T, c Config, name string, ov Overrides) Resolved {
	t.Helper()
	a, err := c.Resolve(name, ov)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLoadProjectMissingIsSilent(t *testing.T) {
	c := assets(t)
	loaded, err := c.LoadProject(t.TempDir())
	if loaded || err != nil {
		t.Fatalf("missing file: loaded = %v, err = %v", loaded, err)
	}
	if a := resolve(t, c, "builder", Overrides{}); a.Model != "kimi-k2.7-code" || a.Provider != "moonshotai" {
		t.Fatalf("a missing file changed the builder: %+v", a)
	}
}

// Each field falls through on its own: flag, then [agents.<name>], then the
// top level, then the roster.
func TestLoadProjectPrecedence(t *testing.T) {
	c, loaded, err := project(t, `
provider = "anthropic"
model    = "claude-sonnet-5"

[agents.planner]
model = "claude-opus-5-5"
`)
	if !loaded || err != nil {
		t.Fatalf("valid file: loaded = %v, err = %v", loaded, err)
	}
	// The top level beats the builder's own pin.
	if a := resolve(t, c, "builder", Overrides{}); a.Model != "claude-sonnet-5" || a.Provider != "anthropic" {
		t.Fatalf("builder: %+v", a)
	}
	// The planner's block applies to the planner, and it still picks up the
	// top-level provider it does not set.
	if a := resolve(t, c, "planner", Overrides{}); a.Model != "claude-opus-5-5" || a.Provider != "anthropic" {
		t.Fatalf("planner: %+v", a)
	}
	// Thinking is set nowhere in the file, so the roster's default reaches it.
	if a := resolve(t, c, "scout", Overrides{}); a.Thinking != "medium" {
		t.Fatalf("scout thinking = %q", a.Thinking)
	}
	// A flag beats both.
	if a := resolve(t, c, "planner", Overrides{Model: "kimi-k3"}); a.Model != "kimi-k3" || a.Provider != "anthropic" {
		t.Fatalf("flagged planner: %+v", a)
	}
}

// repo_instructions follows the same precedence, and false from the project
// is a value rather than "unset": it turns off what the roster leaves on.
func TestLoadProjectRepoInstructions(t *testing.T) {
	c, loaded, err := project(t, `
repo_instructions = false

[agents.brancher]
repo_instructions = true
`)
	if !loaded || err != nil {
		t.Fatalf("valid file: loaded = %v, err = %v", loaded, err)
	}
	for name, want := range map[string]bool{"builder": false, "tester": false, "brancher": true} {
		if got := resolve(t, c, name, Overrides{}).WantsRepoInstructions(); got != want {
			t.Errorf("%s: repo instructions = %v; want %v", name, got, want)
		}
	}
}

// A malformed file is ignored whole, so a typo cannot quietly do nothing.
func TestLoadProjectRejectsMalformed(t *testing.T) {
	for _, tc := range []struct{ name, body, mention string }{
		{"invalid toml", `model = `, ""},
		{"wrong type", `model = 3`, ""},
		{"unknown key", `modle = "x"`, "modle"},
		{"unknown agent", "[agents.bulder]\nmodel = \"x\"", "bulder"},
		// Nothing that loosens a guard is settable from the target repository.
		{"forbidden nested key", "model = \"x\"\n[agents.builder]\ntools = [\"bash\"]", "agents.builder.tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, loaded, err := project(t, tc.body)
			if loaded || err == nil || !strings.HasPrefix(err.Error(), "lathe.toml:") {
				t.Fatalf("loaded = %v, err = %v", loaded, err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("err = %v; want it to name %q", err, tc.mention)
			}
			a := resolve(t, c, "builder", Overrides{})
			if a.Model != "kimi-k2.7-code" || strings.Join(a.Tools, ",") != "read,grep,find,ls,write,edit" {
				t.Fatalf("a rejected file changed the builder: %+v", a)
			}
		})
	}
}

// Agent names are checked against the whole roster, not the workflow at hand.
func TestLoadProjectAcceptsAgentsOutsideTheWorkflow(t *testing.T) {
	c, loaded, err := project(t, "[agents.pr-author]\nmodel = \"x\"")
	if !loaded || err != nil {
		t.Fatalf("loaded = %v, err = %v", loaded, err)
	}
	s, err := c.Capture([]string{"scout"}, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Agents["scout"].Model != "kimi-k2.7-code" {
		t.Fatalf("scout: %+v", s.Agents["scout"])
	}
}

func TestDefaultTOMLRoundTrip(t *testing.T) {
	c := assets(t)
	c.Defaults.Provider = "default-provider"
	c.Defaults.Thinking = "low"
	off, on := false, true
	c.Defaults.RepoInstructions = &off
	c.Agents[0].RepoInstructions = &on
	c.Agents[1].Model = "agent-model"

	body, err := c.DefaultTOML()
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.DefaultTOML()
	if err != nil || !bytes.Equal(body, again) {
		t.Fatalf("output is not deterministic: %v", err)
	}
	var p Project
	if _, err := toml.Decode(string(body), &p); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, body)
	}
	if p.Provider != c.Defaults.Provider || p.Model != c.Defaults.Model || p.Thinking != c.Defaults.Thinking || p.RepoInstructions == nil || *p.RepoInstructions {
		t.Fatalf("global defaults: %+v", p.Overrides)
	}
	if len(p.Agents) != len(c.Agents) {
		t.Fatalf("generated %d agents; want %d", len(p.Agents), len(c.Agents))
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lathe.toml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := c
	if ok, err := loaded.LoadProject(root); !ok || err != nil {
		t.Fatalf("generated file: loaded = %v, err = %v", ok, err)
	}
	for _, agent := range c.Agents {
		want := resolve(t, c, agent.Name, Overrides{})
		got := resolve(t, loaded, agent.Name, Overrides{})
		entry, ok := p.Agents[agent.Name]
		if !ok || entry.RepoInstructions == nil {
			t.Fatalf("%s: missing explicit settings", agent.Name)
		}
		if entry.Provider != want.Provider || entry.Model != want.Model || entry.Thinking != want.Thinking || *entry.RepoInstructions != want.WantsRepoInstructions() {
			t.Errorf("%s: generated settings = %+v; want %+v", agent.Name, entry, want.Agent)
		}
		if got.Provider != want.Provider || got.Model != want.Model || got.Thinking != want.Thinking || got.WantsRepoInstructions() != want.WantsRepoInstructions() {
			t.Errorf("%s: round trip = %+v; want %+v", agent.Name, got.Agent, want.Agent)
		}
	}
	first, second := p.Agents[c.Agents[0].Name], p.Agents[c.Agents[1].Name]
	if !*first.RepoInstructions || *second.RepoInstructions || second.Model != "agent-model" || first.Provider != c.Defaults.Provider {
		t.Fatal("per-agent overrides or inherited defaults were lost")
	}

	if p.Routing.ConfidenceFloor == nil || *p.Routing.ConfidenceFloor != defaultFloor {
		t.Fatalf("confidence_floor = %v; want %v", p.Routing.ConfidenceFloor, defaultFloor)
	}
	routed := make([]string, 0, len(routingPrompts))
	for name := range routingPrompts {
		routed = append(routed, name)
	}
	s, err := loaded.Capture(routed, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range routed {
		roster, got := c.Tiers[name], s.Routing[name].Tiers
		if len(roster) == 0 || len(got) != len(roster) {
			t.Fatalf("%s: captured %d tiers; roster has %d", name, len(got), len(roster))
		}
		for i, tier := range p.Agents[name].Tiers {
			want := resolve(t, c, name, Overrides{Provider: roster[i].Provider, Model: roster[i].Model, Thinking: roster[i].Thinking})
			if tier.Provider != want.Provider || tier.Model != want.Model || tier.Thinking != want.Thinking || tier.When != roster[i].When || tier.Default != roster[i].Default {
				t.Errorf("%s tier %d: generated %+v; want %+v", name, i+1, tier, want.Agent)
			}
		}
	}
}

func TestDefaultTOMLIgnoresProject(t *testing.T) {
	c := assets(t)
	want, err := c.DefaultTOML()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lathe.toml"), []byte("model = \"project-model\"\nrepo_instructions = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.LoadProject(root); !ok || err != nil {
		t.Fatalf("project: loaded = %v, err = %v", ok, err)
	}
	if resolve(t, c, "scout", Overrides{}).Model != "project-model" {
		t.Fatal("project override was not applied")
	}
	got, err := c.DefaultTOML()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("project settings changed generated defaults: %v\n%s", err, got)
	}
}

// The snapshot is frozen: reloading a changed file does not reach it.
func TestCaptureFreezesProject(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "lathe.toml")
	os.WriteFile(path, []byte(`model = "first"`), 0o644)
	c := assets(t)
	if _, err := c.LoadProject(root); err != nil {
		t.Fatal(err)
	}
	s, err := c.Capture([]string{"builder"}, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(`model = "second"`), 0o644)
	if _, err := c.LoadProject(root); err != nil {
		t.Fatal(err)
	}
	if got := s.Agents["builder"].Model; got != "first" {
		t.Fatalf("captured model = %q; want first", got)
	}
	if got := resolve(t, c, "builder", Overrides{}).Model; got != "second" {
		t.Fatalf("reloaded model = %q; want second", got)
	}
}

const tiered = `
model = "kimi-k2.6"

[routing]
confidence_floor = 0.7

[agents.builder]
provider = "openai-codex"

[[agents.builder.tiers]]
when  = "Mechanical change"
model = "gpt-6-luna"

[[agents.builder.tiers]]
when    = "Behaviour change"
default = true

[[agents.builder.tiers]]
when     = "Cross-cutting change"
model    = "gpt-6-astra"
thinking = "high"
`

func TestCaptureResolvesTiers(t *testing.T) {
	c, _, err := project(t, tiered)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Capture([]string{"planner", "builder"}, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Routing["planner"]; ok || len(s.Routing) != 1 || s.ConfidenceFloor != 0.7 || s.RoutingOff {
		t.Fatalf("routing = %+v floor %v off %v", s.Routing, s.ConfidenceFloor, s.RoutingOff)
	}
	rt := s.Routing["builder"]
	if !strings.Contains(rt.Instructions, "implementing this plan") || rt.Default() != 1 ||
		strings.Join(rt.Criteria(), "|") != "Mechanical change|Behaviour change|Cross-cutting change" {
		t.Fatalf("routing = %+v", rt)
	}
	// A tier sets only what differs; the rest is the agent's own resolution.
	for i, want := range []string{"openai-codex/gpt-6-luna/medium", "openai-codex/kimi-k2.6/medium", "openai-codex/gpt-6-astra/high"} {
		a := rt.Tiers[i].Resolved
		if got := a.Provider + "/" + a.Model + "/" + a.Thinking; got != want || !strings.Contains(a.SystemPrompt, "You are the builder") {
			t.Errorf("tier %d = %s, want %s", i, got, want)
		}
	}
	if a := s.Agents["builder"]; a.Model != "kimi-k2.6" {
		t.Fatalf("the agent's own config moved: %+v", a)
	}
}

func TestCaptureFlagsTurnRoutingOff(t *testing.T) {
	c, _, err := project(t, tiered)
	if err != nil {
		t.Fatal(err)
	}
	for _, ov := range []Overrides{{Provider: "p"}, {Model: "m"}, {Thinking: "low"}} {
		s, err := c.Capture([]string{"builder"}, ov)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Routing) != 0 || !s.RoutingOff {
			t.Fatalf("%+v: routing = %+v off %v", ov, s.Routing, s.RoutingOff)
		}
	}
	// No tiers is no routing, and nothing turned it off.
	s, err := assets(t).Capture([]string{"builder"}, Overrides{Model: "m"})
	if err != nil || s.Routing != nil || s.RoutingOff || s.ConfidenceFloor != 0 {
		t.Fatalf("untiered: %+v %v", s, err)
	}
}

func TestCaptureDefaultFloor(t *testing.T) {
	c, _, err := project(t, strings.Replace(tiered, "confidence_floor = 0.7", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Capture([]string{"builder"}, Overrides{})
	if err != nil || s.ConfidenceFloor != 0.5 {
		t.Fatalf("floor = %v, %v", s.ConfidenceFloor, err)
	}
}

func TestLoadProjectRejectsBadTiers(t *testing.T) {
	two := "[[agents.%s.tiers]]\nwhen = \"a\"\ndefault = true\n[[agents.%s.tiers]]\nwhen = \"b\"\n"
	for name, body := range map[string]string{
		"unroutable agent": strings.ReplaceAll(two, "%s", "tester"),
		"one tier":         "[[agents.builder.tiers]]\nwhen = \"a\"\ndefault = true\n",
		"eleven tiers":     "[[agents.builder.tiers]]\nwhen = \"a\"\ndefault = true\n" + strings.Repeat("[[agents.builder.tiers]]\nwhen = \"b\"\n", 10),
		"no default":       "[[agents.planner.tiers]]\nwhen = \"a\"\n[[agents.planner.tiers]]\nwhen = \"b\"\n",
		"two defaults":     "[[agents.planner.tiers]]\nwhen = \"a\"\ndefault = true\n[[agents.planner.tiers]]\nwhen = \"b\"\ndefault = true\n",
		"empty when":       "[[agents.planner.tiers]]\nwhen = \" \"\ndefault = true\n[[agents.planner.tiers]]\nwhen = \"b\"\n",
		"unknown tier key": strings.ReplaceAll(two, "%s", "planner") + "timeout = \"1m\"\n",
		"floor above 1":    "[routing]\nconfidence_floor = 1.5\n",
		"floor below 0":    "[routing]\nconfidence_floor = -0.1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if loaded, err := projectErr(t, body); err == nil || loaded {
				t.Fatalf("accepted: %v", loaded)
			}
		})
	}
	if _, err := projectErr(t, strings.ReplaceAll(two, "%s", "planner")); err != nil {
		t.Fatal(err)
	}
}

func projectErr(t *testing.T, body string) (bool, error) {
	t.Helper()
	_, loaded, err := project(t, body)
	return loaded, err
}
