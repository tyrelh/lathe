package permit

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The shipped deny list, so the test is about the patterns lathe actually runs
// with rather than fixtures that could drift away from them.
var shipped = []string{".git/", ".env*", "*.pem", "*.key"}

func TestDenied(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{".git/config", true},
		{"vendor/x/.git/HEAD", true}, // a directory name, at any depth
		{".env", true},
		{".env.production", true},
		{"config/.env.local", true},
		{"certs/server.pem", true},
		{"deploy/id.key", true},
		{"internal/run/run.go", false},
		{"gitignore.md", false},   // ".git/" is a segment, not a prefix
		{"env.go", false},         // ".env*" is anchored at the dot
		{"keys/README.md", false}, // "*.key" is the extension, not the directory
	} {
		if _, got := Denied(tc.path, shipped); got != tc.want {
			t.Errorf("Denied(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
	if pattern, _ := Denied(".env", shipped); pattern != ".env*" {
		t.Errorf("pattern = %q; want the rule that matched, for the agent to read", pattern)
	}
}

func TestEscapes(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"internal/run/run.go", false},
		{"./main.go", false},
		{"a/../main.go", false}, // lands back inside
		{"..", true},
		{"../outside.go", true},
		{"a/../../outside.go", true},
		{"/etc/passwd", true},
	} {
		if got := Escapes(tc.path); got != tc.want {
			t.Errorf("Escapes(%q) = %v; want %v", tc.path, got, tc.want)
		}
	}
}

// gitRepo makes a throwaway repo with one committed file, which is the state
// Clean demands and the state Enforce reverts back to.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "tracked.txt", "original\n")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCleanRefusesADirtyTree(t *testing.T) {
	repo := gitRepo(t)
	if err := Clean(repo); err != nil {
		t.Fatalf("a freshly committed repo is clean: %v", err)
	}
	write(t, repo, "yours.txt", "work in progress\n")
	err := Clean(repo)
	if err == nil {
		t.Fatal("Clean accepted a dirty tree; rollback cannot then tell your edits from the agent's")
	}
	if !strings.Contains(err.Error(), "yours.txt") {
		t.Fatalf("the error should name what is dirty: %v", err)
	}
}

func TestEnforceRevertsOnlyWhatIsOutOfScope(t *testing.T) {
	repo := gitRepo(t)
	write(t, repo, "tracked.txt", "the agent's edit\n")  // in scope, tracked
	write(t, repo, "new.go", "package main\n")           // in scope, untracked
	write(t, repo, "escaped.go", "package main\n")       // out of scope, untracked
	write(t, repo, "a dir/with space.go", "package a\n") // out of scope, and -z is why it parses
	write(t, repo, "nested/deep/junk.txt", "coverage\n") // out of scope, and -uall is why it is listed

	kept, reverted, err := Enforce(repo, Scope{Allow: []string{"tracked.txt", "new.go"}, Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(kept)
	sort.Strings(reverted)
	if want := []string{"new.go", "tracked.txt"}; !slices.Equal(kept, want) {
		t.Fatalf("kept = %v; want %v", kept, want)
	}
	if want := []string{"a dir/with space.go", "escaped.go", "nested/deep/junk.txt"}; !slices.Equal(reverted, want) {
		t.Fatalf("reverted = %v; want %v", reverted, want)
	}

	if got := read(t, repo, "tracked.txt"); got != "the agent's edit\n" {
		t.Fatalf("an in-scope edit was reverted: %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.go")); err != nil {
		t.Fatal("an in-scope new file was removed:", err)
	}
	for _, gone := range []string{"escaped.go", "a dir/with space.go", "nested/deep/junk.txt"} {
		if _, err := os.Stat(filepath.Join(repo, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s survived Enforce", gone)
		}
	}
}

// A tracked file goes back to HEAD rather than being deleted, which is the
// whole reason the two branches are not one.
func TestEnforceRestoresATrackedFile(t *testing.T) {
	repo := gitRepo(t)
	write(t, repo, "tracked.txt", "out of scope\n")
	if _, _, err := Enforce(repo, Scope{Deny: shipped}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, repo, "tracked.txt"); got != "original\n" {
		t.Fatalf("tracked.txt = %q; want it back at HEAD", got)
	}
}

// Deny beats Allow. A plan cannot reach here carrying .env — FilesPermitted
// rejects it — but an allow list is data, and the boundary does not rely on
// something upstream having been careful.
func TestEnforceRevertsAProtectedPathEvenWhenAllowed(t *testing.T) {
	repo := gitRepo(t)
	write(t, repo, ".env", "SECRET=1\n")
	_, reverted, err := Enforce(repo, Scope{Allow: []string{".env"}, Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reverted, []string{".env"}) {
		t.Fatalf("reverted = %v; want .env, because deny always wins", reverted)
	}
}

// The guard crashes on `null.includes`, so an empty scope has to encode as [].
func TestWriteEncodesEmptyListsAsArrays(t *testing.T) {
	dir := t.TempDir()
	path, err := Write(dir, Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, ScopeFile); got != `{"allow":[],"deny":[],"bashDeny":[]}` {
		t.Fatalf("scope file = %s; want empty arrays, never null", got)
	}
	if path != filepath.Join(dir, ScopeFile) {
		t.Fatalf("path = %q", path)
	}
}

// The extension ships in the binary, so a run directory always has one and the
// target repo never gets a say in which.
func TestGuardWritesTheExtension(t *testing.T) {
	dir := t.TempDir()
	path, err := Guard(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := read(t, dir, GuardFile)
	if !strings.Contains(body, `pi.on("tool_call"`) || !strings.Contains(body, "LATHE_PERMIT") {
		t.Fatalf("%s is not the guard:\n%s", path, body)
	}
}

func gitIn(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// `git checkout -- <path>` restores the worktree from the index, so an
// out-of-scope edit that got staged would come back as itself and be reported
// as reverted. Nothing stops a test suite running `git add`.
func TestEnforceRevertsAStagedEdit(t *testing.T) {
	repo := gitRepo(t)
	write(t, repo, "tracked.txt", "out of scope\n")
	gitIn(t, repo, "add", "tracked.txt")

	_, reverted, err := Enforce(repo, Scope{Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reverted, []string{"tracked.txt"}) {
		t.Fatalf("reverted = %v; want tracked.txt", reverted)
	}
	if got := read(t, repo, "tracked.txt"); got != "original\n" {
		t.Fatalf("tracked.txt = %q; want it back at HEAD, not restored from the index", got)
	}
	if err := Clean(repo); err != nil {
		t.Fatalf("the index still carries the staged edit: %v", err)
	}
}

// A staged file HEAD has never seen has nothing to go back to, so it goes —
// from the index as well as from disk.
func TestEnforceRemovesAStagedNewFile(t *testing.T) {
	repo := gitRepo(t)
	write(t, repo, "added.go", "package main\n")
	gitIn(t, repo, "add", "added.go")

	_, reverted, err := Enforce(repo, Scope{Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reverted, []string{"added.go"}) {
		t.Fatalf("reverted = %v; want added.go", reverted)
	}
	if _, err := os.Stat(filepath.Join(repo, "added.go")); !os.IsNotExist(err) {
		t.Fatal("added.go survived on disk")
	}
	if err := Clean(repo); err != nil {
		t.Fatalf("the index still carries the staged file: %v", err)
	}
}

// The guard compares a resolved path against these strings and Enforce compares
// a cleaned one, so an uncleaned "./README.md" is writable by one half of the
// boundary and not the other.
func TestWriteCleansTheAllowList(t *testing.T) {
	dir := t.TempDir()
	plan := []string{"./README.md", "internal/../main.go"}
	if _, err := Write(dir, Scope{Allow: plan}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, ScopeFile); !strings.Contains(got, `"allow":["README.md","main.go"]`) {
		t.Fatalf("scope file = %s; want the allow list cleaned", got)
	}
	if !slices.Equal(plan, []string{"./README.md", "internal/../main.go"}) {
		t.Fatalf("Write rewrote the caller's slice: %v", plan)
	}
}

// The guard is the half of the boundary Go cannot reach, and a divergence
// between the two is a hole neither half's own tests can see. guard_test.ts
// runs the same table this file does; running it from here is what keeps it
// part of `go test ./...` rather than a file someone has to remember.
func TestGuardMatchesGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the guard runs under Pi's own runtime in production")
	}
	cmd := exec.Command(node, "guard_test.ts")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guard_test.ts failed: %v\n%s", err, out)
	}
}
