package workspace

import (
	"github.com/tyrelh/lathe/internal/gittest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func out(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gittest.Git(t, dir, args...)
}

// source is a committed checkout with a text file, a script and a file to
// delete, and its HEAD.
func source(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	gittest.Init(t, repo)
	gitTest(t, repo, "config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "gone.txt"), []byte("gone\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "run.sh"), []byte("echo\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored/\n"), 0o644)
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-qm", "base")
	return repo, out(t, repo, "rev-parse", "HEAD")
}

// A worktree is created detached at the given commit under the managed root,
// and removed from disk and from git's list.
func TestCreateAndRemoveWorktree(t *testing.T) {
	repo, base := source(t)
	dataRoot := t.TempDir()
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("person's change\n"), 0o644)

	path, err := CreateWorktree(dataRoot, repo, "run1", 0, base)
	if err != nil {
		t.Fatal(err)
	}
	if path != WorktreePath(dataRoot, "run1", 0) {
		t.Fatalf("path = %s", path)
	}
	if got := out(t, path, "rev-parse", "HEAD"); got != base {
		t.Fatalf("HEAD = %s; want %s", got, base)
	}
	if b, _ := os.ReadFile(filepath.Join(path, "a.txt")); string(b) != "a\n" {
		t.Fatalf("the worktree has the checkout's change: %q", b)
	}
	if _, err := CreateWorktree(dataRoot, repo, "run1", 0, base); err == nil {
		t.Fatal("an existing worktree was reused")
	}
	if err := RemoveWorktree(dataRoot, repo, "run1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the worktree is still on disk")
	}
	if list := out(t, repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("stale entries:\n%s", list)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "person's change\n" {
		t.Fatal("the checkout was touched")
	}
	// Removing again, or removing what an interrupted add left unlisted, is fine.
	if err := RemoveWorktree(dataRoot, repo, "run1", 0); err != nil {
		t.Fatal(err)
	}
	stray := WorktreePath(dataRoot, "run2", 1)
	os.MkdirAll(stray, 0o755)
	os.WriteFile(filepath.Join(stray, "x"), nil, 0o644)
	if err := RemoveWorktree(dataRoot, repo, "run2", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("the unlisted directory survived")
	}
}

// Nothing but the exact managed path of a named run and iteration is ever
// removed: malformed identities, paths that escape through a symlink, and
// other directories under the root are left alone.
func TestRemoveRefusesUnownedPaths(t *testing.T) {
	repo, _ := source(t)
	dataRoot := t.TempDir()
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	os.WriteFile(keep, []byte("keep"), 0o644)

	for _, id := range []string{"", ".", "..", "a/b", "../x"} {
		if err := RemoveWorktree(dataRoot, repo, id, 0); err == nil {
			t.Fatalf("run %q was accepted", id)
		}
	}
	if err := RemoveWorktree(dataRoot, repo, "run1", -1); err == nil {
		t.Fatal("a negative iteration was accepted")
	}
	// The run directory is a symlink out of the root.
	os.MkdirAll(WorktreeRoot(dataRoot), 0o755)
	os.Symlink(outside, filepath.Join(WorktreeRoot(dataRoot), "evil"))
	os.MkdirAll(filepath.Join(outside, "iteration-0"), 0o755)
	if err := RemoveWorktree(dataRoot, repo, "evil", 0); err == nil {
		t.Fatal("a path escaping through a symlink was accepted")
	}
	// Directories under the root that are not that iteration stay.
	other := filepath.Join(WorktreeRoot(dataRoot), "run1", "iteration-0x")
	os.MkdirAll(other, 0o755)
	if err := RemoveWorktree(dataRoot, repo, "run1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("a directory lathe does not own was removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("a file outside the root was removed")
	}
}

// The patch carries commits and uncommitted work beyond base — edits,
// deletions, new files, binary content and modes — leaves out protected and
// ignored paths, applies cleanly at base, and leaves both indexes alone.
func TestSavePatch(t *testing.T) {
	repo, base := source(t)
	dataRoot := t.TempDir()
	wt, err := CreateWorktree(dataRoot, repo, "run1", 0, base)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("committed\n"), 0o644)
	gitTest(t, wt, "commit", "-qam", "work")
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("committed\nand more\n"), 0o644)
	os.Remove(filepath.Join(wt, "gone.txt"))
	os.WriteFile(filepath.Join(wt, "new [1].txt"), []byte("new\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "blob.bin"), []byte{0, 1, 2, 0, 255}, 0o644)
	os.Chmod(filepath.Join(wt, "run.sh"), 0o755)
	os.WriteFile(filepath.Join(wt, ".env"), []byte("SECRET=1\n"), 0o644)
	os.MkdirAll(filepath.Join(wt, "ignored"), 0o755)
	os.WriteFile(filepath.Join(wt, "ignored", "cache"), []byte("x"), 0o644)
	indexBefore := out(t, wt, "diff", "--cached", "--name-only")

	dest := filepath.Join(t.TempDir(), "result.patch")
	wrote, err := SavePatch(wt, base, dest, []string{".env", "*.pem"})
	if err != nil || !wrote {
		t.Fatalf("SavePatch = %v, %v", wrote, err)
	}
	patch, _ := os.ReadFile(dest)
	for _, want := range []string{"a.txt", "gone.txt", "new [1].txt", "blob.bin", "GIT binary patch", "new mode 100755"} {
		if !strings.Contains(string(patch), want) {
			t.Errorf("patch is missing %q", want)
		}
	}
	for _, not := range []string{".env", "ignored/cache"} {
		if strings.Contains(string(patch), not) {
			t.Errorf("patch carries %q", not)
		}
	}
	if out(t, wt, "diff", "--cached", "--name-only") != indexBefore {
		t.Fatal("the worktree's index changed")
	}
	if got := out(t, repo, "status", "--porcelain"); got != "" {
		t.Fatalf("the source checkout changed: %q", got)
	}

	// It applies at base in a fresh clone.
	clone := t.TempDir()
	gitTest(t, clone, "clone", "-q", repo, ".")
	gitTest(t, clone, "checkout", "-q", base)
	gitTest(t, clone, "apply", "--index", dest)
	if b, _ := os.ReadFile(filepath.Join(clone, "a.txt")); string(b) != "committed\nand more\n" {
		t.Fatalf("applied a.txt = %q", b)
	}

	// A later export that finds nothing supersedes the earlier patch.
	os.Remove(filepath.Join(wt, ".env"))
	gitTest(t, wt, "checkout", "-q", "--", ".")
	gitTest(t, wt, "clean", "-qfd")
	gitTest(t, wt, "reset", "-q", "--hard", base)
	os.WriteFile(dest+".part", []byte("interrupted"), 0o644)
	wrote, err = SavePatch(wt, base, dest, nil)
	if err != nil || wrote {
		t.Fatalf("empty SavePatch = %v, %v", wrote, err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a stale patch survived an empty export")
	}
}

// Commits nothing else reaches are kept under the recovery ref; HEAD at base,
// or on a branch, needs nothing.
func TestPreserveHead(t *testing.T) {
	repo, base := source(t)
	dataRoot := t.TempDir()
	wt, err := CreateWorktree(dataRoot, repo, "run1", 0, base)
	if err != nil {
		t.Fatal(err)
	}
	ref := RecoveryRef("run1", 0)
	if got, err := PreserveHead(wt, base, ref); got != "" || err != nil {
		t.Fatalf("at base: %q %v", got, err)
	}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("b\n"), 0o644)
	gitTest(t, wt, "commit", "-qam", "detached work")
	head := out(t, wt, "rev-parse", "HEAD")
	if got, err := PreserveHead(wt, base, ref); got != ref || err != nil {
		t.Fatalf("detached: %q %v", got, err)
	}
	if got := out(t, repo, "rev-parse", ref); got != head {
		t.Fatalf("%s = %s; want %s", ref, got, head)
	}
	gitTest(t, repo, "update-ref", "-d", ref)
	gitTest(t, wt, "branch", "kept")
	if got, err := PreserveHead(wt, base, ref); got != "" || err != nil {
		t.Fatalf("on a branch: %q %v", got, err)
	}
}

// Only protected paths among the commits beyond base are reported.
func TestPublishedProtected(t *testing.T) {
	repo, base := source(t)
	os.WriteFile(filepath.Join(repo, "key.pem"), []byte("k"), 0o644)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("b\n"), 0o644)
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-qm", "oops")
	got, err := PublishedProtected(repo, base, []string{"*.pem"})
	if err != nil || len(got) != 1 || got[0] != "key.pem" {
		t.Fatalf("PublishedProtected = %v, %v", got, err)
	}
}
