package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Saved work is restored exactly into a fresh worktree at its base: edits,
// binary content, modes, new files and deletions.
func TestRestoreAppliesTheSavedPatchExactly(t *testing.T) {
	repo, base := source(t)
	dataRoot := t.TempDir()
	first, err := CreateWorktree(dataRoot, repo, "run1", 0, base)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte{0, 1, 2, 255, 0, 'x'}
	os.WriteFile(filepath.Join(first, "a.txt"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(first, "new.bin"), binary, 0o644)
	os.Remove(filepath.Join(first, "gone.txt"))
	os.Chmod(filepath.Join(first, "run.sh"), 0o755)
	patch := filepath.Join(t.TempDir(), "result.patch")
	if saved, err := SavePatch(first, base, patch, nil); err != nil || !saved {
		t.Fatalf("save = %v, %v", saved, err)
	}
	digest, err := PatchDigest(patch)
	if err != nil {
		t.Fatal(err)
	}

	second, err := CreateWorktree(dataRoot, repo, "run1", 1, base)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := Restore(second, base, patch, digest, []string{".env*"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	if strings.Join(paths, ",") != "a.txt,gone.txt,new.bin,run.sh" {
		t.Fatalf("restored paths = %v", paths)
	}
	if b, _ := os.ReadFile(filepath.Join(second, "new.bin")); !bytes.Equal(b, binary) {
		t.Fatalf("binary content = %v", b)
	}
	if _, err := os.Stat(filepath.Join(second, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("deletion not restored")
	}
	if fi, _ := os.Stat(filepath.Join(second, "run.sh")); fi.Mode()&0o111 == 0 {
		t.Fatal("mode not restored")
	}
}

// A patch that changed since submission, a dirty worktree, a wrong base or
// a protected path is refused before anything is applied.
func TestRestoreRefusesUntrustedEvidence(t *testing.T) {
	repo, base := source(t)
	dataRoot := t.TempDir()
	first, _ := CreateWorktree(dataRoot, repo, "run1", 0, base)
	os.WriteFile(filepath.Join(first, "a.txt"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(first, ".env"), []byte("SECRET=1\n"), 0o644)
	patch := filepath.Join(t.TempDir(), "result.patch")
	if _, err := SavePatch(first, base, patch, nil); err != nil {
		t.Fatal(err)
	}
	digest, _ := PatchDigest(patch)
	second, _ := CreateWorktree(dataRoot, repo, "run1", 1, base)

	if _, err := Restore(second, base, patch, digest, []string{".env*"}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected path: %v", err)
	}
	if _, err := Restore(second, base, patch, "0000", nil); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("digest: %v", err)
	}
	if _, err := Restore(second, "deadbeef", patch, digest, nil); err == nil {
		t.Fatal("wrong base accepted")
	}
	os.WriteFile(filepath.Join(second, "dirt.txt"), []byte("x\n"), 0o644)
	if _, err := Restore(second, base, patch, digest, nil); err == nil {
		t.Fatal("dirty worktree accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(second, "a.txt")); string(b) != "a\n" {
		t.Fatal("a refused restoration applied something")
	}
	os.Remove(filepath.Join(second, "dirt.txt"))
	os.Remove(patch)
	if _, err := Restore(second, base, patch, digest, nil); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing patch: %v", err)
	}
}

// Only the work beyond a published commit is restored atop it: edits,
// binary additions and deletions made after the commit.
func TestResidualRestoresOnlyUnpublishedWork(t *testing.T) {
	repo, base := source(t)
	dataRoot := t.TempDir()
	wt, _ := CreateWorktree(dataRoot, repo, "run1", 0, base)
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("published\n"), 0o644)
	gitTest(t, wt, "commit", "-qam", "publish")
	commit := out(t, wt, "rev-parse", "HEAD")
	binary := []byte{0, 9, 255, 'r'}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("published\nand more\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "new.bin"), binary, 0o644)
	os.Remove(filepath.Join(wt, "gone.txt"))
	patch := filepath.Join(t.TempDir(), "result.patch")
	if _, err := SavePatch(wt, base, patch, nil); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "sub", "residual.patch")
	if ok, err := Residual(repo, base, patch, commit, dest); err != nil || !ok {
		t.Fatalf("residual = %v, %v", ok, err)
	}
	digest, _ := PatchDigest(dest)
	second, _ := CreateWorktree(dataRoot, repo, "run1", 1, commit)
	paths, err := Restore(second, commit, dest, digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	if strings.Join(paths, ",") != "a.txt,gone.txt,new.bin" {
		t.Fatalf("restored = %v", paths)
	}
	if b, _ := os.ReadFile(filepath.Join(second, "a.txt")); string(b) != "published\nand more\n" {
		t.Fatalf("a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(second, "new.bin")); !bytes.Equal(b, binary) {
		t.Fatal("binary residual not restored")
	}
	// Work wholly published leaves no residual.
	gitTest(t, wt, "checkout", "-q", "--", ".")
	os.Remove(filepath.Join(wt, "new.bin"))
	SavePatch(wt, base, patch, nil)
	if ok, err := Residual(repo, base, patch, commit, dest); err != nil || ok {
		t.Fatalf("published-only residual = %v, %v", ok, err)
	}
}

func TestResolveCommit(t *testing.T) {
	repo, base := source(t)
	if sha, err := ResolveCommit(repo, "main"); err != nil || sha != base {
		t.Fatalf("resolve = %s, %v", sha, err)
	}
	for _, ref := range []string{"nope", "--all"} {
		if _, err := ResolveCommit(repo, ref); err == nil {
			t.Fatalf("%q resolved", ref)
		}
	}
}
