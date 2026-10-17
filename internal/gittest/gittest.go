package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Git runs git in dir and returns its trimmed output.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Init creates a main-branch repository with local test commit identity.
func Init(t testing.TB, dir string) {
	t.Helper()
	Git(t, dir, "init", "-q", "-b", "main")
	Git(t, dir, "config", "user.name", "T")
	Git(t, dir, "config", "user.email", "t@example.com")
	Git(t, dir, "config", "commit.gpgsign", "false")
}

// Repo commits files in a fresh repository and returns its path and HEAD.
func Repo(t testing.TB, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	Init(t, dir)
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-qm", "init")
	return dir, Git(t, dir, "rev-parse", "HEAD")
}
