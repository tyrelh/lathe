package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	_, err := assets(t).Resolve("builder", Overrides{})
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
