package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tyrelh/lathe/internal/agentconfig"
	"github.com/tyrelh/lathe/internal/config"
	"gopkg.in/yaml.v3"
)

// The Agents page's writes, end to end through a seeded home: a save or reset
// answers with the agent as its page now loads it, a stale hash is a conflict
// that writes nothing, and the file always comes from the roster.
func TestPromptWriteRoutes(t *testing.T) {
	ro := readOnly(t, nil)
	home, seed, roster := seededRoster(t)
	h := handler(ro, t.TempDir(), "dev", Actions{
		SavePrompt: func(key, body, baseHash string) error { return agentconfig.SaveFile(home, key, body, baseHash) },
		ResetPrompt: func(key, baseHash string) error {
			return agentconfig.ResetFile(home, seed, "v1", key, baseHash)
		},
		SaveConfig: func(agent, provider, model, thinking, baseHash string) error {
			return agentconfig.SaveSettings(home, agent, agentconfig.Settings{Provider: provider, Model: model, Thinking: thinking}, baseHash)
		},
	}, Sources{Roster: roster})

	type prompt struct {
		Role   string `json:"role"`
		Key    string `json:"key"`
		Path   string `json:"path"`
		Body   string `json:"body"`
		Seed   string `json:"seed"`
		Hash   string `json:"hash"`
		Edited bool   `json:"edited"`
		Drift  bool   `json:"drift"`
	}
	type settings struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Thinking string `json:"thinking"`
		Path     string `json:"path"`
		Body     string `json:"body"`
		Hash     string `json:"hash"`
		Edited   bool   `json:"edited"`
	}
	type agent struct {
		Name    string    `json:"name"`
		Config  *settings `json:"config"`
		Prompts []prompt  `json:"prompts"`
	}
	send := func(method, path string, body any) (int, map[string]any, agent) {
		b, _ := json.Marshal(body)
		w := browserWrite(h, method, path, Addr, "http://"+Addr, "application/json", string(b))
		var out map[string]any
		var a agent
		json.Unmarshal(w.Body.Bytes(), &out)
		json.Unmarshal(w.Body.Bytes(), &a)
		return w.Code, out, a
	}
	save := func(path, body, hash string) (int, map[string]any, agent) {
		return send("PUT", path, map[string]string{"body": body, "base_hash": hash})
	}
	reset := func(path, hash string) (int, map[string]any, agent) {
		return send("DELETE", path, map[string]string{"base_hash": hash})
	}
	detail := func(name string) agent {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, local("GET", "/api/agents/"+name, nil))
		var a agent
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || w.Code != 200 {
			t.Fatalf("GET /api/agents/%s: %d %s", name, w.Code, w.Body)
		}
		return a
	}
	file := filepath.Join(home, "agents", "planner", "system.md")
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	sys := detail("planner").Prompts[0]
	seeded := sys.Body
	if sys.Role != "system" || sys.Edited || sys.Drift || sys.Path != file || sys.Seed != seeded || sys.Hash != config.Hash(seeded) || read(file) != seeded {
		t.Fatalf("planner system before any save = %+v", sys)
	}
	path := "/api/agents/planner/prompts/system"

	for _, method := range []string{"PUT", "DELETE"} {
		body := `{"body":"x","base_hash":"` + sys.Hash + `"}`
		for _, c := range []struct{ host, origin, contentType string }{
			{Addr, "", "application/json"}, {Addr, "http://evil.example", "application/json"},
			{Addr, "http://" + Addr, "text/plain"}, {"evil.example:4700", "http://evil.example:4700", "application/json"},
		} {
			if w := browserWrite(h, method, path, c.host, c.origin, c.contentType, body); w.Code != http.StatusForbidden {
				t.Errorf("%s to %s from %q as %s: %d", method, c.host, c.origin, c.contentType, w.Code)
			}
		}
	}
	if read(file) != seeded {
		t.Fatal("a forbidden write changed the file")
	}

	for _, c := range []struct {
		path, body, hash string
		code             int
		error            string
	}{
		{path, " \n\t", sys.Hash, http.StatusBadRequest, "a prompt can't be empty; reset it to go back to the seed"},
		{path, "plan", "", http.StatusBadRequest, ""},
		{"/api/agents/planner/prompts/user", "no placeholder", sys.Hash, http.StatusBadRequest, "a user prompt has to contain {{request}}"},
		{"/api/agents/route-plan/prompts/user", "{{request}}", sys.Hash, http.StatusNotFound, ""},
		{"/api/agents/nope/prompts/system", "plan", sys.Hash, http.StatusNotFound, ""},
	} {
		code, out, _ := save(c.path, c.body, c.hash)
		if code != c.code || out["error"] == nil || (c.error != "" && out["error"] != c.error) {
			t.Errorf("PUT %s %q: %d %v", c.path, c.body, code, out)
		}
	}
	for _, p := range []string{"/api/agents/route-plan/prompts/user", "/api/agents/nope/prompts/system"} {
		if code, out, _ := reset(p, sys.Hash); code != http.StatusNotFound || out["error"] == nil {
			t.Errorf("DELETE %s: %d %v", p, code, out)
		}
	}
	if read(file) != seeded {
		t.Fatal("a refused write changed the file")
	}

	// Saved verbatim to the file, owner-only, and the response is the agent
	// page's own payload.
	edited := "  Plan carefully.\n\n"
	code, _, a := save(path, edited, sys.Hash)
	if code != http.StatusOK || !reflect.DeepEqual(a, detail("planner")) {
		t.Fatalf("save: %d %+v", code, a)
	}
	if got := a.Prompts[0]; !got.Edited || got.Drift || got.Body != edited || got.Seed != seeded || got.Hash != config.Hash(edited) {
		t.Errorf("planner system after save = %+v", got)
	}
	if read(file) != edited {
		t.Fatal("the save did not reach the file")
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Errorf("saved file mode = %v", st.Mode())
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, local("GET", "/api/agents", nil))
	cfg, err := roster()
	if err != nil {
		t.Fatal(err)
	}
	var list []agent
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != len(cfg.Agents)+len(config.Routes) {
		t.Fatalf("list after save: %v %s", err, w.Body)
	}
	for _, a := range list {
		for _, p := range a.Prompts {
			if want := a.Name == "planner" && p.Role == "system"; p.Edited != want {
				t.Errorf("list after save: %s %s = %+v", a.Name, p.Role, p)
			}
		}
	}
	// Saving one reviewer's user prompt leaves the others alone.
	general := detail("code-review-general").Prompts[1]
	if code, _, _ := save("/api/agents/code-review-general/prompts/user", "{{request}} generally", general.Hash); code != http.StatusOK {
		t.Fatalf("reviewer save: %d", code)
	}
	if s := detail("code-review-security").Prompts[1]; s.Edited || s.Body != general.Body {
		t.Errorf("a reviewer save reached code-review-security: %+v", s)
	}

	// A save or reset against the hash the page loaded before someone else's
	// edit is refused with what is there now, and writes nothing.
	conflict := func(what string, code int, out map[string]any) {
		t.Helper()
		if code != http.StatusConflict || out["error"] != "this prompt changed since you loaded it" ||
			out["body"] != edited || out["hash"] != config.Hash(edited) || out["edited"] != true {
			t.Errorf("stale %s: %d %v", what, code, out)
		}
		if read(file) != edited {
			t.Errorf("a stale %s wrote", what)
		}
	}
	code, out, _ := save(path, "Plan something else.", sys.Hash)
	conflict("save", code, out)
	code, out, _ = reset(path, sys.Hash)
	conflict("reset", code, out)
	if code, out, _ := send("DELETE", path, map[string]string{}); code != http.StatusBadRequest || out["error"] != "base_hash is required" {
		t.Errorf("reset without a hash: %d %v", code, out)
	}

	// Drift: an edited file whose recorded seed differs from the binary's.
	manifest := filepath.Join(home, agentconfig.ManifestName)
	var m map[string]map[string]map[string]string
	if err := yaml.Unmarshal([]byte(read(manifest)), &m); err != nil {
		t.Fatal(err)
	}
	m["files"]["agents/planner/system.md"]["hash"] = "an older seed"
	b, _ := yaml.Marshal(m)
	os.WriteFile(manifest, b, 0o600)
	if got := detail("planner").Prompts[0]; !got.Drift || !got.Edited || got.Seed != seeded || got.Body != edited {
		t.Errorf("drifted prompt = %+v", got)
	}

	// Reset with the current hash restores the seed.
	if code, _, a := reset(path, config.Hash(edited)); code != http.StatusOK || a.Prompts[0].Edited || a.Prompts[0].Drift || a.Prompts[0].Body != seeded {
		t.Errorf("reset: %d %+v", code, a)
	}
	if read(file) != seeded {
		t.Fatal("the reset did not restore the seed")
	}

	// Config: saving keeps the file's comments, a stale hash is a conflict
	// that writes nothing, and bad values are refused.
	configPath := filepath.Join(home, "agents", "planner", "config.yaml")
	before := detail("planner").Config
	edit := func(hash, thinking string) (int, map[string]any, agent) {
		return send("PUT", "/api/agents/planner/config", map[string]string{
			"provider": "anthropic", "model": "claude", "thinking": thinking, "base_hash": hash})
	}
	code, _, a = edit(before.Hash, "high")
	if code != http.StatusOK || a.Config == nil || a.Config.Provider != "anthropic" || a.Config.Model != "claude" || a.Config.Thinking != "high" || !a.Config.Edited {
		t.Fatalf("config save: %d %+v", code, a.Config)
	}
	saved := read(configPath)
	for _, want := range []string{"# Read the repo and produce the plan", "# Routing tiers, cheapest first.", "provider: anthropic", "model: claude", "tiers:"} {
		if !strings.Contains(saved, want) {
			t.Errorf("saved config lacks %q:\n%s", want, saved)
		}
	}
	code, out, _ = edit(before.Hash, "low")
	if code != http.StatusConflict || out["error"] != "this configuration changed since you loaded it" || read(configPath) != saved {
		t.Errorf("stale config save: %d %v", code, out)
	}
	if code, _, _ := edit(config.Hash(saved), "max"); code != http.StatusBadRequest || read(configPath) != saved {
		t.Errorf("bad thinking: %d", code)
	}
	if code, _, _ := send("PUT", "/api/agents/route-plan/config", map[string]string{
		"provider": "p", "model": "m", "thinking": "low", "base_hash": "x"}); code != http.StatusNotFound {
		t.Errorf("routing phase config: %d", code)
	}

	// Without the actions, no write route exists.
	bare := handler(ro, t.TempDir(), "dev", Actions{}, Sources{Roster: roster})
	for _, c := range []struct{ method, path string }{{"PUT", path}, {"DELETE", path}, {"PUT", "/api/agents/planner/config"}} {
		assertNoWriteRoute(t, bare, c.method, c.path)
	}
	if read(file) != seeded {
		t.Fatal("a read-only dashboard wrote")
	}
}
