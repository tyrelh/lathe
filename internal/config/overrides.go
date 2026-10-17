package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// ErrConflict is a write whose base hash no longer matches the file.
var ErrConflict = errors.New("the file changed since it was loaded")

// ErrInvalid is agent configuration that does not validate.
var ErrInvalid = errors.New("invalid agent configuration")

// Hash returns the hex sha256 of body.
func Hash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// WriteAtomic replaces path with body, owner-only (os.CreateTemp's mode),
// through a temp file in the same directory, creating missing parent
// directories owner-only. The temp file is removed on failure.
func WriteAtomic(path, body string) (err error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
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
	return os.Rename(f.Name(), path)
}
