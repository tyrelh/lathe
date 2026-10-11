package main

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/agentconfig"
	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/workflow"
)

// embedded seeds a fresh home from the embedded seed and loads it.
func embedded(t *testing.T) config.Config {
	t.Helper()
	cfg, err := agentconfig.Open(t.TempDir(), Seed, "test")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The embedded seed covers exactly the compiled agents, plus routing.
func TestSeedCoversTheRoster(t *testing.T) {
	dirs, err := fs.ReadDir(Seed, "agents")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != len(config.Permissions) {
		t.Fatalf("seed has %d agent dirs; the binary knows %d agents", len(dirs), len(config.Permissions))
	}
	for _, p := range config.Permissions {
		for _, f := range []string{"config.yaml", "system.md", "user.md"} {
			if _, err := fs.Stat(Seed, config.AgentFile(p.Name, f)); err != nil {
				t.Errorf("seed: %v", err)
			}
		}
	}
	for _, r := range config.Routes {
		if _, err := fs.Stat(Seed, r.Key); err != nil {
			t.Errorf("seed: %v", err)
		}
	}
}

// Each reviewer has its own copy of the code-review report contract.
func TestReviewersHaveTheirOwnUserPrompt(t *testing.T) {
	var first string
	for _, name := range []string{"code-review-general", "code-review-security", "code-review-slop", "code-review-adversarial"} {
		b, err := fs.ReadFile(Seed, config.AgentFile(name, "user.md"))
		if err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = string(b)
		}
		if string(b) != first || !strings.Contains(first, `"findings"`) {
			t.Errorf("%s user prompt differs from the shared contract", name)
		}
	}
}

func TestEmbeddedSettings(t *testing.T) {
	cfg := embedded(t)
	for name, want := range map[string]string{
		"scout": "moonshotai/kimi-k2.7-code/medium", "planner": "moonshotai/kimi-k3/medium",
		"builder": "moonshotai/kimi-k2.7-code/medium", "tester": "moonshotai/kimi-k2.6/medium",
		"plan-reviewer": "moonshotai/kimi-k3/medium", "pr-author": "moonshotai/kimi-k2.6/medium",
	} {
		a, err := cfg.Resolve(name, config.Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Provider + "/" + a.Model + "/" + a.Thinking; got != want || a.Deadline.Minutes() != 10 {
			t.Errorf("%s = %s %v; want %s", name, got, a.Deadline, want)
		}
	}
	for name, want := range map[string]bool{"builder": true, "brancher": false, "committer": false, "pr-author": false} {
		if a, _ := cfg.Resolve(name, config.Overrides{}); a.WantsRepoInstructions() != want {
			t.Errorf("%s: repo instructions = %v", name, a.WantsRepoInstructions())
		}
	}
	for name, models := range map[string]string{"planner": "kimi-k2.7-code,kimi-k3,kimi-k3", "builder": "kimi-k2.6,kimi-k2.7-code,kimi-k3"} {
		s, err := cfg.Capture([]string{name}, config.Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		rt := s.Routing[name]
		var got []string
		for _, tier := range rt.Tiers {
			got = append(got, tier.Resolved.Model)
		}
		if strings.Join(got, ",") != models || rt.Default() != 1 || rt.Tiers[2].Resolved.Thinking != "high" || rt.Instructions == "" {
			t.Errorf("%s tiers = %v", name, got)
		}
	}
}

// Every workflow's whole roster resolves out of the seed, which is what
// catches an agent added to a graph with no seed files.
func TestEmbeddedRostersResolve(t *testing.T) {
	cfg := embedded(t)
	for name, agents := range workflow.Agents {
		snapshot, err := cfg.Capture(agents, config.Overrides{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, agent := range agents {
			a := snapshot.Agents[agent]
			if !strings.Contains(a.SystemPrompt, "You are the "+agent) {
				t.Errorf("%s: %s has no system prompt of its own", name, agent)
			}
			if !strings.Contains(a.UserPrompt, "## Report") {
				t.Errorf("%s: %s has no report contract", name, agent)
			}
		}
	}
	if _, ok := workflow.Graphs["build"]; !ok || len(workflow.Agents["build"]) != 12 {
		t.Fatalf("build roster: %v", workflow.Agents["build"])
	}
	a, _ := cfg.Resolve("builder", config.Overrides{})
	if strings.Join(a.Tools, ",") != "read,grep,find,ls,write,edit" {
		t.Fatalf("builder tools: %v", a.Tools)
	}
}
