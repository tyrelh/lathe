package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHash(t *testing.T) {
	if Hash("a") == Hash("b") || Hash("a") != Hash("a") || len(Hash("")) != 64 {
		t.Fatal("hash is not a sha256 of the body")
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "file.md")
	for _, body := range []string{"first\n", "second"} {
		if err := WriteAtomic(path, body); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != body {
			t.Fatalf("file = %q, %v", b, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temp files left behind: %v %v", entries, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	for _, d := range []string{filepath.Join(dir, "a"), filepath.Dir(path)} {
		if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v", d, st.Mode())
		}
	}
}
