// Package config decodes the embedded agent roster and resolves the two path
// roots. Everything here is settled once, at startup, and passed down: no
// package-level globals, and no second `git rev-parse` three calls later.
package config

import (
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Agent is one row of the roster, and also the [defaults] block. An empty
// field means "inherit"; there is no way to override a default back to empty,
// which is the price of not carrying a pointer for every key.
type Agent struct {
	Name     string   `toml:"name"`
	Provider string   `toml:"provider"`
	Model    string   `toml:"model"`
	Thinking string   `toml:"thinking"`
	Timeout  string   `toml:"timeout"`
	Tools    []string `toml:"tools"`
	System   string   `toml:"system"` // key into the embedded FS, not a filesystem path
	User     string   `toml:"user"`
}

// Config is the decoded roster plus the filesystem the prompts came from.
type Config struct {
	// Protected is the deny list every agent is held to, in internal/permit's
	// two forms. It is one list in one file on purpose: the Go gates and the
	// guard extension have to be holding the same one.
	Protected []string `toml:"protected"`
	// BashDenied is the same idea for the one agent that gets a shell: Go
	// regexps, checked by the guard before a command runs. It is coarse and a
	// determined model gets past it — `python -c` alone does — so it is the
	// careless case it catches, and it grows by incident.
	BashDenied []string `toml:"bash_denied"`
	Defaults   Agent    `toml:"defaults"`
	Agents     []Agent  `toml:"agents"`

	fsys fs.FS
}

// Overrides are the per-run flags. Empty fields change nothing.
type Overrides struct {
	Provider string
	Model    string
	Thinking string
}

// Resolved is one agent with defaults, roster keys and flags already applied
// and its prompts already read. Nothing downstream touches the FS again.
// Agent is embedded rather than copied field by field, so a new roster key
// only has to be added in one place.
type Resolved struct {
	Agent

	// Deadline is Agent.Timeout parsed, so a bad duration fails at startup
	// rather than mid-run. SystemPrompt and UserPrompt are the bodies at
	// Agent.System and Agent.User, which are keys into the FS, not text.
	Deadline     time.Duration
	SystemPrompt string
	UserPrompt   string
}

// Load decodes lathe.toml out of fsys. fsys is the embedded assets in
// production and a fstest.MapFS in tests.
func Load(fsys fs.FS) (Config, error) {
	b, err := fs.ReadFile(fsys, "lathe.toml")
	if err != nil {
		return Config{}, err
	}
	var c Config
	if _, err := toml.Decode(string(b), &c); err != nil {
		return Config{}, fmt.Errorf("lathe.toml: %w", err)
	}
	c.fsys = fsys
	return c, nil
}

// Resolve returns the named agent, or an error naming every agent that does
// exist. Unknown names fail here, before anything is spawned.
func (c Config) Resolve(name string, ov Overrides) (Resolved, error) {
	var a Agent
	var found bool
	for _, candidate := range c.Agents {
		if candidate.Name == name {
			a, found = candidate, true
			break
		}
	}
	if !found {
		names := make([]string, len(c.Agents))
		for i, candidate := range c.Agents {
			names[i] = candidate.Name
		}
		return Resolved{}, fmt.Errorf("unknown agent %q (have: %s)", name, strings.Join(names, ", "))
	}

	r := Resolved{Agent: a}
	r.Provider = pick(ov.Provider, a.Provider, c.Defaults.Provider)
	r.Model = pick(ov.Model, a.Model, c.Defaults.Model)
	r.Thinking = pick(ov.Thinking, a.Thinking, c.Defaults.Thinking)
	if len(r.Tools) == 0 {
		r.Tools = c.Defaults.Tools
	}

	var err error
	if d := pick(a.Timeout, c.Defaults.Timeout); d != "" {
		if r.Deadline, err = time.ParseDuration(d); err != nil {
			return Resolved{}, fmt.Errorf("agent %s: timeout: %w", name, err)
		}
	}
	if r.SystemPrompt, err = c.readPrompt(a.System); err != nil {
		return Resolved{}, fmt.Errorf("agent %s: %w", name, err)
	}
	if r.UserPrompt, err = c.readPrompt(a.User); err != nil {
		return Resolved{}, fmt.Errorf("agent %s: %w", name, err)
	}
	return r, nil
}

// readPrompt reads one prompt out of the embedded FS. An empty key is an agent
// that does not set that prompt, which is not an error.
func (c Config) readPrompt(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	b, err := fs.ReadFile(c.fsys, key)
	return string(b), err
}

// Prompt substitutes the request into the agent's user prompt. One placeholder
// is the whole templating story; a second one is the trigger to reach for
// text/template.
func (r Resolved) Prompt(request string) string {
	if r.UserPrompt == "" {
		return request
	}
	return strings.ReplaceAll(r.UserPrompt, "{{request}}", request)
}

// TargetRoot is the repo lathe acts on: discovered from where it was invoked,
// never configured. A directory outside a git repo is an error rather than a
// silent fallback to cwd, because the fallback would point an agent at $HOME.
func TargetRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", dir)
	}
	return filepath.Abs(strings.TrimSpace(string(out)))
}

func pick(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
