package agentconfig

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/tyrelh/lathe/internal/config"
)

// shipped is the seed the binary embeds.
func shipped(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(os.DirFS(filepath.Join("..", "..")), "seed")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHome(t *testing.T) {
	t.Setenv("HOME", "/somewhere")
	if h, err := Home(); err != nil || h != filepath.Join("/somewhere", ".lathe") {
		t.Fatalf("home = %q, %v", h, err)
	}
}

// Seeding an empty home writes every file and records each in the manifest.
func TestSeedEmptyHome(t *testing.T) {
	home, seed := t.TempDir(), shipped(t)
	r, err := Seed(home, seed, "v1")
	if err != nil {
		t.Fatal(err)
	}
	files, err := seedFiles(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Written, files) || len(r.Replaced) != 0 || len(r.Drift) != 0 {
		t.Fatalf("report = %+v", r)
	}
	m, err := readManifest(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range files {
		want, _ := fs.ReadFile(seed, rel)
		if got := read(t, filepath.Join(home, rel)); got != string(want) {
			t.Errorf("%s differs from its seed", rel)
		}
		if e := m.Files[rel]; e.Hash != config.Hash(string(want)) || e.Version != "v1" {
			t.Errorf("%s manifest entry = %+v", rel, e)
		}
	}
	for _, p := range config.Permissions {
		for _, f := range []string{"config.yaml", "system.md", "user.md"} {
			if !slices.Contains(files, config.AgentFile(p.Name, f)) {
				t.Errorf("seed has no %s", config.AgentFile(p.Name, f))
			}
		}
	}
	// A second seed changes nothing.
	if r, err := Seed(home, seed, "v1"); err != nil || len(r.Written)+len(r.Replaced)+len(r.Drift) != 0 {
		t.Fatalf("reseed = %+v, %v", r, err)
	}
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Resolve("builder", config.Overrides{})
	if err != nil || a.Model != "claude-opus-5-5" || a.Provider != "anthropic" || a.Thinking != "low" ||
		!strings.Contains(a.SystemPrompt, "You are the builder") || len(c.Tiers["builder"]) != 3 || len(c.Tiers["planner"]) != 3 {
		t.Fatalf("builder = %+v, tiers %v, %v", a, c.Tiers, err)
	}
}

// An upgrade replaces untouched files, keeps and flags edited ones, fills
// missing ones and ignores directories for agents the binary does not know.
func TestSeedUpgrade(t *testing.T) {
	home := t.TempDir()
	old := fstest.MapFS{
		"agents/scout/system.md": {Data: []byte("old scout")},
		"agents/scout/user.md":   {Data: []byte("old user {{request}}")},
		"agents/scout/extra.md":  {Data: []byte("unchanged")},
	}
	if _, err := Seed(home, old, "v1"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "agents/scout/user.md"), "my user {{request}}")
	os.Remove(filepath.Join(home, "agents/scout/extra.md"))
	write(t, filepath.Join(home, "agents/stranger/config.yaml"), "anything: at all\n")
	next := fstest.MapFS{
		"agents/scout/system.md": {Data: []byte("new scout")},
		"agents/scout/user.md":   {Data: []byte("new user {{request}}")},
		"agents/scout/extra.md":  {Data: []byte("unchanged")},
	}
	r, err := Seed(home, next, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Replaced, []string{"agents/scout/system.md"}) || !slices.Equal(r.Drift, []string{"agents/scout/user.md"}) ||
		!slices.Equal(r.Written, []string{"agents/scout/extra.md"}) {
		t.Fatalf("report = %+v", r)
	}
	if read(t, filepath.Join(home, "agents/scout/system.md")) != "new scout" || read(t, filepath.Join(home, "agents/scout/user.md")) != "my user {{request}}" ||
		read(t, filepath.Join(home, "agents/scout/extra.md")) != "unchanged" || read(t, filepath.Join(home, "agents/stranger/config.yaml")) != "anything: at all\n" {
		t.Fatal("files after the upgrade are wrong")
	}
	m, _ := readManifest(home)
	if m.Files["agents/scout/system.md"].Version != "v2" || m.Files["agents/scout/user.md"].Version != "v1" {
		t.Fatalf("manifest = %+v", m)
	}
	// An edit whose seed has not changed is not drift.
	if r, err := Seed(home, next, "v2"); err != nil || len(r.Drift) != 1 {
		t.Fatalf("reseed = %+v %v", r, err)
	}
	write(t, filepath.Join(home, "agents/scout/system.md"), "my scout")
	if r, _ := Seed(home, next, "v2"); slices.Contains(r.Drift, "agents/scout/system.md") {
		t.Fatal("an edit against the current seed was flagged as drift")
	}
}

// An untouched file is only replaced by a demonstrably newer release: a
// rollback, the same version, or an unordered one such as dev leaves it.
func TestSeedReplacesOnlyForNewerVersions(t *testing.T) {
	for _, tc := range []struct {
		recorded, binary string
		replace          bool
	}{
		{"v1.2.0", "v1.3.0", true},
		{"v1.2.9", "v1.10.0", true},
		{"1.2.0", "v2", true},
		{"v1.3.0", "v1.2.0", false},
		{"v1.3.0", "v1.3.0", false},
		{"dev", "v1.3.0", false},
		{"v1.3.0", "dev", false},
		{"dev", "dev", false},
		{"v1.2.0", "v1.3.0-rc1", false},
	} {
		home := t.TempDir()
		old := fstest.MapFS{"agents/scout/system.md": {Data: []byte("old")}}
		next := fstest.MapFS{"agents/scout/system.md": {Data: []byte("new")}}
		if _, err := Seed(home, old, tc.recorded); err != nil {
			t.Fatal(err)
		}
		r, err := Seed(home, next, tc.binary)
		if err != nil {
			t.Fatal(err)
		}
		got := read(t, filepath.Join(home, "agents/scout/system.md"))
		if (got == "new") != tc.replace || (len(r.Replaced) == 1) != tc.replace || len(r.Drift) != 0 {
			t.Errorf("%s -> %s: file %q, report %+v", tc.recorded, tc.binary, got, r)
		}
		m, _ := readManifest(home)
		if want := map[bool]string{true: tc.binary, false: tc.recorded}[tc.replace]; m.Files["agents/scout/system.md"].Version != want {
			t.Errorf("%s -> %s: recorded version %q", tc.recorded, tc.binary, m.Files["agents/scout/system.md"].Version)
		}
	}
}

// Writers of one home take turns: of several saves against the same hash only
// one lands, and a seed upgrade racing a save never overwrites the saved edit.
func TestWritesAreSerialized(t *testing.T) {
	home := t.TempDir()
	key := "agents/scout/system.md"
	old := fstest.MapFS{key: {Data: []byte("old")}}
	if _, err := Seed(home, old, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	landed := 0
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := SaveFile(home, key, strings.Repeat("x", i+1), config.Hash("old"))
			if err != nil && !errors.Is(err, config.ErrConflict) {
				t.Error(err)
			}
			if err == nil {
				mu.Lock()
				landed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if landed != 1 {
		t.Fatalf("%d saves against the same hash landed; want 1", landed)
	}

	next := fstest.MapFS{key: {Data: []byte("new")}}
	for i := range 20 {
		home := t.TempDir()
		path := filepath.Join(home, key)
		if _, err := Seed(home, old, "v1.0.0"); err != nil {
			t.Fatal(err)
		}
		var saveErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, err := Seed(home, next, "v2.0.0"); _ = err }()
		go func() { defer wg.Done(); saveErr = SaveFile(home, key, "mine", config.Hash("old")) }()
		wg.Wait()
		got := read(t, path)
		switch {
		case saveErr == nil && got != "mine":
			t.Fatalf("round %d: the save landed and the upgrade overwrote it with %q", i, got)
		case errors.Is(saveErr, config.ErrConflict) && got != "new":
			t.Fatalf("round %d: the save was refused but the file is %q", i, got)
		case saveErr != nil && !errors.Is(saveErr, config.ErrConflict):
			t.Fatal(saveErr)
		}
	}
}

// Seeded files and the manifest are owner-only, in owner-only directories.
func TestSeedIsOwnerOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".lathe")
	if _, err := Seed(home, shipped(t), "v1"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"agents/scout/system.md", "agents/planner/config.yaml", "routing/builder.md", ManifestName} {
		if st, err := os.Stat(filepath.Join(home, p)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", p, st.Mode(), err)
		}
	}
	for _, d := range []string{"", "agents", "agents/scout", "routing"} {
		if st, err := os.Stat(filepath.Join(home, d)); err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("dir %q: %v %v", d, st.Mode(), err)
		}
	}
	if err := SaveSettings(home, "scout", Settings{Provider: "p", Model: "m", Thinking: "low"}, config.Hash(read(t, filepath.Join(home, "agents/scout/config.yaml")))); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(home, "agents/scout/config.yaml")); st.Mode().Perm() != 0o600 {
		t.Errorf("saved config mode = %v", st.Mode())
	}
}

// The whole shipped seed, with an unknown agent's directory beside it.
func TestOpenIgnoresUnknownAgents(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "agents/stranger/config.yaml"), "not: valid: yaml: [")
	c, err := Open(home, shipped(t), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve("stranger", config.Overrides{}); err == nil {
		t.Fatal("an unknown agent loaded")
	}
	for key, f := range c.Files {
		if f.Edited || f.Drift || f.Seed != f.Body {
			t.Errorf("%s: %+v", key, f)
		}
	}
}

func TestOpenMarksEditsAndDrift(t *testing.T) {
	home, seed := t.TempDir(), shipped(t)
	if _, err := Open(home, seed, "v1"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "agents/scout/system.md"), "You scout by edit.\n")
	c, err := Open(home, seed, "v1")
	if err != nil {
		t.Fatal(err)
	}
	f := c.Files["agents/scout/system.md"]
	if !f.Edited || f.Drift || f.Body != "You scout by edit.\n" || !strings.Contains(f.Seed, "You are the scout") {
		t.Fatalf("edited file = %+v", f)
	}
	m, _ := readManifest(home)
	m.Files["agents/scout/system.md"] = entry{Hash: "older seed", Version: "v0"}
	if err := writeManifest(home, m); err != nil {
		t.Fatal(err)
	}
	c, err = Open(home, seed, "v1")
	if err != nil || !c.Files["agents/scout/system.md"].Drift {
		t.Fatalf("drift not flagged: %+v %v", c.Files["agents/scout/system.md"], err)
	}
}

// A missing or invalid file refuses the load with the file named; a file
// missing before seeding is filled instead.
func TestLoadRefusesBadFiles(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		remove           bool
	}{
		{"missing config", "agents/builder/config.yaml", "", true},
		{"missing prompt", "agents/tester/system.md", "", true},
		{"missing routing", "routing/planner.md", "", true},
		{"empty prompt", "agents/scout/system.md", " \n", false},
		{"no request", "agents/scout/user.md", "no placeholder", false},
		{"bad yaml", "agents/builder/config.yaml", "provider: [", false},
		{"second document", "agents/builder/config.yaml", "provider: p\nmodel: m\nthinking: low\ntimeout: 1m\n---\nprovider: q\n", false},
		{"unknown key", "agents/builder/config.yaml", "provider: p\nmodel: m\nthinking: low\ntimeout: 1m\ntools: [bash]\n", false},
		{"missing model", "agents/builder/config.yaml", "provider: p\nthinking: low\ntimeout: 1m\n", false},
		{"bad thinking", "agents/builder/config.yaml", "provider: p\nmodel: m\nthinking: max\ntimeout: 1m\n", false},
		{"bad timeout", "agents/builder/config.yaml", "provider: p\nmodel: m\nthinking: low\ntimeout: soon\n", false},
		{"two defaults", "agents/builder/config.yaml", "provider: p\nmodel: m\nthinking: low\ntimeout: 1m\ntiers:\n  - when: a\n    default: true\n  - when: b\n    default: true\n", false},
		{"tiers on tester", "agents/tester/config.yaml", "provider: p\nmodel: m\nthinking: low\ntimeout: 1m\ntiers:\n  - when: a\n    default: true\n  - when: b\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if _, err := Seed(home, shipped(t), "v1"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, filepath.FromSlash(tc.file))
			if tc.remove {
				os.Remove(path)
			} else {
				write(t, path, tc.body)
			}
			_, err := Load(home)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("err = %v; want one naming %s", err, path)
			}
			if !tc.remove && !errors.Is(err, config.ErrInvalid) {
				t.Fatalf("err = %v is not ErrInvalid", err)
			}
			if tc.remove {
				if _, err := Open(home, shipped(t), "v1"); err != nil {
					t.Fatalf("a seed gap was not filled: %v", err)
				}
			}
		})
	}
}

func TestSaveSettingsKeepsComments(t *testing.T) {
	home := t.TempDir()
	if _, err := Seed(home, shipped(t), "v1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "agents/planner/config.yaml")
	before := read(t, path)
	err := SaveSettings(home, "planner", Settings{Provider: "anthropic", Model: "claude", Thinking: "high"}, config.Hash(before))
	if err != nil {
		t.Fatal(err)
	}
	after := read(t, path)
	for _, want := range []string{"# Read the repo and produce the plan", "# Routing tiers, cheapest first.", "provider: anthropic", "model: claude", "thinking: high", "timeout: 10m"} {
		if !strings.Contains(after, want) {
			t.Errorf("saved file lacks %q:\n%s", want, after)
		}
	}
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := c.Resolve("planner", config.Overrides{}); a.Provider != "anthropic" || a.Model != "claude" || a.Thinking != "high" || len(c.Tiers["planner"]) != 3 {
		t.Fatalf("planner = %+v", a)
	}
	// A stale hash writes nothing.
	if err := SaveSettings(home, "planner", Settings{Provider: "x", Model: "y", Thinking: "low"}, config.Hash(before)); !errors.Is(err, config.ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if read(t, path) != after {
		t.Fatal("a stale save wrote")
	}
	if err := SaveSettings(home, "planner", Settings{Provider: "x", Model: "y", Thinking: "max"}, config.Hash(after)); !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("invalid save: %v", err)
	}
	if read(t, path) != after {
		t.Fatal("an invalid save wrote")
	}
}

func TestSaveAndResetFile(t *testing.T) {
	home, seed := t.TempDir(), shipped(t)
	if _, err := Seed(home, seed, "v1"); err != nil {
		t.Fatal(err)
	}
	key := "agents/scout/system.md"
	path := filepath.Join(home, key)
	original := read(t, path)
	if err := SaveFile(home, key, "edited", config.Hash("not it")); !errors.Is(err, config.ErrConflict) || read(t, path) != original {
		t.Fatalf("stale save: %v", err)
	}
	if err := SaveFile(home, key, "edited", config.Hash(original)); err != nil || read(t, path) != "edited" {
		t.Fatalf("save: %v", err)
	}
	if err := SaveFile(home, "../escape", "x", ""); err == nil {
		t.Fatal("a path outside home was accepted")
	}
	m, _ := readManifest(home)
	m.Files[key] = entry{Hash: "older seed", Version: "v0"}
	writeManifest(home, m)
	if err := ResetFile(home, seed, "v2", key, config.Hash(original)); !errors.Is(err, config.ErrConflict) || read(t, path) != "edited" {
		t.Fatalf("stale reset: %v", err)
	}
	if err := ResetFile(home, seed, "v2", key, config.Hash("edited")); err != nil || read(t, path) != original {
		t.Fatalf("reset: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("reset file mode = %v", st.Mode())
	}
	m, _ = readManifest(home)
	if m.Files[key] != (entry{config.Hash(original), "v2"}) {
		t.Fatalf("manifest after reset = %+v", m.Files[key])
	}
	if r, _ := Seed(home, seed, "v2"); slices.Contains(r.Drift, key) {
		t.Fatal("a reset file is drift")
	}
}
