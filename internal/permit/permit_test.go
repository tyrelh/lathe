package permit

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tyrelh/lathe/internal/process"
)

// TestMain lets this test binary serve as the supervisor helper, so the
// supervised tests run git the way a writing run does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == process.HelperArg {
		os.Exit(process.Helper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

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
	if got := read(t, dir, ScopeFile); got != `{"allow":[],"deny":[],"bashDeny":[],"readRoots":[]}` {
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

// A Next.js route is a literal filename that git would read as a glob:
// reverting pages/blog/[slug].tsx must not also revert pages/blog/s.tsx, which
// the pattern [slug].tsx matches and which the scope allowed.
func TestEnforceTreatsBracketedPathsLiterally(t *testing.T) {
	repo := gitRepo(t)
	write(t, repo, "pages/blog/[slug].tsx", "route\n")
	write(t, repo, "pages/blog/s.tsx", "sibling\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "routes")
	write(t, repo, "pages/blog/[slug].tsx", "out of scope\n")
	write(t, repo, "pages/blog/s.tsx", "allowed edit\n")

	kept, reverted, err := Enforce(repo, Scope{Allow: []string{"pages/blog/s.tsx"}, Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reverted, []string{"pages/blog/[slug].tsx"}) || !slices.Equal(kept, []string{"pages/blog/s.tsx"}) {
		t.Fatalf("kept %v, reverted %v", kept, reverted)
	}
	if got := read(t, repo, "pages/blog/s.tsx"); got != "allowed edit\n" {
		t.Fatalf("reverting the bracketed route also reverted its sibling: %q", got)
	}
	if got := read(t, repo, "pages/blog/[slug].tsx"); got != "route\n" {
		t.Fatalf("[slug].tsx = %q", got)
	}
}

// worktree adds a detached worktree of repo at HEAD, as a writing run gets.
func worktree(t *testing.T, repo string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "--detach", dir, "HEAD")
	return dir
}

// Enforce in one worktree reverts only that worktree: the source checkout and
// a sibling worktree share its object store and refs, and none of their dirt is
// the run's.
func TestEnforceStaysInsideItsWorktree(t *testing.T) {
	repo := gitRepo(t)
	wt, sibling := worktree(t, repo), worktree(t, repo)
	for _, dir := range []string{repo, sibling} {
		write(t, dir, "tracked.txt", "someone else's edit\n")
		write(t, dir, "theirs.go", "package main\n")
	}
	write(t, wt, "tracked.txt", "the agent's edit\n")
	write(t, wt, "escaped.go", "package main\n")

	kept, reverted, err := Enforce(wt, Scope{Allow: []string{"tracked.txt"}, Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"tracked.txt"}) || !slices.Equal(reverted, []string{"escaped.go"}) {
		t.Fatalf("kept %v, reverted %v", kept, reverted)
	}
	for _, dir := range []string{repo, sibling} {
		if got := read(t, dir, "tracked.txt"); got != "someone else's edit\n" {
			t.Fatalf("%s: tracked.txt = %q", dir, got)
		}
		if _, err := os.Stat(filepath.Join(dir, "theirs.go")); err != nil {
			t.Fatalf("%s: an untracked file outside the worktree was removed: %v", dir, err)
		}
	}
	if err := Clean(repo); err == nil {
		t.Fatal("the source checkout's dirt should be its own, not cleaned away")
	}
}

// In a worktree .git is a file naming the real git directory. Protected paths
// written beside it go, allowed or not, and the .git file itself is left alone.
func TestEnforceRevertsProtectedPathsInAWorktree(t *testing.T) {
	repo := gitRepo(t)
	wt := worktree(t, repo)
	pointer := read(t, wt, ".git")
	if !strings.HasPrefix(pointer, "gitdir: ") {
		t.Fatalf(".git in a worktree should be a gitdir file: %q", pointer)
	}
	protected := []string{".env", "config/.env.local", "certs/server.pem", "deploy/id.key"}
	for _, p := range protected {
		write(t, wt, p, "secret\n")
	}

	_, reverted, err := Enforce(wt, Scope{Allow: protected, Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(reverted)
	want := slices.Clone(protected)
	sort.Strings(want)
	if !slices.Equal(reverted, want) {
		t.Fatalf("reverted = %v; want %v", reverted, want)
	}
	if got := read(t, wt, ".git"); got != pointer {
		t.Fatalf(".git changed: %q", got)
	}
	if err := Clean(wt); err != nil {
		t.Fatal(err)
	}
}

// A symlink committed nowhere and pointing out of the worktree is untracked
// dirt like any other: Enforce removes the link and never follows it.
func TestEnforceRemovesASymlinkOutOfTheWorktree(t *testing.T) {
	repo := gitRepo(t)
	wt := worktree(t, repo)
	if err := os.Symlink(filepath.Join(repo, "tracked.txt"), filepath.Join(wt, "link.txt")); err != nil {
		t.Fatal(err)
	}
	_, reverted, err := Enforce(wt, Scope{Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reverted, []string{"link.txt"}) {
		t.Fatalf("reverted = %v", reverted)
	}
	if _, err := os.Lstat(filepath.Join(wt, "link.txt")); !os.IsNotExist(err) {
		t.Fatal("the link survived")
	}
	if got := read(t, repo, "tracked.txt"); got != "original\n" {
		t.Fatalf("the link's target changed: %q", got)
	}
}

// launches is a Registrar counting the supervised launches it saw.
type launches struct {
	mu                        sync.Mutex
	intents, registered, done int
}

func (l *launches) Intent(string, []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.intents++
	return nil
}

func (l *launches) Register(string, process.Registration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.registered++
	return nil
}

func (l *launches) Drained(string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.done++
	return nil
}

// With a runner installed, as a writing run installs one, every git command
// the boundary runs is a supervised launch, and the answers are the same as
// running git directly.
func TestBoundaryRunsGitSupervised(t *testing.T) {
	repo := gitRepo(t)
	wt := worktree(t, repo)
	l := &launches{}
	defer process.Install(&process.Runner{Bin: os.Args[0], Registrar: l})()

	if err := Clean(wt); err != nil {
		t.Fatal(err)
	}
	before, err := Fingerprint(wt)
	if err != nil {
		t.Fatal(err)
	}
	write(t, wt, "tracked.txt", "changed\n")
	write(t, wt, "leavings.txt", "test output\n")
	// tracked.txt is committed, so inHEAD has to answer through the runner for
	// it to count; leavings.txt is not, so it must not.
	changed, err := Mutations(wt, before)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"tracked.txt"}) {
		t.Fatalf("mutations = %v; want tracked.txt", changed)
	}
	_, reverted, err := Enforce(wt, Scope{Deny: shipped})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(reverted)
	if !slices.Equal(reverted, []string{"leavings.txt", "tracked.txt"}) {
		t.Fatalf("reverted = %v", reverted)
	}
	if got := read(t, wt, "tracked.txt"); got != "original\n" {
		t.Fatalf("tracked.txt = %q", got)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.intents == 0 || l.intents != l.registered || l.registered != l.done {
		t.Fatalf("launches: %d intents, %d registered, %d drained; want every git command supervised and drained",
			l.intents, l.registered, l.done)
	}
}

// A failing git command through the runner is still an error naming where it
// ran, not an empty status read as a clean tree.
func TestStatusFailsSupervised(t *testing.T) {
	defer process.Install(&process.Runner{Bin: os.Args[0], Registrar: &launches{}})()
	dir := t.TempDir()
	err := Clean(dir)
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("Clean outside a repository = %v", err)
	}
}

// Partition keeps order, skips empty names and splits on the deny list.
func TestPartition(t *testing.T) {
	denied, kept := Partition([]string{"a.go", "", ".env", "x/.git/config", "b.go"}, []string{".env*", ".git/"})
	if strings.Join(denied, ",") != ".env,x/.git/config" || strings.Join(kept, ",") != "a.go,b.go" {
		t.Fatalf("denied %v kept %v", denied, kept)
	}
}
