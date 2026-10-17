// Package config holds the agent roster as loaded from the engineer's agent
// configuration, merged with the compiled permissions, and resolves it into
// the snapshot a run executes from. It reads no files itself: internal/agentconfig
// loads them and hands the result to New.
package config

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyrelh/lathe/internal/jev"
)

// Agent is one agent's configuration and tools. System and User are the paths
// of its prompts relative to the configuration home, keys into Config.Files.
type Agent struct {
	Name     string
	Provider string
	Model    string
	Thinking string
	Timeout  string
	Tools    []string
	System   string
	User     string
	// RepoInstructions defaults to on, so nil means yes.
	RepoInstructions *bool
}

// WantsRepoInstructions reports whether the agent's first turn carries the
// target repo's AGENTS.md; unset means yes.
func (a Agent) WantsRepoInstructions() bool {
	return a.RepoInstructions == nil || *a.RepoInstructions
}

// TierConfig is one configured tier. Empty fields inherit the agent's own
// resolved configuration.
type TierConfig struct {
	When     string
	Default  bool
	Provider string
	Model    string
	Thinking string
}

// File is one configuration file as loaded. Seed is the binary's seed for it,
// "" when it has none. Edited is a file that differs from its seed; Drift is an
// edited file whose seed changed since it was last written.
type File struct {
	Path   string
	Body   string
	Seed   string
	Edited bool
	Drift  bool
}

// Config is the loaded roster: every agent the binary knows, in roster order,
// with its tools, tiers and the text of every configuration file.
type Config struct {
	Agents []Agent
	// Tiers is each routed agent's tiers, cheapest first.
	Tiers map[string][]TierConfig
	// Files is every configuration file keyed by its slash-separated path
	// relative to the configuration home.
	Files map[string]File
}

// AgentFile returns the home-relative path of one of agent's files.
func AgentFile(agent, file string) string {
	return "agents/" + agent + "/" + file
}

// New merges loaded agent configuration with the compiled permissions. It
// returns the agents in Permissions order and errors when one is missing.
// Agents the binary does not know are dropped.
func New(agents []Agent, tiers map[string][]TierConfig, files map[string]File) (Config, error) {
	c := Config{
		Tiers: map[string][]TierConfig{},
		Files: files,
	}
	for _, p := range Permissions {
		i := slices.IndexFunc(agents, func(a Agent) bool { return a.Name == p.Name })
		if i < 0 {
			return Config{}, fmt.Errorf("agent %s: no configuration", p.Name)
		}
		a := agents[i]
		a.Tools = append([]string{}, p.Tools...)
		a.System, a.User = AgentFile(p.Name, "system.md"), AgentFile(p.Name, "user.md")
		c.Agents = append(c.Agents, a)
		if t := tiers[p.Name]; len(t) > 0 {
			if err := CheckTiers(p.Name, t); err != nil {
				return Config{}, fmt.Errorf("agent %s: %w", p.Name, err)
			}
			c.Tiers[p.Name] = t
		}
	}
	return c, nil
}

// CheckTiers returns why an agent's tiers are invalid, or nil. No tiers is valid.
func CheckTiers(name string, tiers []TierConfig) error {
	if len(tiers) == 0 {
		return nil
	}
	if RoutingKey(name) == "" {
		return errors.New("tiers are only allowed on planner and builder")
	}
	if len(tiers) < jev.MinLevels || len(tiers) > jev.MaxLevels {
		return fmt.Errorf("%d tiers; routing needs %d to %d", len(tiers), jev.MinLevels, jev.MaxLevels)
	}
	defaults := 0
	for i, t := range tiers {
		if strings.TrimSpace(t.When) == "" {
			return fmt.Errorf("tier %d has no when", i+1)
		}
		if t.Thinking != "" && !ValidThinking(t.Thinking) {
			return fmt.Errorf("tier %d: thinking %q is not one of %s", i+1, t.Thinking, strings.Join(Thinking, ", "))
		}
		if t.Default {
			defaults++
		}
	}
	if defaults != 1 {
		return fmt.Errorf("%d tiers set default: true; exactly one must", defaults)
	}
	return nil
}

// Overrides are the per-run flags. Empty fields change nothing.
type Overrides struct {
	Provider string
	Model    string
	Thinking string
}

// defaultFloor is the routing confidence floor.
const defaultFloor = 0.5

// Resolved is one agent with flags already applied and its prompts already
// read. Nothing downstream touches the filesystem again.
type Resolved struct {
	Agent

	// Deadline is Agent.Timeout parsed. SystemPrompt and UserPrompt are the
	// bodies at Agent.System and Agent.User.
	Deadline     time.Duration
	SystemPrompt string
	UserPrompt   string
}

// Resolve returns the named agent with ov applied over its configuration, or
// an error naming every agent that does exist.
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
	r.Provider = pick(ov.Provider, a.Provider)
	r.Model = pick(ov.Model, a.Model)
	r.Thinking = pick(ov.Thinking, a.Thinking)
	var err error
	if a.Timeout != "" {
		if r.Deadline, err = time.ParseDuration(a.Timeout); err != nil {
			return Resolved{}, fmt.Errorf("agent %s: timeout: %w", name, err)
		}
	}
	if r.SystemPrompt, err = c.body(a.System); err != nil {
		return Resolved{}, fmt.Errorf("agent %s: %w", name, err)
	}
	if r.UserPrompt, err = c.body(a.User); err != nil {
		return Resolved{}, fmt.Errorf("agent %s: %w", name, err)
	}
	return r, nil
}

// body returns the loaded text of key. An empty key is no prompt.
func (c Config) body(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	f, ok := c.Files[key]
	if !ok {
		return "", fmt.Errorf("%s was not loaded", key)
	}
	return f.Body, nil
}

// AgentPrompts is one agent or routing phase and the files it runs with.
// Routes is the agent a routing phase routes. Config is an agent's
// config.yaml; a routing phase has none.
type AgentPrompts struct {
	Name             string    `json:"name"`
	Kind             string    `json:"kind"`
	Routes           string    `json:"routes,omitempty"`
	RepoInstructions bool      `json:"repo_instructions"`
	Config           *Settings `json:"config,omitempty"`
	Prompts          []Prompt  `json:"prompts"`
}

// FileView is one configuration file as the dashboard shows it. Body, Seed and
// Hash are only filled when bodies are asked for.
type FileView struct {
	Key    string `json:"key"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
	Seed   string `json:"seed,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Edited bool   `json:"edited"`
	Drift  bool   `json:"drift"`
}

// Prompt is one prompt by role: system, user or instructions.
type Prompt struct {
	Role string `json:"role"`
	FileView
}

// Settings is an agent's provider, model, thinking and timeout, and the
// config.yaml they come from.
type Settings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
	Timeout  string `json:"timeout"`
	FileView
}

// Prompts returns every agent in roster order, then every routing phase, with
// each file's drift state. Bodies, seeds and hashes are filled only when bodies
// is set. A file that was not loaded is an error naming the agent or phase.
func (c Config) Prompts(bodies bool) ([]AgentPrompts, error) {
	out := make([]AgentPrompts, 0, len(c.Agents)+len(Routes))
	for _, a := range c.Agents {
		cfg, err := c.view(AgentFile(a.Name, "config.yaml"), bodies)
		if err != nil {
			return nil, fmt.Errorf("agent %s: %w", a.Name, err)
		}
		ap := AgentPrompts{Name: a.Name, Kind: "agent", RepoInstructions: a.WantsRepoInstructions(), Prompts: []Prompt{},
			Config: &Settings{Provider: a.Provider, Model: a.Model, Thinking: a.Thinking, Timeout: a.Timeout, FileView: cfg}}
		for _, p := range []struct{ role, key string }{{"system", a.System}, {"user", a.User}} {
			if p.key == "" {
				continue
			}
			v, err := c.view(p.key, bodies)
			if err != nil {
				return nil, fmt.Errorf("agent %s: %w", a.Name, err)
			}
			ap.Prompts = append(ap.Prompts, Prompt{Role: p.role, FileView: v})
		}
		out = append(out, ap)
	}
	for _, r := range Routes {
		v, err := c.view(r.Key, bodies)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Phase, err)
		}
		out = append(out, AgentPrompts{Name: r.Phase, Kind: "routing", Routes: r.Agent, Prompts: []Prompt{{Role: "instructions", FileView: v}}})
	}
	return out, nil
}

// view returns key's file as the dashboard shows it.
func (c Config) view(key string, bodies bool) (FileView, error) {
	f, ok := c.Files[key]
	if !ok {
		return FileView{}, fmt.Errorf("%s was not loaded", key)
	}
	v := FileView{Key: key, Path: f.Path, Edited: f.Edited, Drift: f.Drift}
	if bodies {
		v.Body, v.Seed, v.Hash = f.Body, f.Seed, Hash(f.Body)
	}
	return v, nil
}

// Prompt substitutes the request into the agent's user prompt.
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

// Snapshot is the effective roster captured at submission: every agent the
// workflow will spawn, already resolved against its configuration and the
// run's flags. It is what a worker executes from, so editing a file cannot
// change work that is already queued.
type Snapshot struct {
	Version    int      `json:"version"`
	Protected  []string `json:"protected"`
	BashDenied []string `json:"bash_denied"`
	// CommandDeadline is CommandTimeout.
	CommandDeadline time.Duration       `json:"command_deadline"`
	Agents          map[string]Resolved `json:"agents"`
	// Routing is every routed agent's question and tiers. ConfidenceFloor
	// applies to all of them. RoutingOff is a run whose flags turned routing
	// off, leaving Routing empty.
	Routing         map[string]Routing `json:"routing,omitempty"`
	ConfidenceFloor float64            `json:"confidence_floor,omitempty"`
	RoutingOff      bool               `json:"routing_off,omitempty"`
}

// Routing is one routed agent as captured: the Score instructions its routing
// phase asks and its tiers, cheapest first.
type Routing struct {
	Instructions string `json:"instructions"`
	Tiers        []Tier `json:"tiers"`
}

// Tier is one resolved tier.
type Tier struct {
	When     string   `json:"when"`
	Default  bool     `json:"default,omitempty"`
	Resolved Resolved `json:"resolved"`
}

// Criteria is each tier's when, in order: one Score level per tier.
func (r Routing) Criteria() []string {
	c := make([]string, len(r.Tiers))
	for i, t := range r.Tiers {
		c[i] = t.When
	}
	return c
}

// Default is the index of the default tier.
func (r Routing) Default() int {
	for i, t := range r.Tiers {
		if t.Default {
			return i
		}
	}
	return 0
}

// SnapshotVersion is stamped into every captured specification. A worker that
// does not recognise it refuses the run rather than guessing.
const SnapshotVersion = 3

// Capture resolves every named agent, and every tier of a routed one, now and
// freezes the result. A provider, model or thinking flag in ov turns routing
// off for the run.
func (c Config) Capture(agents []string, ov Overrides) (Snapshot, error) {
	s := Snapshot{
		Version:         SnapshotVersion,
		Protected:       Protected,
		BashDenied:      BashDenied,
		CommandDeadline: CommandTimeout,
		Agents:          map[string]Resolved{},
	}
	for _, name := range agents {
		r, err := c.Resolve(name, ov)
		if err != nil {
			return Snapshot{}, err
		}
		s.Agents[name] = r

		tiers := c.Tiers[name]
		if len(tiers) == 0 {
			continue
		}
		if ov.Provider != "" || ov.Model != "" || ov.Thinking != "" {
			s.RoutingOff = true
			continue
		}
		rt := Routing{}
		if rt.Instructions, err = c.body(RoutingKey(name)); err != nil {
			return Snapshot{}, fmt.Errorf("agent %s: routing: %w", name, err)
		}
		for i, t := range tiers {
			tr, err := c.Resolve(name, Overrides{Provider: t.Provider, Model: t.Model, Thinking: t.Thinking})
			if err != nil {
				return Snapshot{}, fmt.Errorf("agent %s: tier %d: %w", name, i+1, err)
			}
			rt.Tiers = append(rt.Tiers, Tier{When: t.When, Default: t.Default, Resolved: tr})
		}
		if s.Routing == nil {
			s.Routing = map[string]Routing{}
			s.ConfidenceFloor = defaultFloor
		}
		s.Routing[name] = rt
	}
	return s, nil
}

// Resolve returns a captured agent. Unlike Config.Resolve it reads no files
// and applies no overrides: both happened at submission.
func (s Snapshot) Resolve(name string) (Resolved, error) {
	r, ok := s.Agents[name]
	if !ok {
		return Resolved{}, fmt.Errorf("agent %q is not in this run's captured roster", name)
	}
	return r, nil
}
