package dashboard

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/tyrelh/lathe/internal/config"
)

// findPrompt returns the named agent's prompt for role, without its body, and
// reports false once it has answered.
func findPrompt(w http.ResponseWriter, roster Roster, name, role string) (config.Prompt, bool) {
	a, err := findAgent(roster, name, false)
	if errors.Is(err, errNoAgent) {
		writeError(w, http.StatusNotFound, err.Error())
		return config.Prompt{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return config.Prompt{}, false
	}
	p, ok := a.prompt(role)
	if !ok {
		writeError(w, http.StatusNotFound, name+" has no "+role+" prompt")
		return config.Prompt{}, false
	}
	return p, true
}

// writtenAgent answers a write with the agent as GET /api/agents/{name}
// serves it after the write.
func writtenAgent(w http.ResponseWriter, roster Roster, name string) {
	a, err := findAgent(roster, name, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, a, nil)
}

// savePrompt writes an agent's prompt file. The key comes from the roster,
// never the request. A stale base hash is a conflict that answers with the
// current body and hash and writes nothing.
func savePrompt(mu *sync.Mutex, roster Roster, save SavePrompt) http.HandlerFunc {
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
			writeError(w, http.StatusBadRequest, "a prompt can't be empty; reset it to go back to the seed")
			return
		case body.BaseHash == "":
			writeError(w, http.StatusBadRequest, "base_hash is required")
			return
		case role == "user" && !strings.Contains(body.Body, config.RequestPlaceholder):
			writeError(w, http.StatusBadRequest, "a user prompt has to contain "+config.RequestPlaceholder)
			return
		}
		err := save(p.Key, body.Body, body.BaseHash)
		if errors.Is(err, config.ErrConflict) {
			promptConflict(w, roster, name, role)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writtenAgent(w, roster, name)
	}
}

// resetPrompt restores an agent's prompt file to its seed. A stale base hash
// is a conflict that answers with the current body and hash and writes
// nothing.
func resetPrompt(mu *sync.Mutex, roster Roster, reset ResetPrompt) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
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
		if body.BaseHash == "" {
			writeError(w, http.StatusBadRequest, "base_hash is required")
			return
		}
		err := reset(p.Key, body.BaseHash)
		if errors.Is(err, config.ErrConflict) {
			promptConflict(w, roster, name, role)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writtenAgent(w, roster, name)
	}
}

// promptConflict answers a stale prompt write with the prompt's current body,
// hash and edited state.
func promptConflict(w http.ResponseWriter, roster Roster, name, role string) {
	a, err := findAgent(roster, name, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	current, ok := a.prompt(role)
	if !ok {
		writeError(w, http.StatusInternalServerError, name+" has no "+role+" prompt")
		return
	}
	writeStatus(w, http.StatusConflict, map[string]any{
		"error": "this prompt changed since you loaded it", "body": current.Body, "hash": current.Hash,
		"edited": current.Edited,
	})
}

// configConflict answers a stale configuration write with the agent's current
// configuration.
func configConflict(w http.ResponseWriter, roster Roster, name string) {
	a, err := findAgent(roster, name, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeStatus(w, http.StatusConflict, map[string]any{
		"error": "this configuration changed since you loaded it", "config": a.Config,
	})
}

// saveConfig sets an agent's provider, model and thinking in its config.yaml.
// A stale base hash is a conflict that answers with the current file and
// writes nothing; a value that does not validate is refused.
func saveConfig(mu *sync.Mutex, roster Roster, save SaveConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Thinking string `json:"thinking"`
			BaseHash string `json:"base_hash"`
		}
		if !browserJSON(w, r, &body) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		name := r.PathValue("name")
		a, err := findAgent(roster, name, false)
		if errors.Is(err, errNoAgent) || (err == nil && a.Config == nil) {
			writeError(w, http.StatusNotFound, "no such agent")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		switch {
		case strings.TrimSpace(body.Provider) == "" || strings.TrimSpace(body.Model) == "":
			writeError(w, http.StatusBadRequest, "provider and model are required")
			return
		case !config.ValidThinking(body.Thinking):
			writeError(w, http.StatusBadRequest, "thinking must be one of "+strings.Join(config.Thinking, ", "))
			return
		case body.BaseHash == "":
			writeError(w, http.StatusBadRequest, "base_hash is required")
			return
		}
		err = save(name, strings.TrimSpace(body.Provider), strings.TrimSpace(body.Model), body.Thinking, body.BaseHash)
		if errors.Is(err, config.ErrConflict) {
			configConflict(w, roster, name)
			return
		}
		if errors.Is(err, config.ErrInvalid) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writtenAgent(w, roster, name)
	}
}
