package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// overlay is the FS Overlay returns.
type overlay struct {
	dir  string
	base fs.FS
}

// Overlay returns base with each file under prompts/ served from dir/<name>
// when that file exists. Everything else, roster.toml included, comes from base.
func Overlay(dir string, base fs.FS) fs.FS {
	return overlay{dir: dir, base: base}
}

// Open returns dir/<name> for a regular file under prompts/, and base's file
// when there is none, a file stands where one of its directories would be, or
// it is a directory. Any other error opening the override is returned.
func (o overlay) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if strings.HasPrefix(name, "prompts/") {
		path := filepath.Join(o.dir, filepath.FromSlash(name))
		st, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		case err != nil:
			return nil, err
		case st.Mode().IsRegular():
			return os.Open(path)
		}
	}
	return o.base.Open(name)
}

// ErrPromptConflict is a save whose base hash no longer matches the effective
// prompt.
var ErrPromptConflict = errors.New("the prompt changed since it was loaded")

// PromptHash returns the hex sha256 of body.
func PromptHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// OverridePath returns the absolute path of key's prompt override under dir.
// It errors unless key is a valid path under prompts/ that stays under dir.
func OverridePath(dir, key string) (string, error) {
	if !fs.ValidPath(key) || !strings.HasPrefix(key, "prompts/") {
		return "", fmt.Errorf("%q is not a prompt key", key)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, filepath.FromSlash(key))
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is not a prompt key", key)
	}
	return path, nil
}

// SavePrompt writes body as key's prompt override under dir, atomically and
// creating directories as needed. A body equal to the built-in in base removes
// the override instead. It writes nothing and returns an error wrapping
// ErrPromptConflict when baseHash is not the hash of the effective prompt.
func SavePrompt(dir string, base fs.FS, key, body, baseHash string) error {
	path, err := OverridePath(dir, key)
	if err != nil {
		return err
	}
	builtin, err := fs.ReadFile(base, key)
	if err != nil {
		return err
	}
	current, err := fs.ReadFile(Overlay(dir, base), key)
	if err != nil {
		return err
	}
	if PromptHash(string(current)) != baseHash {
		return fmt.Errorf("%s: %w", key, ErrPromptConflict)
	}
	if body == string(builtin) {
		return removeOverride(path)
	}
	return writeAtomic(path, body)
}

// ResetPrompt removes key's prompt override under dir. A missing override is
// not an error.
func ResetPrompt(dir, key string) error {
	path, err := OverridePath(dir, key)
	if err != nil {
		return err
	}
	return removeOverride(path)
}

// removeOverride removes the file at path. Nothing there, or a directory, is
// left alone.
func removeOverride(path string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		return nil
	}
	return os.Remove(path)
}

// writeAtomic replaces path with body through a temp file in the same
// directory, which is removed on failure.
func writeAtomic(path, body string) (err error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(parent, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.WriteString(body); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
