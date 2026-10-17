package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/gittest"
)

// FetchBranch brings origin's branch into the remote-tracking ref and names
// its commit, leaving the checkout's branch and HEAD alone.
func TestFetchBranch(t *testing.T) {
	repo, origin, sha := shipped(t)
	other := t.TempDir()
	gitTest(t, other, "clone", "-q", origin, ".")
	gittest.Identity(t, other)
	gitTest(t, other, "checkout", "-q", "feat/x")
	os.WriteFile(filepath.Join(other, "a.txt"), []byte("theirs\n"), 0o644)
	gitTest(t, other, "commit", "-qam", "theirs")
	gitTest(t, other, "push", "-q", "origin", "feat/x")
	theirs, _ := Head(other)

	got, err := FetchBranch(repo, "feat/x")
	if err != nil || got != theirs {
		t.Fatalf("FetchBranch = %s, %v; want %s", got, err, theirs)
	}
	if head, _ := Head(repo); head != sha {
		t.Fatalf("the fetch moved HEAD to %s", head)
	}
}

// A fetch that finds its remote-tracking ref locked by another fetch waits
// for it rather than failing.
func TestFetchBranchWaitsForALockedRef(t *testing.T) {
	repo, _, sha := shipped(t)
	lock := filepath.Join(repo, ".git", "refs", "remotes", "origin", "feat", "x.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, func() { os.Remove(lock) })

	if got, err := FetchBranch(repo, "feat/x"); err != nil || got != sha {
		t.Fatalf("FetchBranch = %s, %v; want %s", got, err, sha)
	}
}

// The push is a fast-forward guarded by the head lathe expects: it lands when
// origin is still there, and is refused when someone moved it.
func TestPushExpecting(t *testing.T) {
	repo, origin, sha := shipped(t)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("c\n"), 0o644)
	gitTest(t, repo, "commit", "-qam", "next")
	next, _ := Head(repo)
	if err := PushExpecting(repo, "feat/x", sha); err != nil {
		t.Fatal(err)
	}
	if got, _ := RemoteHead(repo, "feat/x"); got != next {
		t.Fatalf("origin at %s, want %s", got, next)
	}

	// Someone else moves origin back; the next push expecting next is refused
	// and leaves their update in place.
	gitTest(t, origin, "update-ref", "refs/heads/feat/x", sha)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("d\n"), 0o644)
	gitTest(t, repo, "commit", "-qam", "again")
	if err := PushExpecting(repo, "feat/x", next); err == nil {
		t.Fatal("pushed over an update lathe did not expect")
	}
	if got, _ := RemoteHead(repo, "feat/x"); got != sha {
		t.Fatalf("origin moved to %s", got)
	}
	if got, err := RemoteHead(repo, "nope"); got != "" || err != nil {
		t.Fatalf("a missing branch = %q, %v", got, err)
	}
}
