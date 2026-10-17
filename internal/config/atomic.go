package config

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with body, owner-only (os.CreateTemp's mode),
// through a temp file in the same directory, creating missing parent
// directories owner-only. The temp file is removed on failure.
func WriteFileAtomic(path string, body []byte) (err error) {
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
	if _, err = f.Write(body); err != nil {
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
