// Package permit is the write boundary, in both languages it is written in.
// guard.ts is the veto: Pi runs it before a tool executes, so a write outside
// the plan never happens. The Go here is what checks the same rules on a plan
// before an agent sees it, and what reverts anything that survived a turn
// anyway. Denied is the rule both halves implement and neither may drift from.
package permit

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Norm is the one canonical path form every half of the boundary compares in:
// slash-separated and cleaned, so "./README.md" out of a plan and "README.md"
// out of git status are the same string. The guard, Enforce and the gates all
// match by equality, so they must all normalize here rather than each inline.
func Norm(path string) string { return filepath.ToSlash(filepath.Clean(path)) }

// Denied reports the first protected pattern path matches, if any. Two forms,
// both stdlib, because the list is short and a matcher is not worth writing:
//
//   - an entry ending in "/" is a directory name, matched against any segment
//     of the path, so ".git/" covers "vendor/x/.git/config" as well as ".git/HEAD"
//   - anything else is a filepath.Match glob against the basename, so "*.pem"
//     covers a key wherever it is
//
// path is repo-relative and slash-separated by the time it gets here; callers
// that start from an absolute path resolve it first.
func Denied(path string, deny []string) (string, bool) {
	clean := Norm(path)
	segments := strings.Split(clean, "/")
	for _, pattern := range deny {
		if dir := strings.TrimSuffix(pattern, "/"); dir != pattern {
			for _, s := range segments {
				if s == dir {
					return pattern, true
				}
			}
			continue
		}
		// A bad pattern in the roster matches nothing rather than panicking;
		// filepath.Match only errors on a malformed glob.
		if ok, _ := filepath.Match(pattern, segments[len(segments)-1]); ok {
			return pattern, true
		}
	}
	return "", false
}

// Escapes reports whether a repo-relative path would land outside the repo.
// An absolute path counts: "every path in the plan is repo-relative" is the
// claim that makes the allow list a string comparison later.
func Escapes(path string) bool {
	if filepath.IsAbs(path) {
		return true
	}
	clean := filepath.Clean(path)
	return clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// Scope is what one agent turn is allowed to leave behind, and the whole of
// what the guard extension is told. Allow is exact repo-relative paths — the
// plan's file list — so matching it is string equality rather than a glob
// engine. Deny and BashDeny are patterns. Deny always wins.
type Scope struct {
	Allow    []string `json:"allow"`
	Deny     []string `json:"deny"`
	BashDeny []string `json:"bashDeny"`
}

// ScopeFile is the name Write gives the scope inside the run directory, and
// GuardFile the extension's. Both live with raw.jsonl rather than in the target
// repo: nothing lathe does is stamped into the repo it acts on.
const (
	ScopeFile = "permit.json"
	GuardFile = "guard.ts"
)

//go:embed guard.ts
var guard []byte

// Guard writes the extension into dir once per run and returns its path. It is
// compiled in rather than installed, so there is no install root to resolve and
// no way for the target repo to supply a different one.
func Guard(dir string) (string, error) {
	path := filepath.Join(dir, GuardFile)
	return path, os.WriteFile(path, guard, 0o600)
}

// Write rewrites the scope the guard reads. It is called before every spawn,
// because the builder's allow list and the tester's differ, and the guard
// re-reads the file per tool call so a rewrite needs no reload.
func Write(dir string, s Scope) (string, error) {
	// Norm here is what keeps the guard and Enforce reading the same list. A
	// fresh slice, because the caller's is the plan's and this is not its
	// business to rewrite.
	allow := make([]string, len(s.Allow))
	for i, path := range s.Allow {
		allow[i] = Norm(path)
	}
	s.Allow = allow
	// Marshalling a nil slice gives null, which would crash the guard on
	// `scope.allow.includes`. Empty is the honest encoding of "permits nothing".
	for _, list := range []*[]string{&s.Deny, &s.BashDeny} {
		if *list == nil {
			*list = []string{}
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, ScopeFile)
	return path, os.WriteFile(path, b, 0o600)
}

// Clean fails if repo has uncommitted changes. Rollback cannot tell the agent's
// edits from yours, so it refuses to guess: a writing run starts from a tree
// where everything that is there afterwards is the agent's doing.
func Clean(repo string) error {
	dirty, err := status(repo)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("%s has uncommitted changes (%s); commit or stash them first",
			repo, strings.Join(first(dirty, 5), ", "))
	}
	return nil
}

// Enforce reverts everything in repo that the scope does not allow and reports
// both sides. It is defense in depth: with the guard in front of the tool this
// should find nothing the builder did, and if it does, that is a guard bug and
// the trace says so. It does real work after a test run, which writes whatever
// the suite felt like writing.
//
// The caller is responsible for having passed Clean first. Without that
// guarantee this reverts work that was never the agent's.
func Enforce(repo string, s Scope) (kept, reverted []string, err error) {
	dirty, err := status(repo)
	if err != nil {
		return nil, nil, err
	}
	allowed := map[string]bool{}
	for _, p := range s.Allow {
		allowed[Norm(p)] = true
	}
	for _, e := range dirty {
		// Deny always wins, so being allowed is not on its own enough.
		if _, protected := Denied(e.path, s.Deny); allowed[e.path] && !protected {
			kept = append(kept, e.path)
			continue
		}
		if err := revert(repo, e); err != nil {
			return kept, reverted, err
		}
		reverted = append(reverted, e.path)
	}
	return kept, reverted, nil
}

// Changed lists every uncommitted path, including untracked files and deletions.
// A rename arrives as both halves, so a caller checking claims sees both paths.
func Changed(repo string) ([]string, error) {
	entries, err := status(repo)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.path)
	}
	return paths, nil
}

// Fingerprint records the content of every uncommitted path, including new and
// untracked files. It is taken before a validation group starts, so Mutations
// can say afterwards whether the code the group judged is still the code in the
// tree. Committed files need no fingerprint: git status already says when one
// of them changes.
func Fingerprint(repo string) (map[string]string, error) {
	dirty, err := status(repo)
	if err != nil {
		return nil, err
	}
	fp := make(map[string]string, len(dirty))
	for _, e := range dirty {
		fp[e.path] = digest(filepath.Join(repo, e.path))
	}
	return fp, nil
}

// Mutations lists the source paths whose content differs from the fingerprint.
// Source is everything the fingerprint held plus everything committed; a path
// that is new since the fingerprint and was never committed is a test's
// leavings, which the cleanup after it removes. The split is what keeps a
// changed implementation file from passing as test output: it was dirty when
// the fingerprint was taken, so any change to it counts.
//
// It cannot see a file that was changed and put back while nobody looked, nor
// a change to an ignored file.
func Mutations(repo string, before map[string]string) ([]string, error) {
	dirty, err := status(repo)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(dirty))
	var changed []string
	for _, e := range dirty {
		seen[e.path] = true
		if sum, ok := before[e.path]; ok {
			if digest(filepath.Join(repo, e.path)) != sum {
				changed = append(changed, e.path)
			}
			continue
		}
		if inHEAD(repo, e.path) {
			changed = append(changed, e.path)
		}
	}
	// Dirty before and clean now is a change too: something put it back.
	for p := range before {
		if !seen[p] {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// digest is a file's content hash, or "missing" for a path with no file, which
// is how a deletion compares.
func digest(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "missing"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// entry is one line of git status: the path, and whether git has ever seen it.
type entry struct {
	path      string
	untracked bool
}

// status lists what is uncommitted.
//
// -uall because a bare -u collapses an untracked directory into one entry, so a
// whole directory of out-of-scope files arrives as a path git checkout cannot
// revert. -z because a path with a space in it comes back quoted and escaped
// otherwise, and the parser gets it silently wrong for exactly the files you
// would most want reverted. --no-renames because a rename is one record holding
// two NUL-separated paths, which this parser would mis-slice; off, it arrives as
// "D old" plus "?? new", which revert and the claim gate already cover.
// Ignored files are deliberately absent: build caches and node_modules are not
// the agent's doing.
func status(repo string) ([]entry, error) {
	cmd := exec.Command("git", "status", "--porcelain=v1", "-z", "-uall", "--no-renames")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %w", repo, err)
	}
	var entries []entry
	for _, rec := range strings.Split(string(out), "\x00") {
		// Each record is "XY <path>"; the shortest real one is four bytes.
		if len(rec) < 4 {
			continue
		}
		entries = append(entries, entry{
			path:      Norm(rec[3:]),
			untracked: rec[:2] == "??",
		})
	}
	return entries, nil
}

// revert puts one path back to the state Clean vouched for.
//
// From HEAD rather than from the index, because `git checkout -- <path>` reads
// the index: a test suite that ran `git add` on an out-of-scope edit would have
// it "reverted" to itself, reported as reverted, and still sitting there. The
// bash deny list does not stop staging and is not the thing that should have to.
//
// A path HEAD has never seen has nothing to go back to, so it is removed — and
// unstaged first, or the index keeps content the working tree no longer has. No
// rename branch: a worktree rename arrives as "D old" plus "?? new", which both
// sides already cover.
func revert(repo string, e entry) error {
	if inHEAD(repo, e.path) {
		return git(repo, "checkout", "HEAD", "--", e.path)
	}
	if !e.untracked {
		if err := git(repo, "reset", "-q", "HEAD", "--", e.path); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(repo, e.path))
}

// inHEAD reports whether the last commit carries this path.
func inHEAD(repo, path string) bool {
	cmd := exec.Command("git", "cat-file", "-e", "HEAD:"+path)
	cmd.Dir = repo
	return cmd.Run() == nil
}

// git runs with --literal-pathspecs: a path here is always one exact file, and a
// Next.js route like pages/blog/[slug].tsx must not also revert pages/blog/s.tsx.
func git(repo string, args ...string) error {
	cmd := exec.Command("git", append([]string{"--literal-pathspecs"}, args...)...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), repo, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func first(entries []entry, n int) []string {
	var paths []string
	for _, e := range entries {
		if len(paths) == n {
			return append(paths, "...")
		}
		paths = append(paths, e.path)
	}
	return paths
}
