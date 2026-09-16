package install

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// repo returns a fake lathe checkout and a fake home with one skill directory.
func repo(t *testing.T) (repoRoot, home string) {
	t.Helper()
	repoRoot = t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "SKILL.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	home = t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude/skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return repoRoot, home
}

func TestRunLinksAndIsIdempotent(t *testing.T) {
	repoRoot, home := repo(t)
	link := filepath.Join(home, ".claude/skills/lathe")

	for i := 0; i < 2; i++ {
		if err := Run(repoRoot, io.Discard); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if target, err := os.Readlink(link); err != nil || target != repoRoot {
			t.Fatalf("run %d: link = %q, %v; want %q", i, target, err, repoRoot)
		}
	}
	// .codex/skills does not exist in the fake home, so it must be skipped, not created.
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("created a skill directory that did not exist: %v", err)
	}
}

func TestRunRefusesToClobber(t *testing.T) {
	repoRoot, home := repo(t)
	link := filepath.Join(home, ".claude/skills/lathe")

	if err := os.WriteFile(link, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run(repoRoot, io.Discard); err == nil {
		t.Fatal("overwrote a real file")
	}
	if b, _ := os.ReadFile(link); string(b) != "mine" {
		t.Fatalf("file changed: %q", b)
	}

	os.Remove(link)
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := Run(repoRoot, io.Discard); err == nil {
		t.Fatal("retargeted someone else's symlink")
	}
}

func TestRunRejectsNonRepo(t *testing.T) {
	_, _ = repo(t)
	if err := Run(t.TempDir(), io.Discard); err == nil {
		t.Fatal("accepted a directory with no SKILL.md")
	}
}
