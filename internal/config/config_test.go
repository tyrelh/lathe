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
