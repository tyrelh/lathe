package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// override writes body as key's prompt override under dir.
func override(t *testing.T, dir, key, body string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, fsys fs.FS, key string) string {
	t.Helper()
	b, err := fs.ReadFile(fsys, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An override file wins; no file, a directory where the file would be, or a
// file where its directory would be, falls back to the built-in.
func TestOverlay(t *testing.T) {
	dir := t.TempDir()
	override(t, dir, "prompts/alpha/system.md", "alpha override")
	override(t, dir, "prompts/gamma", "stray file")
	if err := os.MkdirAll(filepath.Join(dir, "prompts", "shared", "user.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	fsys := Overlay(dir, promptsFS())
	for key, want := range map[string]string{
		"prompts/alpha/system.md": "alpha override",
		"prompts/gamma/system.md": "gamma system",
		"prompts/shared/user.md":  "shared {{request}}",
	} {
		if got := read(t, fsys, key); got != want {
			t.Errorf("%s = %q; want %q", key, got, want)
		}
	}
	if _, err := fsys.Open("prompts/../roster.toml"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("invalid path: err = %v", err)
	}
}

// A named pipe at an override path falls back rather than blocking on open.
func TestOverlayFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts", "alpha", "system.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("named pipes unavailable: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		b, _ := fs.ReadFile(Overlay(dir, promptsFS()), "prompts/alpha/system.md")
		done <- string(b)
	}()
	select {
	case got := <-done:
		if got != "alpha system" {
			t.Fatalf("read %q; want the built-in", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading through a named pipe override blocked")
	}
}

// What a machine can change about the roster stays what lathe.toml allows.
func TestOverlayNeverServesRoster(t *testing.T) {
	dir := t.TempDir()
	override(t, dir, "roster.toml", "[[agents]]\nname = \"intruder\"\n")
	c, err := Load(Overlay(dir, promptsFS()))
	if err != nil {
		t.Fatal(err)
	}
	if c.hasAgent("intruder") || !c.hasAgent("alpha") {
		t.Fatalf("roster = %+v; want the base roster", c.Agents)
	}
}

func TestOverridePath(t *testing.T) {
	dir := t.TempDir()
	got, err := OverridePath(dir, "prompts/alpha/system.md")
	if want := filepath.Join(dir, "prompts", "alpha", "system.md"); err != nil || got != want {
		t.Fatalf("OverridePath = %q, %v; want %q", got, err, want)
	}
	base := promptsFS()
	base["roster.md"] = &fstest.MapFile{Data: []byte("x")}
	for _, key := range []string{"", "roster.toml", "roster.md", "prompts", "prompts/", "/prompts/x.md", "prompts/../roster.toml", "../prompts/x.md", "prompts/./x.md"} {
		if _, err := OverridePath(dir, key); err == nil {
			t.Errorf("OverridePath(%q) accepted", key)
		}
		if err := SavePrompt(dir, base, key, "x", PromptHash("x")); err == nil {
			t.Errorf("SavePrompt(%q) accepted", key)
		}
		if err := ResetPrompt(dir, key); err == nil {
			t.Errorf("ResetPrompt(%q) accepted", key)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("rejected keys wrote %v", entries)
	}
}

func TestSavePrompt(t *testing.T) {
	dir := t.TempDir()
	key := "prompts/alpha/system.md"
	path := filepath.Join(dir, "prompts", "alpha", "system.md")
	base := promptsFS()

	if err := SavePrompt(dir, base, key, "first edit\n", PromptHash("alpha system")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "first edit\n" {
		t.Fatalf("override = %q, %v", b, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("override mode: %v, %v", st, err)
	}
	// Saving against the override's own hash replaces it, and the temp file the
	// write went through is gone.
	if err := SavePrompt(dir, base, key, "second edit", PromptHash("first edit\n")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, Overlay(dir, base), key); got != "second edit" {
		t.Fatalf("override = %q", got)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err != nil || len(entries) != 1 || entries[0].Name() != "system.md" {
		t.Fatalf("directory holds %v, %v; want only system.md", entries, err)
	}
	// A body equal to the built-in leaves no override behind.
	if err := SavePrompt(dir, base, key, "alpha system", PromptHash("second edit")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("override still there: %v", err)
	}
}

// A stale hash writes nothing, whether the page loaded the built-in or an
// override that has since changed.
func TestSavePromptConflict(t *testing.T) {
	dir := t.TempDir()
	key := "prompts/alpha/system.md"
	if err := SavePrompt(dir, promptsFS(), key, "edit", PromptHash("not what is there")); !errors.Is(err, ErrPromptConflict) {
		t.Fatalf("err = %v; want ErrPromptConflict", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a conflict wrote %v", entries)
	}
	path := override(t, dir, key, "hand edit")
	if err := SavePrompt(dir, promptsFS(), key, "edit", PromptHash("alpha system")); !errors.Is(err, ErrPromptConflict) {
		t.Fatalf("err = %v; want ErrPromptConflict", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "hand edit" {
		t.Fatalf("override = %q; want the hand edit", b)
	}
}

func TestSavePromptUnknownKey(t *testing.T) {
	dir := t.TempDir()
	if err := SavePrompt(dir, promptsFS(), "prompts/nobody.md", "x", PromptHash("")); err == nil {
		t.Fatal("a key the roster's FS lacks was saved")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %v", entries)
	}
}

func TestResetPrompt(t *testing.T) {
	dir := t.TempDir()
	key := "prompts/alpha/system.md"
	if err := ResetPrompt(dir, key); err != nil {
		t.Fatalf("missing override: %v", err)
	}
	path := override(t, dir, key, "edit")
	if err := ResetPrompt(dir, key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("override still there: %v", err)
	}
}

func TestPromptsOverlay(t *testing.T) {
	dir := t.TempDir()
	override(t, dir, "prompts/alpha/system.md", "alpha override")
	c, err := Load(Overlay(dir, promptsFS()))
	if err != nil {
		t.Fatal(err)
	}
	for _, bodies := range []bool{true, false} {
		got, err := c.Prompts(bodies)
		if err != nil {
			t.Fatal(err)
		}
		sys, user := got[0].Prompts[0], got[0].Prompts[1]
		if sys.OverridePath != filepath.Join(dir, "prompts", "alpha", "system.md") || !sys.Overridden {
			t.Fatalf("bodies %v: alpha system = %+v", bodies, sys)
		}
		if user.OverridePath != filepath.Join(dir, "prompts", "shared", "user.md") || user.Overridden {
			t.Fatalf("bodies %v: alpha user = %+v", bodies, user)
		}
		if !bodies {
			if sys.Body != "" || sys.Builtin != "" || sys.Hash != "" {
				t.Fatalf("bodies false: alpha system = %+v", sys)
			}
			continue
		}
		if sys.Body != "alpha override" || sys.Builtin != "alpha system" || sys.Hash != PromptHash("alpha override") {
			t.Fatalf("alpha system = %+v", sys)
		}
		if user.Body != "shared {{request}}" || user.Builtin != user.Body || user.Hash != PromptHash(user.Body) {
			t.Fatalf("alpha user = %+v", user)
		}
	}
	// A run resolves the override too.
	if a := resolve(t, c, "alpha", Overrides{}); a.SystemPrompt != "alpha override" {
		t.Fatalf("resolved system prompt = %q", a.SystemPrompt)
	}
}
