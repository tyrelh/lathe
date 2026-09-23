package main

import (
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/workflow"
)

// The embedded copy is the one that ships. A //go:embed pattern that missed a
// prompt would otherwise only show up as a broken run on someone else's machine.
func TestEmbeddedAssetsResolveTheRoster(t *testing.T) {
	cfg, err := config.Load(Assets)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cfg.Resolve("scout", config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.SystemPrompt, "You are the scout") || !strings.Contains(a.UserPrompt, "## Report") {
		t.Fatal("the scout's prompts are not in the binary")
	}
}

func TestEmbeddedBuilder(t *testing.T) {
	cfg, err := config.Load(Assets)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cfg.Resolve("builder", config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Tools, ",") != "read,grep,find,ls,write,edit" {
		t.Fatalf("builder tools: %v", a.Tools)
	}
	if !strings.Contains(a.SystemPrompt, "You are the builder") || !strings.Contains(a.UserPrompt, `"needed"`) {
		t.Fatal("builder prompts missing")
	}
}

func TestEmbeddedTester(t *testing.T) {
	cfg, err := config.Load(Assets)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cfg.Resolve("tester", config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Tools, ",") != "read,grep,find,ls,bash" {
		t.Fatalf("tester tools: %v", a.Tools)
	}
	if !strings.Contains(a.SystemPrompt, "You are the tester") || !strings.Contains(a.UserPrompt, `"command"`) {
		t.Fatal("tester prompts missing")
	}
}

func TestEmbeddedPlanReviewer(t *testing.T) {
	cfg, err := config.Load(Assets)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cfg.Resolve("plan-reviewer", config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Model != "kimi-k3" || strings.Join(a.Tools, ",") != "read,grep,find,ls" {
		t.Fatalf("reviewer configuration: %+v", a)
	}
	if !strings.Contains(a.SystemPrompt, "You are the plan-reviewer") || !strings.Contains(a.UserPrompt, `"feedback"`) {
		t.Fatal("reviewer prompts missing")
	}
}

// Every workflow's whole roster resolves out of the embedded assets, which is
// what catches an agent added to a graph with no row in roster.toml or no prompt
// file — a failure that would otherwise wait for someone to run that workflow.
func TestEmbeddedRostersResolve(t *testing.T) {
	cfg, err := config.Load(Assets)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, ok := workflow.Graphs["build"]; !ok || len(workflow.Agents["build"]) != 11 {
		t.Fatalf("build roster: %v", workflow.Agents["build"])
	}
}
