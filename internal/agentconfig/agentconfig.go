// Package agentconfig seeds, loads and edits the engineer's agent
// configuration under ~/.lathe: agents/<name>/{config.yaml,system.md,user.md},
// routing/<agent>.md and the seed manifest .seed.yaml. Those files are the only
// source of agent configuration; nothing falls back to the embedded seed.
package agentconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"gopkg.in/yaml.v3"
)

// ManifestName is the seed manifest's path relative to the home.
const ManifestName = ".seed.yaml"

// Home returns ~/.lathe.
func Home() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".lathe"), nil
}

// LockName is the lock file, relative to the home, that every lathe process
// holds while it writes the home.
const LockName = ".lock"

// lock takes an exclusive lock on home shared by every process, creating home
// owner-only, and returns its release. It blocks until the lock is free.
func lock(home string) (func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, LockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", home, err)
	}
	// Closing the file releases the flock.
	return func() { f.Close() }, nil
}

// invalid is a validation error naming the file it is about.
type invalid struct{ path, msg string }

func (e *invalid) Error() string { return e.path + ": " + e.msg }
func (e *invalid) Unwrap() error { return config.ErrInvalid }

func invalidf(path, format string, args ...any) error {
	return &invalid{path: path, msg: fmt.Sprintf(format, args...)}
}

// entry is one seeded file's record: the hash of the seed lathe last wrote or
// matched, and the version of lathe that recorded it.
type entry struct {
	Hash    string `yaml:"hash"`
	Version string `yaml:"version"`
}

type manifest struct {
	Files map[string]entry `yaml:"files"`
}

// readManifest returns home's manifest; a missing one is empty.
func readManifest(home string) (manifest, error) {
	path := filepath.Join(home, ManifestName)
	m := manifest{Files: map[string]entry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := decodeStrict(b, &m); err != nil && !errors.Is(err, io.EOF) {
		return m, invalidf(path, "%v", err)
	}
	if m.Files == nil {
		m.Files = map[string]entry{}
	}
	return m, nil
}

// writeManifest replaces home's manifest atomically.
func writeManifest(home string, m manifest) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return config.WriteAtomic(filepath.Join(home, ManifestName), string(b))
}

// seedFiles returns every file in fsys, in fs.WalkDir's lexical order.
func seedFiles(fsys fs.FS) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// Report is what Seed did: files written because they were missing, files
// replaced because they were untouched and their seed changed, and edited
// files whose seed changed since, which it left alone.
type Report struct {
	Written  []string
	Replaced []string
	Drift    []string
}

// parseVersion returns the numeric parts of a release version such as v1.2.3,
// or false for anything else, dev included. A pre-release or build suffix
// makes a version unordered too.
func parseVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if v == "" || len(parts) > 3 {
		return nil, false
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || strconv.Itoa(n) != p {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether version is a release later than recorded. Either
// being unordered, such as dev, is not newer.
func Newer(version, recorded string) bool {
	a, ok := parseVersion(version)
	b, ok2 := parseVersion(recorded)
	if !ok || !ok2 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// Seed writes every file in fsys missing from home, replaces each file that
// still matches its recorded seed when fsys carries a different seed and
// version is newer than the recorded one, and leaves every other file alone.
// It records each seed it writes or finds in place in the manifest, with
// version. It holds the home's lock throughout. Paths in the report are
// relative to home.
func Seed(home string, fsys fs.FS, version string) (Report, error) {
	var r Report
	unlock, err := lock(home)
	if err != nil {
		return r, err
	}
	defer unlock()
	m, err := readManifest(home)
	if err != nil {
		return r, err
	}
	files, err := seedFiles(fsys)
	if err != nil {
		return r, err
	}
	changed := false
	for _, rel := range files {
		b, err := fs.ReadFile(fsys, rel)
		if err != nil {
			return r, err
		}
		seed, seedHash := string(b), config.Hash(string(b))
		path := filepath.Join(home, filepath.FromSlash(rel))
		cur, err := os.ReadFile(path)
		rec, recorded := m.Files[rel]
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := config.WriteAtomic(path, seed); err != nil {
				return r, err
			}
			m.Files[rel], changed = entry{seedHash, version}, true
			r.Written = append(r.Written, rel)
		case err != nil:
			return r, err
		default:
			curHash := config.Hash(string(cur))
			switch {
			case curHash == seedHash:
				if !recorded || rec.Hash != seedHash {
					m.Files[rel], changed = entry{seedHash, version}, true
				}
			case recorded && curHash == rec.Hash:
				if !Newer(version, rec.Version) {
					break
				}
				// An editor that does not take the lock may have written since
				// the first read; an edit is never replaced.
				if again, err := os.ReadFile(path); err != nil || config.Hash(string(again)) != rec.Hash {
					if err != nil && !errors.Is(err, fs.ErrNotExist) {
						return r, err
					}
					r.Drift = append(r.Drift, rel)
					break
				}
				if err := config.WriteAtomic(path, seed); err != nil {
					return r, err
				}
				m.Files[rel], changed = entry{seedHash, version}, true
				r.Replaced = append(r.Replaced, rel)
			case !recorded || seedHash != rec.Hash:
				r.Drift = append(r.Drift, rel)
			}
		}
	}
	if changed {
		if err := writeManifest(home, m); err != nil {
			return r, err
		}
	}
	return r, nil
}

type fileConfig struct {
	Provider         string              `yaml:"provider"`
	Model            string              `yaml:"model"`
	Thinking         string              `yaml:"thinking"`
	Timeout          string              `yaml:"timeout"`
	RepoInstructions *bool               `yaml:"repo_instructions"`
	Tiers            []config.TierConfig `yaml:"tiers"`
}

// decodeStrict decodes the single YAML document in b into v, rejecting
// unknown keys and any document after the first. An empty b returns io.EOF.
func decodeStrict(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("more than one YAML document")
	}
	return nil
}

// parseConfig decodes and validates one agent's config.yaml; path names it in
// errors.
func parseConfig(name, path string, b []byte) (fileConfig, error) {
	var fc fileConfig
	if err := decodeStrict(b, &fc); err != nil {
		if errors.Is(err, io.EOF) {
			return fc, invalidf(path, "empty")
		}
		return fc, invalidf(path, "%v", err)
	}
	for _, f := range []struct{ key, val string }{
		{"provider", fc.Provider}, {"model", fc.Model}, {"thinking", fc.Thinking}, {"timeout", fc.Timeout},
	} {
		if strings.TrimSpace(f.val) == "" {
			return fc, invalidf(path, "%s is required", f.key)
		}
	}
	if !config.ValidThinking(fc.Thinking) {
		return fc, invalidf(path, "thinking %q is not one of %s", fc.Thinking, strings.Join(config.Thinking, ", "))
	}
	if d, err := time.ParseDuration(fc.Timeout); err != nil || d <= 0 {
		return fc, invalidf(path, "timeout %q is not a positive duration", fc.Timeout)
	}
	if err := config.CheckTiers(name, fc.Tiers); err != nil {
		return fc, invalidf(path, "tiers: %v", err)
	}
	return fc, nil
}

// readFile reads rel under home and refuses a missing one.
func readFile(home, rel string) (config.File, error) {
	path := filepath.Join(home, filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config.File{}, invalidf(path, "missing")
	}
	if err != nil {
		return config.File{}, fmt.Errorf("%s: %w", path, err)
	}
	return config.File{Path: path, Body: string(b)}, nil
}

// readText reads a prompt file under home and refuses a missing or blank one.
func readText(home, rel string) (config.File, error) {
	f, err := readFile(home, rel)
	if err != nil {
		return f, err
	}
	if strings.TrimSpace(f.Body) == "" {
		return config.File{}, invalidf(f.Path, "empty")
	}
	return f, nil
}

// Load reads every known agent's files and the routing instructions from home
// and merges them with the compiled permissions. A missing or invalid file is
// an error naming it. Directories for agents the binary does not know are
// ignored.
func Load(home string) (config.Config, error) {
	files := map[string]config.File{}
	var agents []config.Agent
	allTiers := map[string][]config.TierConfig{}
	for _, p := range config.Permissions {
		key := config.AgentFile(p.Name, "config.yaml")
		f, err := readFile(home, key)
		if err != nil {
			return config.Config{}, err
		}
		fc, err := parseConfig(p.Name, f.Path, []byte(f.Body))
		if err != nil {
			return config.Config{}, err
		}
		files[key] = f
		for _, role := range []string{"system.md", "user.md"} {
			k := config.AgentFile(p.Name, role)
			f, err := readText(home, k)
			if err != nil {
				return config.Config{}, err
			}
			if role == "user.md" && !strings.Contains(f.Body, "{{request}}") {
				return config.Config{}, invalidf(f.Path, "a user prompt has to contain {{request}}")
			}
			files[k] = f
		}
		agents = append(agents, config.Agent{Name: p.Name, Provider: fc.Provider, Model: fc.Model,
			Thinking: fc.Thinking, Timeout: fc.Timeout, RepoInstructions: fc.RepoInstructions})
		allTiers[p.Name] = fc.Tiers
	}
	for _, r := range config.Routes {
		f, err := readText(home, r.Key)
		if err != nil {
			return config.Config{}, err
		}
		files[r.Key] = f
	}
	return config.New(agents, allTiers, files)
}

// Open seeds home from fsys, loads it, and marks each loaded file with its
// seed and whether it is edited or has drifted.
func Open(home string, fsys fs.FS, version string) (config.Config, error) {
	if _, err := Seed(home, fsys, version); err != nil {
		return config.Config{}, err
	}
	c, err := Load(home)
	if err != nil {
		return config.Config{}, err
	}
	m, err := readManifest(home)
	if err != nil {
		return config.Config{}, err
	}
	for key, f := range c.Files {
		b, err := fs.ReadFile(fsys, key)
		if err != nil {
			continue
		}
		f.Seed = string(b)
		cur, seed := config.Hash(f.Body), config.Hash(f.Seed)
		f.Edited = cur != seed
		rec, ok := m.Files[key]
		f.Drift = f.Edited && (!ok || (cur != rec.Hash && seed != rec.Hash))
		c.Files[key] = f
	}
	return c, nil
}

// filePath returns key's absolute path under home, or an error when key is not
// a valid relative path.
func filePath(home, key string) (string, error) {
	if !fs.ValidPath(key) || key == "." {
		return "", fmt.Errorf("%q is not a configuration file", key)
	}
	return filepath.Join(home, filepath.FromSlash(key)), nil
}

// checkBase reads path and returns its body, or an error wrapping
// config.ErrConflict when baseHash is not its hash.
func checkBase(path, baseHash string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if config.Hash(string(b)) != baseHash {
		return "", fmt.Errorf("%s: %w", path, config.ErrConflict)
	}
	return string(b), nil
}

// editLocked holds home's lock while it checks that home/key's hash is
// baseHash and calls fn with the file's path and current body. It returns
// fn's error, or an error wrapping config.ErrConflict without calling fn.
func editLocked(home, key, baseHash string, fn func(path, cur string) error) error {
	path, err := filePath(home, key)
	if err != nil {
		return err
	}
	unlock, err := lock(home)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := checkBase(path, baseHash)
	if err != nil {
		return err
	}
	return fn(path, cur)
}

// SaveFile writes body to home/key atomically under the home's lock. It
// writes nothing and returns an error wrapping config.ErrConflict when
// baseHash is not the current file's hash.
func SaveFile(home, key, body, baseHash string) error {
	return editLocked(home, key, baseHash, func(path, _ string) error {
		return config.WriteAtomic(path, body)
	})
}

// ResetFile restores home/key to its seed in fsys and records that seed in
// the manifest with version, under the home's lock. It writes nothing and
// returns an error wrapping config.ErrConflict when baseHash is not the
// current file's hash.
func ResetFile(home string, fsys fs.FS, version, key, baseHash string) error {
	return editLocked(home, key, baseHash, func(path, _ string) error {
		b, err := fs.ReadFile(fsys, key)
		if err != nil {
			return fmt.Errorf("%s has no seed: %w", key, err)
		}
		m, err := readManifest(home)
		if err != nil {
			return err
		}
		if err := config.WriteAtomic(path, string(b)); err != nil {
			return err
		}
		m.Files[key] = entry{config.Hash(string(b)), version}
		return writeManifest(home, m)
	})
}

// Settings are the config.yaml fields the dashboard edits.
type Settings struct {
	Provider string
	Model    string
	Thinking string
}

// SaveSettings sets provider, model and thinking in agent's config.yaml,
// editing the YAML node tree so comments and other keys survive. It writes
// nothing and returns an error wrapping config.ErrConflict when baseHash is
// not the current file's hash, or wrapping config.ErrInvalid when the result
// does not validate. It holds the home's lock from the check to the write.
func SaveSettings(home, agent string, s Settings, baseHash string) error {
	return editLocked(home, config.AgentFile(agent, "config.yaml"), baseHash, func(path, cur string) error {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(cur), &doc); err != nil {
			return invalidf(path, "%v", err)
		}
		if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
			return invalidf(path, "not a mapping")
		}
		m := doc.Content[0]
		setScalar(m, "provider", s.Provider)
		setScalar(m, "model", s.Model)
		setScalar(m, "thinking", s.Thinking)
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		if _, err := parseConfig(agent, path, buf.Bytes()); err != nil {
			return err
		}
		return config.WriteAtomic(path, buf.String())
	})
}

// setScalar sets key's value in mapping m to the string value, keeping the
// key's comments, or appends the pair when key is absent.
func setScalar(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			v.Kind, v.Tag, v.Value, v.Style, v.Content = yaml.ScalarNode, "!!str", value, 0, nil
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}
