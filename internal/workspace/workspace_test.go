package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// A workspace names one checkout on one host, and refuses to be executed
// anywhere else — the check a git_ref workspace will not need.
func TestLocalDescribesThisHost(t *testing.T) {
	dir := t.TempDir()
	ws, err := Local(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Kind != KindLocalPath || !filepath.IsAbs(ws.Path) || ws.Host == "" {
		t.Fatalf("workspace = %+v", ws)
	}
	if err := ws.Check(); err != nil {
		t.Fatalf("this host rejected its own workspace: %v", err)
	}
	elsewhere := ws
	elsewhere.Host = "some-other-machine"
	if err := elsewhere.Check(); err == nil {
		t.Fatal("a workspace belonging to another host was accepted")
	}
	unknown := ws
	unknown.Kind = "git_ref"
	if err := unknown.Check(); err == nil {
		t.Fatal("an unknown workspace kind was accepted")
	}
}

// The branch is recorded at submission and rechecked at execution, so a
// detached HEAD has to fail rather than record nothing.
func TestBranchAndCommit(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qm", "one")

	branch, err := Branch(dir)
	if err != nil || branch != "main" {
		t.Fatalf("branch = %q, %v", branch, err)
	}
	commit, err := Commit(dir)
	if err != nil || len(commit) < 7 {
		t.Fatalf("commit = %q, %v", commit, err)
	}
	run(t, dir, "checkout", "-q", "--detach")
	if _, err := Branch(dir); err == nil {
		t.Fatal("a detached HEAD reported a branch")
	}
}

// The lock is what keeps two lathe processes out of one checkout. It
// coordinates lathe only: nothing here stops an editor.
func TestLockIsExclusivePerCheckout(t *testing.T) {
	data, repo := t.TempDir(), t.TempDir()
	held, err := Acquire(data, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(data, repo); err == nil {
		t.Fatal("two processes locked one checkout")
	}
	// A different checkout is a different lock.
	other, err := Acquire(data, t.TempDir())
	if err != nil {
		t.Fatalf("an unrelated checkout was blocked: %v", err)
	}
	other.Release()

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(data, repo)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	again.Release()
	// Releasing twice is not an error: the deferred release and an explicit
	// one must not fight.
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}
