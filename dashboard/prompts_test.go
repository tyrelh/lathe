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

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
)

// The Agents page's two writes, end to end through real overrides in a temp
// data root: a save or reset answers with the agent as its page now loads it,
// a stale hash is a conflict that writes nothing, and the key always comes
// from the roster.
func TestPromptWriteRoutes(t *testing.T) {
	root := t.TempDir()
	if err := trace.Init(root); err != nil {
		t.Fatal(err)
	}
	ro, err := trace.OpenRO(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	dir, assets := t.TempDir(), os.DirFS("..")
	roster := func() (config.Config, error) { return config.Load(config.Overlay(dir, assets)) }
	h := handler(ro, "dev", Actions{
		SavePrompt:  func(key, body, baseHash string) error { return config.SavePrompt(dir, assets, key, body, baseHash) },
		ResetPrompt: func(key string) error { return config.ResetPrompt(dir, key) },
	}, Sources{Roster: roster})

	type prompt struct {
		Role         string `json:"role"`
		Key          string `json:"key"`
		Body         string `json:"body"`
		Builtin      string `json:"builtin"`
		Hash         string `json:"hash"`
		OverridePath string `json:"override_path"`
		Overridden   bool   `json:"overridden"`
	}
	type agent struct {
		Name    string   `json:"name"`
		Prompts []prompt `json:"prompts"`
	}
	request := func(h http.Handler, method, path, host, origin, contentType, body string) *httptest.ResponseRecorder {
		r := local(method, path, strings.NewReader(body))
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	send := func(method, path, body string) (int, map[string]any, agent) {
		w := request(h, method, path, Addr, "http://"+Addr, "application/json", body)
		var out map[string]any
		var a agent
		json.Unmarshal(w.Body.Bytes(), &out)
		json.Unmarshal(w.Body.Bytes(), &a)
		return w.Code, out, a
	}
	save := func(path, body, hash string) (int, map[string]any, agent) {
		b, _ := json.Marshal(map[string]string{"body": body, "base_hash": hash})
		return send("PUT", path, string(b))
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
	overridePath := filepath.Join(dir, "prompts", "planner", "system.md")
	noOverride := func(when string) {
		t.Helper()
		if _, err := os.Stat(overridePath); !os.IsNotExist(err) {
			t.Fatalf("%s: override exists: %v", when, err)
		}
	}

	sys := detail("planner").Prompts[0]
	builtin := sys.Body
	if sys.Role != "system" || sys.Overridden || sys.OverridePath != overridePath || sys.Builtin != builtin || sys.Hash != config.PromptHash(builtin) {
		t.Fatalf("planner system before any save = %+v", sys)
	}
	path := "/api/agents/planner/prompts/system"

	for _, method := range []string{"PUT", "DELETE"} {
		body := `{"body":"x","base_hash":"` + sys.Hash + `"}`
		for _, c := range []struct{ host, origin, contentType string }{
			{Addr, "", "application/json"}, {Addr, "http://evil.example", "application/json"},
			{Addr, "http://" + Addr, "text/plain"}, {"evil.example:4700", "http://evil.example:4700", "application/json"},
		} {
			if w := request(h, method, path, c.host, c.origin, c.contentType, body); w.Code != http.StatusForbidden {
				t.Errorf("%s to %s from %q as %s: %d", method, c.host, c.origin, c.contentType, w.Code)
			}
		}
	}
	noOverride("forbidden writes")

	for _, c := range []struct {
		path, body, hash string
		code             int
		error            string
	}{
		{path, " \n\t", sys.Hash, http.StatusBadRequest, "a prompt can't be empty; reset it to go back to the built-in"},
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
		if code, out, _ := send("DELETE", p, `{}`); code != http.StatusNotFound || out["error"] == nil {
			t.Errorf("DELETE %s: %d %v", p, code, out)
		}
	}
	noOverride("refused writes")

	// Saved verbatim, and the response is the agent page's own payload.
	edited := "  Plan carefully.\n\n"
	code, _, a := save(path, edited, sys.Hash)
	if code != http.StatusOK || !reflect.DeepEqual(a, detail("planner")) {
		t.Fatalf("save: %d %+v", code, a)
	}
	if got := a.Prompts[0]; !got.Overridden || got.Body != edited || got.Builtin != builtin || got.Hash != config.PromptHash(edited) {
		t.Errorf("planner system after save = %+v", got)
	}
	if b, err := os.ReadFile(overridePath); err != nil || string(b) != edited {
		t.Fatalf("override file = %q, %v", b, err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, local("GET", "/api/agents", nil))
	var list []agent
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 15 {
		t.Fatalf("list after save: %v %s", err, w.Body)
	}
	for _, a := range list {
		for _, p := range a.Prompts {
			if want := a.Name == "planner" && p.Role == "system"; p.Overridden != want || p.OverridePath != filepath.Join(dir, p.Key) {
				t.Errorf("list after save: %s %s = %+v", a.Name, p.Role, p)
			}
		}
	}
	for _, field := range []string{`"hash"`, `"builtin"`, `"body"`} {
		if strings.Contains(w.Body.String(), field) {
			t.Errorf("the list carries %s", field)
		}
	}

	// A save against the hash the page loaded before someone else's edit is
	// refused with what is there now.
	code, out, _ := save(path, "Plan something else.", sys.Hash)
	if code != http.StatusConflict || out["error"] != "this prompt changed since you loaded it" ||
		out["body"] != edited || out["hash"] != config.PromptHash(edited) || out["overridden"] != true {
		t.Errorf("stale save: %d %v", code, out)
	}
	if b, _ := os.ReadFile(overridePath); string(b) != edited {
		t.Errorf("a stale save wrote %q", b)
	}

	// Saving the built-in back removes the override rather than shadowing it.
	if code, _, a := save(path, builtin, config.PromptHash(edited)); code != http.StatusOK || a.Prompts[0].Overridden || a.Prompts[0].Body != builtin {
		t.Errorf("save equal to the built-in: %d %+v", code, a)
	}
	noOverride("saving the built-in")

	if code, _, _ := save(path, edited, sys.Hash); code != http.StatusOK {
		t.Fatalf("second save: %d", code)
	}
	for range 2 {
		if code, _, a := send("DELETE", path, `{}`); code != http.StatusOK || a.Prompts[0].Overridden || a.Prompts[0].Body != builtin {
			t.Errorf("reset: %d %+v", code, a)
		}
	}
	noOverride("reset")

	// Without the actions, neither route exists.
	bare := handler(ro, "dev", Actions{}, Sources{Roster: roster})
	for _, method := range []string{"PUT", "DELETE"} {
		w := request(bare, method, path, Addr, "http://"+Addr, "application/json", `{"body":"x","base_hash":"`+sys.Hash+`"}`)
		if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
			t.Errorf("%s on a read-only dashboard: %d", method, w.Code)
		}
	}
	noOverride("a read-only dashboard")
}
