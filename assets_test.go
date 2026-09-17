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
