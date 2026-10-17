package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeGH puts a gh on PATH that reports one pull request from the file pr.json
// in its directory, and returns that path.
func fakeGH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pr := filepath.Join(dir, "pr.json")
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\ncat "+pr+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pr
}

// shipped is a checkout on feat/x at a commit origin also has: what a build
// leaves behind.
func shipped(t *testing.T) (repo, origin, sha string) {
	t.Helper()
	repo, origin = t.TempDir(), t.TempDir()
	run(t, origin, "init", "-q", "--bare")
	run(t, repo, "init", "-q", "-b", "main")
	run(t, repo, "config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	run(t, repo, "add", ".")
	run(t, repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "base")
	run(t, repo, "checkout", "-qb", "feat/x")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("b\n"), 0o644)
	run(t, repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qam", "change")
	run(t, repo, "remote", "add", "origin", origin)
	run(t, repo, "push", "-q", "origin", "main", "feat/x")
	head, err := Head(repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, origin, head
}
