package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Every way the pull request or origin's branch can have moved on refuses the
// revision and names what to restore; nothing is switched or repaired. The
// local checkout's branch, HEAD and changes do not matter: a revision runs in
// a worktree of its own.
func TestCheckRevisable(t *testing.T) {
	const open = `{"url":"u","state":"OPEN","headRefName":"feat/x","baseRefName":"main"}`
	commit := func(t *testing.T, repo string) {
		os.WriteFile(filepath.Join(repo, "a.txt"), []byte("mine\n"), 0o644)
		run(t, repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qam", "manual")
	}
	for _, tc := range []struct {
		name, pr, want string
		change         func(t *testing.T, repo, origin, sha string)
	}{
		{"eligible", open, "", nil},
		{"checkout on another branch", open, "", func(t *testing.T, repo, _, _ string) { run(t, repo, "checkout", "-q", "main") }},
		{"an unpushed local commit", open, "", func(t *testing.T, repo, _, _ string) { commit(t, repo) }},
		{"a dirty checkout", open, "", func(t *testing.T, repo, _, _ string) {
			os.WriteFile(filepath.Join(repo, "a.txt"), []byte("dirty\n"), 0o644)
		}},
		{"local branch reset back", open, "", func(t *testing.T, repo, _, _ string) { run(t, repo, "reset", "-q", "--hard", "HEAD^") }},
		{"a push to the pull request", open, "is at", func(t *testing.T, repo, _, sha string) {
			commit(t, repo)
			run(t, repo, "push", "-q", "origin", "feat/x")
			run(t, repo, "reset", "-q", "--hard", sha)
		}},
		{"branch deleted on origin", open, "does not exist", func(t *testing.T, _, origin, _ string) {
			run(t, origin, "update-ref", "-d", "refs/heads/feat/x")
		}},
		{"closed", strings.Replace(open, "OPEN", "CLOSED", 1), "is closed", nil},
		{"merged", strings.Replace(open, "OPEN", "MERGED", 1), "is merged", nil},
		{"another head", strings.Replace(open, `"feat/x"`, `"feat/y"`, 1), "comes from feat/y", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := fakeGH(t)
			os.WriteFile(pr, []byte(tc.pr), 0o644)
			repo, origin, sha := shipped(t)
			if tc.change != nil {
				tc.change(t, repo, origin, sha)
			}
			before, _ := Head(repo)
			got, err := CheckRevisable(repo, Evidence{PR: "u", Branch: "feat/x", Commit: sha})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want == "" && got.Base != "main":
				t.Fatalf("pr = %+v", got)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v; want %q", err, tc.want)
			}
			if after, _ := Head(repo); after != before {
				t.Fatal("the check moved HEAD")
			}
		})
	}
}

// FetchBranch brings origin's branch into the remote-tracking ref and names
// its commit, leaving the checkout's branch and HEAD alone.
func TestFetchBranch(t *testing.T) {
	repo, origin, sha := shipped(t)
	other := t.TempDir()
	run(t, other, "clone", "-q", origin, ".")
	run(t, other, "checkout", "-q", "feat/x")
	os.WriteFile(filepath.Join(other, "a.txt"), []byte("theirs\n"), 0o644)
	run(t, other, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qam", "theirs")
	run(t, other, "push", "-q", "origin", "feat/x")
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
	run(t, repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qam", "next")
	next, _ := Head(repo)
	if err := PushExpecting(repo, "feat/x", sha); err != nil {
		t.Fatal(err)
	}
	if got, _ := RemoteHead(repo, "feat/x"); got != next {
		t.Fatalf("origin at %s, want %s", got, next)
	}

	// Someone else moves origin back; the next push expecting next is refused
	// and leaves their update in place.
	run(t, origin, "update-ref", "refs/heads/feat/x", sha)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("d\n"), 0o644)
	run(t, repo, "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-qam", "again")
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
