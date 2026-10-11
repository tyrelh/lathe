package dashboard

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/tyrelh/lathe/internal/config"
)

// findPrompt returns the named agent's prompt for role, without its body, and
// reports false once it has answered.
func findPrompt(w http.ResponseWriter, roster func() (config.Config, error), name, role string) (config.Prompt, bool) {
	a, err := findAgent(roster, name, false)
	if errors.Is(err, errNoAgent) {
		submitError(w, http.StatusNotFound, err.Error(), "")
		return config.Prompt{}, false
	}
	if err != nil {
		submitError(w, http.StatusInternalServerError, err.Error(), "")
		return config.Prompt{}, false
	}
	j := slices.IndexFunc(a.Prompts, func(p config.Prompt) bool { return p.Role == role })
	if j < 0 {
		submitError(w, http.StatusNotFound, name+" has no "+role+" prompt", "")
		return config.Prompt{}, false
	}
	return a.Prompts[j], true
}

// writtenAgent answers a prompt write with the agent as GET /api/agents/{name}
// serves it after the write.
func writtenAgent(w http.ResponseWriter, roster func() (config.Config, error), name string) {
	a, err := findAgent(roster, name, true)
	if err != nil {
		submitError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, a, nil)
}

// savePrompt writes an agent's prompt as an override of the built-in. The key
// comes from the roster, never the request. A stale base hash is a conflict
// that answers with the current body and hash and writes nothing.
func savePrompt(mu *sync.Mutex, roster func() (config.Config, error), save func(key, body, baseHash string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Body     string `json:"body"`
			BaseHash string `json:"base_hash"`
		}
		if !browserJSON(w, r, &body) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		name, role := r.PathValue("name"), r.PathValue("role")
		p, ok := findPrompt(w, roster, name, role)
		if !ok {
			return
		}
		switch {
		case strings.TrimSpace(body.Body) == "":
			submitError(w, http.StatusBadRequest, "a prompt can't be empty; reset it to go back to the built-in", "")
			return
		case body.BaseHash == "":
			submitError(w, http.StatusBadRequest, "base_hash is required", "")
			return
		case role == "user" && !strings.Contains(body.Body, "{{request}}"):
			submitError(w, http.StatusBadRequest, "a user prompt has to contain {{request}}", "")
			return
		}
		err := save(p.Key, body.Body, body.BaseHash)
		if errors.Is(err, config.ErrPromptConflict) {
			a, err := findAgent(roster, name, true)
			if err != nil {
				submitError(w, http.StatusInternalServerError, err.Error(), "")
				return
			}
			current := a.Prompts[slices.IndexFunc(a.Prompts, func(c config.Prompt) bool { return c.Role == role })]
			writeStatus(w, http.StatusConflict, map[string]any{
				"error": "this prompt changed since you loaded it", "body": current.Body, "hash": current.Hash,
				"overridden": current.Overridden,
			})
			return
		}
		if err != nil {
			submitError(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		writtenAgent(w, roster, name)
	}
}

// resetPrompt removes an agent's prompt override, so the built-in applies.
func resetPrompt(mu *sync.Mutex, roster func() (config.Config, error), reset func(key string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !browserJSON(w, r, &struct{}{}) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		name := r.PathValue("name")
		p, ok := findPrompt(w, roster, name, r.PathValue("role"))
		if !ok {
			return
		}
		if err := reset(p.Key); err != nil {
			submitError(w, http.StatusInternalServerError, err.Error(), "")
			return
		}
		writtenAgent(w, roster, name)
	}
}
