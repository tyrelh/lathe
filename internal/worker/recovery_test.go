package worker

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workspace"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

// committedBuild is a build whose only iteration committed feat/x and then
// failed, with its work saved as a patch against base. A gh on PATH answers
// pr list from gh/list and pr view with the state in gh/state.
type committedBuild struct {
	db                                      *trace.DB
	dataRoot, repo, id, base, commit, patch string
	gh                                      string
}

func newCommittedBuild(t *testing.T) *committedBuild {
	t.Helper()
	b := &committedBuild{dataRoot: t.TempDir(), repo: t.TempDir(), gh: t.TempDir()}
	origin := t.TempDir()
	gitT(t, origin, "init", "-q", "--bare")
	gitT(t, b.repo, "init", "-q", "-b", "main")
	gitT(t, b.repo, "config", "user.name", "T")
	gitT(t, b.repo, "config", "user.email", "t@example.com")
	gitT(t, b.repo, "config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(b.repo, "hello.txt"), []byte("hello\n"), 0o644)
	gitT(t, b.repo, "add", ".")
	gitT(t, b.repo, "commit", "-qm", "base")
	gitT(t, b.repo, "remote", "add", "origin", origin)
	b.base = gitT(t, b.repo, "rev-parse", "HEAD")
	gitT(t, b.repo, "checkout", "-q", "-b", "feat/x")
	os.WriteFile(filepath.Join(b.repo, "hello.txt"), []byte("hello world\n"), 0o644)
	gitT(t, b.repo, "commit", "-qam", "greet")
	b.commit = gitT(t, b.repo, "rev-parse", "HEAD")
	b.patch = filepath.Join(b.dataRoot, "result.patch")
	if _, err := workspace.SavePatch(b.repo, b.base, b.patch, nil); err != nil {
		t.Fatal(err)
	}

	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"\"pr list\") cat \"" + b.gh + "/list\" ;;\n" +
		"\"pr view\") printf '{\"url\":\"%s\",\"state\":\"%s\",\"headRefName\":\"feat/x\",\"baseRefName\":\"main\",\"isDraft\":%s,\"headRefOid\":\"%s\",\"isCrossRepository\":false}\\n' \"$3\" \"$(cat \"" + b.gh + "/state\")\" \"$(cat \"" + b.gh + "/draft\")\" \"$(cat \"" + b.gh + "/oid\")\" ;;\n" +
		"*) exit 1 ;;\nesac\n"
	os.WriteFile(filepath.Join(b.gh, "gh"), []byte(script), 0o755)
	os.WriteFile(filepath.Join(b.gh, "list"), []byte("[]"), 0o644)
	os.WriteFile(filepath.Join(b.gh, "state"), []byte("OPEN"), 0o644)
	os.WriteFile(filepath.Join(b.gh, "draft"), []byte("true"), 0o644)
	os.WriteFile(filepath.Join(b.gh, "oid"), []byte(b.commit), 0o644)
	t.Setenv("PATH", b.gh+string(os.PathListSeparator)+os.Getenv("PATH"))

	var err error
	if b.db, err = trace.Open(b.dataRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.db.Close() })
	ws, err := workspace.Local(b.repo)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := Spec{Version: SpecVersion, Workflow: "build", Request: "greet", Workspace: ws, Base: b.base}.Marshal()
	if b.id, err = b.db.Submit(trace.Request{Workflow: "build", Repo: b.repo, Request: "greet", Branch: "main", Spec: spec}); err != nil {
		t.Fatal(err)
	}
	attempt, token := trace.NewAttemptID(b.id), trace.NewClaimToken()
	if err := b.db.Reserve(b.id, attempt, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Claim(attempt, token, "host", os.Getpid(), time.Minute); err != nil {
		t.Fatal(err)
	}
	must(t, b.db.SetWorktree(b.id, attempt, "/wt", b.base, trace.WorktreeActive))
	must(t, b.db.SetShipped(b.id, attempt, "feat/x", b.commit))
	must(t, b.db.Complete(b.id, attempt, token, trace.StatusFail, "push failed"))
	must(t, b.db.SetWorktreeState(b.id, 0, trace.WorktreeRemoved, ""))
	must(t, b.db.SetPatch(b.id, 0, b.patch))
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func noCapture(string, config.Overrides) (config.Snapshot, error) { return config.Snapshot{}, nil }

func (b *committedBuild) prepare(t *testing.T, o ResumeOptions) (Spec, error) {
	t.Helper()
	rec, err := PrepareRecovery(b.db, b.dataRoot, b.id, "carry on", o, noCapture)
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	must(t, json.Unmarshal(rec.Spec, &s))
	return s, nil
}

// recoverAndFail appends a recovery of the latest iteration and records it
// failing without having restored anything, as a failure before worktree
// creation or a failed restoration leaves it. It returns the iteration.
func (b *committedBuild) recoverAndFail(t *testing.T, o ResumeOptions) int {
	t.Helper()
	rec, err := PrepareRecovery(b.db, b.dataRoot, b.id, "try", o, noCapture)
	must(t, err)
	n, err := b.db.Recover(b.id, rec)
	must(t, err)
	attempt, token := trace.NewAttemptID(b.id), trace.NewClaimToken()
	must(t, b.db.Reserve(b.id, attempt, ""))
	if _, err := b.db.Claim(attempt, token, "host", os.Getpid(), time.Minute); err != nil {
		t.Fatal(err)
	}
	must(t, b.db.Complete(b.id, attempt, token, trace.StatusFail, "restoring failed"))
	return n
}

// failedImplement is an implement run, claimed with its worktree recorded at
// base and active, ready for a test to end it.
type failedImplement struct {
	db                    *trace.DB
	dataRoot, repo, id    string
	attempt, token, base string
}

func newFailedImplement(t *testing.T) *failedImplement {
	t.Helper()
	f := &failedImplement{dataRoot: t.TempDir(), repo: t.TempDir()}
	gitT(t, f.repo, "init", "-q", "-b", "main")
	gitT(t, f.repo, "config", "user.name", "T")
	gitT(t, f.repo, "config", "user.email", "t@example.com")
	gitT(t, f.repo, "config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(f.repo, "hello.txt"), []byte("hello\n"), 0o644)
	gitT(t, f.repo, "add", ".")
	gitT(t, f.repo, "commit", "-qm", "base")
	f.base = gitT(t, f.repo, "rev-parse", "HEAD")
	var err error
	if f.db, err = trace.Open(f.dataRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	ws, err := workspace.Local(f.repo)
	must(t, err)
	spec, _ := Spec{Version: SpecVersion, Workflow: "implement", Request: "greet", Workspace: ws, Base: f.base}.Marshal()
	if f.id, err = f.db.Submit(trace.Request{Workflow: "implement", Repo: f.repo, Request: "greet", Branch: "main", Spec: spec}); err != nil {
		t.Fatal(err)
	}
	f.attempt, f.token = trace.NewAttemptID(f.id), trace.NewClaimToken()
	must(t, f.db.Reserve(f.id, f.attempt, ""))
	if _, err := f.db.Claim(f.attempt, f.token, "host", os.Getpid(), time.Minute); err != nil {
		t.Fatal(err)
	}
	must(t, f.db.SetWorktree(f.id, f.attempt, workspace.WorktreePath(f.dataRoot, f.id, 0), f.base, trace.WorktreeActive))
	return f
}

// settle runs cleanup as the manager does for the run's recorded worktree.
func (f *failedImplement) settle(t *testing.T) {
	t.Helper()
	ws, err := f.db.Worktrees()
	must(t, err)
	if len(ws) != 1 {
		t.Fatalf("worktrees = %+v", ws)
	}
	must(t, Settle(f.db, f.dataRoot, ws[0], true))
}

func (f *failedImplement) prepare(o ResumeOptions) (trace.Recovery, error) {
	return PrepareRecovery(f.db, f.dataRoot, f.id, "carry on", o, noCapture)
}

// A lost run whose dirty worktree vanished before it could be preserved has
// unavailable work: normal resume refuses and appends nothing; a restart
// proceeds and says what is missing.
func TestLostWorktreeIsNotEmpty(t *testing.T) {
	f := newFailedImplement(t)
	must(t, f.db.MarkLost(f.id, "worker gone"))
	f.settle(t)
	its, _ := f.db.Iterations(f.id)
	if its[0].Preservation != trace.PreservedMissing || its[0].WorktreeState != trace.WorktreeRemoved {
		t.Fatalf("iteration = %+v", its[0])
	}
	if _, err := f.prepare(ResumeOptions{}); !errors.Is(err, trace.ErrNotRecoverable) || !strings.Contains(err.Error(), "--restart") {
		t.Fatalf("normal resume: %v", err)
	}
	if its, _ := f.db.Iterations(f.id); len(its) != 1 {
		t.Fatal("a refusal appended an iteration")
	}
	rec, err := f.prepare(ResumeOptions{Restart: true})
	must(t, err)
	if _, err := f.db.Recover(f.id, rec); err != nil {
		t.Fatal(err)
	}
	if its, _ := f.db.Iterations(f.id); len(its) != 2 || its[0].Status != trace.StatusLost {
		t.Fatalf("history = %+v", its)
	}
}

// A worktree exported and found empty is known-empty; removal recorded
// without any preservation outcome, as older records are, is not.
func TestEmptyExportVersusLegacyRemoval(t *testing.T) {
	f := newFailedImplement(t)
	if _, err := workspace.CreateWorktree(f.dataRoot, f.repo, f.id, 0, f.base); err != nil {
		t.Fatal(err)
	}
	must(t, f.db.Complete(f.id, f.attempt, f.token, trace.StatusFail, "auth failed"))
	f.settle(t)
	rec, err := f.prepare(ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var s Spec
	must(t, json.Unmarshal(rec.Spec, &s))
	if s.Recovery.Patch != "" || !strings.Contains(s.Recovery.Unavailable, "found no changes") {
		t.Fatalf("empty export = %+v", s.Recovery)
	}

	legacy := newFailedImplement(t)
	must(t, legacy.db.Complete(legacy.id, legacy.attempt, legacy.token, trace.StatusFail, "old"))
	must(t, legacy.db.SetWorktreeState(legacy.id, 0, trace.WorktreeRemoved, ""))
	if _, err := legacy.prepare(ResumeOptions{}); !errors.Is(err, trace.ErrNotRecoverable) {
		t.Fatalf("legacy removal: %v", err)
	}
	if _, err := legacy.prepare(ResumeOptions{Restart: true}); err != nil {
		t.Fatalf("legacy restart: %v", err)
	}
}

// A recovery that never finished restoring still owes its evidence: the next
// recovery restores the same patch at the same base, refuses it once it has
// changed, and only a restart abandons it.
func TestPendingRestorationCarriesForward(t *testing.T) {
	b := newCommittedBuild(t)
	first, err := b.prepare(t, ResumeOptions{})
	must(t, err)
	b.recoverAndFail(t, ResumeOptions{})
	s, err := b.prepare(t, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Recovery.Patch != b.patch || s.Recovery.Digest != first.Recovery.Digest || s.Base != b.base {
		t.Fatalf("pending restoration = %+v, base %s", s.Recovery, s.Base)
	}
	f, _ := os.OpenFile(b.patch, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n")
	f.Close()
	if _, err := b.prepare(t, ResumeOptions{}); !errors.Is(err, trace.ErrNotRecoverable) || !strings.Contains(err.Error(), "--restart") {
		t.Fatalf("changed evidence: %v", err)
	}
	os.Remove(b.patch)
	if _, err := b.prepare(t, ResumeOptions{}); !errors.Is(err, trace.ErrNotRecoverable) {
		t.Fatalf("missing evidence: %v", err)
	}
	if its, _ := b.db.Iterations(b.id); len(its) != 2 {
		t.Fatal("a refusal appended an iteration")
	}
	if s, err := b.prepare(t, ResumeOptions{Restart: true}); err != nil || s.Recovery.Patch != "" || s.Base != b.base {
		t.Fatalf("restart = %+v, %v", s.Recovery, err)
	}
}

// With a delivered commit whose saved work is missing or damaged, a normal
// resume refuses and a restart continues at the verified published head.
func TestRestartWithoutPublishedEvidence(t *testing.T) {
	for _, damage := range []string{"missing", "damaged"} {
		t.Run(damage, func(t *testing.T) {
			b := newCommittedBuild(t)
			gitT(t, b.repo, "push", "-q", "origin", "feat/x")
			if damage == "missing" {
				os.Remove(b.patch)
			} else {
				os.WriteFile(b.patch, []byte("not a patch\n"), 0o600)
			}
			if _, err := b.prepare(t, ResumeOptions{}); !errors.Is(err, trace.ErrNotRecoverable) {
				t.Fatalf("normal resume: %v", err)
			}
			s, err := b.prepare(t, ResumeOptions{Restart: true})
			if err != nil || s.Base != b.commit || s.Recovery.Patch != "" || s.Recovery.Publication.Head != b.commit {
				t.Fatalf("restart = %+v, %v", s.Recovery, err)
			}
		})
	}
}

// An attempted creation is not evidence: a matching draft discovered after
// gh pr create failed is never treated as lathe's own.
func TestIntentIsNotDraftProvenance(t *testing.T) {
	b := newCommittedBuild(t)
	gitT(t, b.repo, "push", "-q", "origin", "feat/x")
	dir := ReportDir(b.dataRoot, b.id, 0)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "pr-intent.json"), []byte(`{"branch":"feat/x","draft":true,"validation":"unresolved"}`), 0o644)
	os.WriteFile(filepath.Join(b.gh, "list"), []byte(`[{"url":"https://x/pull/5","state":"OPEN","headRefName":"feat/x","isDraft":true,"headRefOid":"`+b.commit+`","isCrossRepository":false}]`), 0o644)
	s, err := b.prepare(t, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pub := s.Recovery.Publication; pub.PR != "https://x/pull/5" || !pub.Draft || pub.LatheDraft {
		t.Fatalf("publication = %+v", pub)
	}
}

// A commit that never reached origin is restored from its patch at its base,
// and the recorded branch stays the run's, expected absent on origin.
func TestRecoverCommitBeforePushFailure(t *testing.T) {
	b := newCommittedBuild(t)
	s, err := b.prepare(t, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pub := s.Recovery.Publication
	if pub == nil || pub.Branch != "feat/x" || pub.Head != "" || pub.PR != "" ||
		s.Recovery.Patch != b.patch || s.Base != b.base || s.Version != RecoveryVersion {
		t.Fatalf("spec = %+v %+v %+v", s, s.Recovery, pub)
	}
	must(t, recheck(b.repo, pub))
	gitT(t, b.repo, "push", "-q", "origin", "feat/x")
	if err := recheck(b.repo, pub); err == nil {
		t.Fatal("a branch that appeared on origin passed the execution recheck")
	}
}

// A recovery of published work that fails before committing keeps the
// branch and pull request for the next recovery.
func TestRepeatedRecoveryKeepsPublication(t *testing.T) {
	b := newCommittedBuild(t)
	gitT(t, b.repo, "push", "-q", "origin", "feat/x")
	dir := ReportDir(b.dataRoot, b.id, 0)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "pr.json"), []byte(`{"url":"https://x/pull/1","draft":true,"validation":"unresolved"}`), 0o644)
	rec, err := PrepareRecovery(b.db, b.dataRoot, b.id, "first", ResumeOptions{}, noCapture)
	must(t, err)
	n, err := b.db.Recover(b.id, rec)
	must(t, err)
	attempt, token := trace.NewAttemptID(b.id), trace.NewClaimToken()
	must(t, b.db.Reserve(b.id, attempt, ""))
	if _, err := b.db.Claim(attempt, token, "host", os.Getpid(), time.Minute); err != nil {
		t.Fatal(err)
	}
	must(t, b.db.Complete(b.id, attempt, token, trace.StatusFail, "planning failed"))

	s, err := b.prepare(t, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pub := s.Recovery.Publication
	if s.Recovery.Source != n || pub == nil || pub.Branch != "feat/x" || pub.Head != b.commit ||
		pub.PR != "https://x/pull/1" || !pub.LatheDraft || s.Base != b.commit {
		t.Fatalf("second recovery = %+v %+v", s.Recovery, pub)
	}
}

// A push whose acknowledgement was lost is found on origin: the recovery
// continues from the published commit, reapplies nothing, and keeps lathe's
// recorded draft provenance.
func TestRecoverLostPushAcknowledgement(t *testing.T) {
	b := newCommittedBuild(t)
	gitT(t, b.repo, "push", "-q", "origin", "feat/x")
	dir := ReportDir(b.dataRoot, b.id, 0)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "pr.json"), []byte(`{"url":"https://x/pull/1","draft":true,"validation":"unresolved"}`), 0o644)
	s, err := b.prepare(t, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pub := s.Recovery.Publication
	if pub == nil || pub.Head != b.commit || pub.PR != "https://x/pull/1" || !pub.LatheDraft || s.Base != b.commit || s.Recovery.Patch != "" {
		t.Fatalf("publication = %+v, base %s, patch %q", pub, s.Base, s.Recovery.Patch)
	}
	// Queued state that changes is caught at execution.
	must(t, recheck(b.repo, pub))
	os.WriteFile(filepath.Join(b.gh, "state"), []byte("MERGED"), 0o644)
	if err := recheck(b.repo, pub); err == nil {
		t.Fatal("a merged pull request passed the execution recheck")
	}
}

// A pull request whose creation acknowledgement was lost is discovered by a
// unique branch match; its draft status is of unknown provenance. Two
// matches, a closed pull request, external movement, residual work and an
// incompatible restart base are refused, and a refusal appends nothing.
func TestRecoverPublicationReconciliation(t *testing.T) {
	b := newCommittedBuild(t)
	gitT(t, b.repo, "push", "-q", "origin", "feat/x")
	one := `[{"url":"https://x/pull/2","state":"OPEN","headRefName":"feat/x","baseRefName":"main","isDraft":true,"headRefOid":"` +
		b.commit + `","isCrossRepository":false}]`
	os.WriteFile(filepath.Join(b.gh, "list"), []byte(one), 0o644)
	s, err := b.prepare(t, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pub := s.Recovery.Publication; pub.PR != "https://x/pull/2" || !pub.Draft || pub.LatheDraft {
		t.Fatalf("discovered = %+v", pub)
	}
	if _, err := b.prepare(t, ResumeOptions{Restart: true, Base: "main"}); !errors.Is(err, trace.ErrNotRecoverable) {
		t.Fatalf("restart from an older base: %v", err)
	}

	two := `[` + strings.Trim(one, "[]") + `,` + strings.Trim(strings.Replace(one, "pull/2", "pull/3", 1), "[]") + `]`
	os.WriteFile(filepath.Join(b.gh, "list"), []byte(two), 0o644)
	if _, err := b.prepare(t, ResumeOptions{}); err == nil {
		t.Fatal("ambiguous pull requests accepted")
	}
	os.WriteFile(filepath.Join(b.gh, "list"), []byte(strings.Replace(one, "OPEN", "CLOSED", 1)), 0o644)
	if _, err := b.prepare(t, ResumeOptions{}); err == nil {
		t.Fatal("closed pull request accepted")
	}
	os.WriteFile(filepath.Join(b.gh, "list"), []byte(strings.Replace(one, `"isCrossRepository":false`, `"isCrossRepository":true`, 1)), 0o644)
	if _, err := b.prepare(t, ResumeOptions{}); err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Fatalf("fork pull request with the same branch name: %v", err)
	}
	os.WriteFile(filepath.Join(b.gh, "list"), []byte(strings.Replace(one, b.commit, b.base, 1)), 0o644)
	if _, err := b.prepare(t, ResumeOptions{}); err == nil {
		t.Fatal("a pull request at another commit was adopted")
	}
	os.WriteFile(filepath.Join(b.gh, "list"), []byte("[]"), 0o644)

	// Residual work beyond the published commit is restored there alone; a
	// restart leaves it out and says where it is kept.
	os.WriteFile(filepath.Join(b.repo, "extra.txt"), []byte("residual\n"), 0o644)
	if _, err := workspace.SavePatch(b.repo, b.base, b.patch, nil); err != nil {
		t.Fatal(err)
	}
	s, err = b.prepare(t, ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	residual, _ := os.ReadFile(s.Recovery.Patch)
	if s.Base != b.commit || !strings.Contains(string(residual), "extra.txt") || strings.Contains(string(residual), "hello.txt") {
		t.Fatalf("residual restoration: base %s, patch %s:\n%s", s.Base, s.Recovery.Patch, residual)
	}
	s, err = b.prepare(t, ResumeOptions{Restart: true})
	if err != nil || s.Recovery.Patch != "" || s.Base != b.commit || !strings.Contains(s.Recovery.Unavailable, b.patch) {
		t.Fatalf("restart with residual work: %+v, %v", s.Recovery, err)
	}
	os.Remove(filepath.Join(b.repo, "extra.txt"))
	workspace.SavePatch(b.repo, b.base, b.patch, nil)

	os.WriteFile(filepath.Join(b.repo, "hello.txt"), []byte("someone else\n"), 0o644)
	gitT(t, b.repo, "commit", "-qam", "external")
	gitT(t, b.repo, "push", "-q", "origin", "feat/x")
	if _, err := b.prepare(t, ResumeOptions{}); err == nil || !strings.Contains(err.Error(), "moved outside lathe") {
		t.Fatalf("external movement: %v", err)
	}
	if its, _ := b.db.Iterations(b.id); len(its) != 1 {
		t.Fatal("a refusal appended an iteration")
	}
}

// Evidence follows the iteration that opened the pull request.
func TestEvidenceFromARecoveredIteration(t *testing.T) {
	dataRoot := t.TempDir()
	row := trace.Row{ID: "build_x", Workflow: "build", Branch: "feat/x", Commit: "c1", Iteration: 1}
	if _, err := Evidence(dataRoot, row); err == nil {
		t.Fatal("evidence without a pull request")
	}
	dir := ReportDir(dataRoot, row.ID, 1)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "pr.json"), []byte(`{"url":"https://x/pull/9"}`), 0o644)
	ev, err := Evidence(dataRoot, row)
	if err != nil || ev.PR != "https://x/pull/9" {
		t.Fatalf("evidence = %+v, %v", ev, err)
	}
}
