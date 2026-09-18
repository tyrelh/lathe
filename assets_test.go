package main

import (
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/config"
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
