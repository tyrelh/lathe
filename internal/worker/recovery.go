package worker

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyrelh/lathe/internal/config"
	"github.com/tyrelh/lathe/internal/run"
	"github.com/tyrelh/lathe/internal/trace"
	"github.com/tyrelh/lathe/internal/workflow"
	"github.com/tyrelh/lathe/internal/workspace"
)

// RecoverySpec is what a recovery iteration continues from, fixed at
// submission.
type RecoverySpec struct {
	Source   int    `json:"source_iteration"`
	Guidance string `json:"guidance"`
	Restart  bool   `json:"restart,omitempty"`
	// Patch and Digest are the source iteration's saved work and its
	// sha256; empty when it had none or a restart leaves it out.
	Patch  string `json:"patch,omitempty"`
	Digest string `json:"patch_sha256,omitempty"`
	// Failure is how the source iteration ended; Unavailable is what work
	// the recovery does not restore, and why.
	Failure     string `json:"failure"`
	Unavailable string `json:"unavailable,omitempty"`
	// Publication is the source's work as origin already has it, when
	// reconciliation found it delivered.
	Publication *run.Publication `json:"publication,omitempty"`
}

// owned is what reconciliation settled for a writing source iteration: the
// branch and pull request the run owns (nil for none), the commit to
// continue from, and the saved work to restore there.
//
// patch was saved against base. digest, when set, is the digest recorded
// when an earlier recovery took that patch as its evidence. published is a
// commit the source delivered on top of base, which patch includes: only
// what patch holds beyond it may be restored.
type owned struct {
	pub                            *run.Publication
	base, patch, digest, published string
}

// reconcile establishes, fail-closed, what a writing source iteration's
// work is on origin. Ownership comes from the source's own commit and branch
// and from any publication its specification inherited, so a recovery that
// failed before committing keeps the branch and pull request it continued.
// Delivered work becomes the base and only the residual beyond it is
// restored; a branch origin never received is reused with its absence as the
// expected head. External movement, unowned, ambiguous, closed or merged pull
// requests and unreadable state are refused.
func reconcile(dataRoot string, row trace.Row, spec Spec, src trace.Iteration) (owned, error) {
	out := owned{base: src.Base, patch: src.Patch}
	if out.base == "" {
		out.base = spec.Base
	}
	// A recovery that never finished restoring its evidence still owes it:
	// its own saved state, if any, does not stand for that work.
	if rec := spec.Recovery; rec != nil && rec.Patch != "" && !src.Restored {
		out.base, out.patch, out.digest = spec.Base, rec.Patch, rec.Digest
	}
	var inherited *run.Publication
	if spec.Recovery != nil {
		inherited = spec.Recovery.Publication
	}
	if !workflow.Publishes(spec.Workflow) || (src.Commit == "" && inherited == nil) {
		return out, nil
	}
	refuse := func(format string, a ...any) (owned, error) {
		return owned{}, fmt.Errorf("%w: "+format, append([]any{trace.ErrNotRecoverable}, a...)...)
	}
	repo, branch := spec.Workspace.Path, row.Branch
	if inherited != nil {
		branch = inherited.Branch
	}
	if branch == "" {
		return refuse("iteration %d committed %s but no branch is recorded for it", src.N, workspace.Short(src.Commit))
	}
	remote, err := workspace.RemoteHead(repo, branch)
	if err != nil {
		return refuse("origin could not be read to establish what was published (%v)", err)
	}
	pub := &run.Publication{Branch: branch}
	if inherited != nil {
		pub.PR, pub.Draft, pub.LatheDraft = inherited.PR, inherited.Draft, inherited.LatheDraft
	}
	// The head origin had before the source delivered anything.
	before := ""
	switch {
	case inherited != nil:
		before = inherited.Head
	case spec.Workflow == workflow.NameRevise:
		before = src.Base
	}
	switch {
	case src.Commit != "" && remote == src.Commit && remote != before:
		// The source delivered its commit; restoration reduces the saved
		// work to what lies beyond it.
		pub.Head, out.published = remote, remote
	case remote == before:
		// Nothing new reached origin; the source's work, committed or not,
		// is restored at its base.
		pub.Head = remote
	default:
		return refuse("origin/%s is at %s, not where lathe left it: it moved outside lathe", branch, workspace.Short(remote))
	}
	if pub.Head != "" {
		if err := adoptPR(dataRoot, row, repo, pub); err != nil {
			return refuse("%v", err)
		}
	}
	out.pub = pub
	return out, nil
}

// adoptPR finds the pull request for a delivered branch: the one recorded or
// inherited, else a unique match on the branch, and verifies it is open and
// comes from that branch in origin's repository at pub.Head.
func adoptPR(dataRoot string, row trace.Row, repo string, pub *run.Publication) error {
	url, draft, lathe := pub.PR, pub.Draft, pub.LatheDraft
	if url == "" {
		url, draft, lathe = prRecord(dataRoot, row)
	}
	var pr workspace.PR
	if url != "" {
		var err error
		if pr, err = workspace.ViewPR(repo, url); err != nil {
			return fmt.Errorf("the pull request could not be read (%v)", err)
		}
		draft = pr.Draft
	} else {
		found, ok, err := workspace.FindPR(repo, pub.Branch)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		// Discovered: nothing ties its creation to lathe, so a draft keeps
		// its draft status whatever lathe intended.
		pr, url, draft, lathe = found, found.URL, found.Draft, false
	}
	if err := workspace.OwnsPR(pr, pub.Branch, pub.Head); err != nil {
		return err
	}
	pub.PR, pub.Draft, pub.LatheDraft = url, draft, draft && lathe
	return nil
}

// residualOf is the saved work beyond own.published, written beside the
// source's reports, or "" when the source's work is wholly published. A
// missing or damaged patch is an error.
func residualOf(dataRoot string, row trace.Row, spec Spec, src trace.Iteration, own owned) (string, error) {
	if own.patch == "" {
		return "", fmt.Errorf("iteration %d's saved work is missing, so lathe cannot tell what lies beyond its published commit %s", src.N, workspace.Short(own.published))
	}
	if own.digest != "" {
		if got, err := workspace.PatchDigest(own.patch); err != nil || got != own.digest {
			return "", fmt.Errorf("saved patch %s is missing or changed since it was recorded", own.patch)
		}
	}
	dest := filepath.Join(ReportDir(dataRoot, row.ID, src.N), "residual.patch")
	residual, err := workspace.Residual(spec.Workspace.Path, own.base, own.patch, own.published, dest)
	if err != nil {
		return "", fmt.Errorf("iteration %d's saved work: %w", src.N, err)
	}
	if !residual {
		return "", nil
	}
	return dest, nil
}

// unrestored says what a restart leaves out.
func unrestored(n int, patch string) string {
	msg := fmt.Sprintf("restart: iteration %d's unpublished work is not restored", n)
	if patch != "" {
		msg += " (its saved work is kept at " + patch + ")"
	}
	return msg
}

// prRecord is the pull request the run's reports record, latest first: its
// URL, whether lathe opened it as a draft, and whether that draft was
// lathe's own for unaccepted work.
func prRecord(dataRoot string, row trace.Row) (url string, draft, lathe bool) {
	for i := row.Iteration; i >= 0; i-- {
		if pr, ok := workflow.ReadPRRecord(ReportDir(dataRoot, row.ID, i)); ok {
			return pr.URL, pr.Draft, pr.LatheDraft()
		}
	}
	return "", false, false
}

// recheck repeats, under the worker's claim, the publication checks the
// submission made: origin's branch is still at the head it found and the
// pull request is still open.
func recheck(repo string, pub *run.Publication) error {
	if _, err := workspace.CheckPublication(repo, pub.Branch, pub.Head, pub.PR); err != nil {
		return fmt.Errorf("%w; the queued recovery no longer matches it, and nothing was changed", err)
	}
	return nil
}

// ResumeOptions are a recovery submission's choices.
type ResumeOptions struct {
	Restart   bool
	Base      string // restart only: the ref to start from
	Overrides config.Overrides
}

// Capture resolves the configuration workflow's agents run with now.
type Capture func(workflow string, ov config.Overrides) (config.Snapshot, error)

// Resume is the one recovery submission; the CLI and the dashboard both call
// it. It inspects the run's latest iteration, then appends the recovery in a
// transaction that checks the same state again. A refusal appends nothing.
func Resume(db *trace.DB, dataRoot, runID, guidance string, o ResumeOptions, capture Capture) (int, error) {
	rec, err := PrepareRecovery(db, dataRoot, runID, guidance, o, capture)
	if err != nil {
		return 0, err
	}
	return db.Recover(runID, rec)
}

// CheckRecoverable checks what of row and its iterations its rules out for any
// recovery, without contacting origin. It returns the latest iteration and its
// parsed specification.
func CheckRecoverable(db *trace.DB, row trace.Row, its []trace.Iteration) (trace.Iteration, Spec, error) {
	if err := trace.Recoverable(row); err != nil {
		return trace.Iteration{}, Spec{}, err
	}
	if err := db.Stopped(row.ID, row.Iteration); err != nil {
		return trace.Iteration{}, Spec{}, err
	}
	n := row.Iteration
	if n >= len(its) {
		return trace.Iteration{}, Spec{}, fmt.Errorf("%w: iteration %d is not recorded", trace.ErrNotRecoverable, n)
	}
	src := its[n]
	var spec Spec
	if err := json.Unmarshal(src.Spec, &spec); err != nil || spec.Workflow == "" {
		return trace.Iteration{}, Spec{}, fmt.Errorf("%w: iteration %d records no readable specification", trace.ErrNotRecoverable, n)
	}
	if _, ok := workflow.Graphs[spec.Workflow]; !ok {
		return trace.Iteration{}, Spec{}, fmt.Errorf("%w: unknown workflow %q", trace.ErrNotRecoverable, spec.Workflow)
	}
	if err := spec.Workspace.Check(); err != nil {
		return trace.Iteration{}, Spec{}, err
	}
	if spec.Issue != "" && src.Resolved == "" {
		return trace.Iteration{}, Spec{}, fmt.Errorf("%w: iteration %d never resolved issue %s, so its task was not captured; lathe will not substitute the issue as it reads now. Submit a new run instead",
			trace.ErrNotRecoverable, n, spec.Issue)
	}
	return src, spec, nil
}

// PrepareRecovery checks that runID may be resumed and builds the recovery
// iteration to append, without writing anything.
func PrepareRecovery(db *trace.DB, dataRoot, runID, guidance string, o ResumeOptions, capture Capture) (trace.Recovery, error) {
	guidance = strings.TrimSpace(guidance)
	if guidance == "" {
		return trace.Recovery{}, errors.New("a recovery needs guidance on how to continue")
	}
	if o.Base != "" && !o.Restart {
		return trace.Recovery{}, errors.New("--base applies only to --restart; a recovery restores work at its recorded base")
	}
	row, err := runRow(db, runID)
	if err != nil {
		return trace.Recovery{}, err
	}
	its, err := db.Iterations(runID)
	if err != nil {
		return trace.Recovery{}, err
	}
	src, spec, err := CheckRecoverable(db, row, its)
	if err != nil {
		return trace.Recovery{}, err
	}
	n := row.Iteration

	// The task is the one the run resolved, never a refetched issue body.
	request := spec.Request
	if spec.Issue != "" {
		request = src.Resolved
	}

	rs := &RecoverySpec{Source: n, Guidance: guidance, Restart: o.Restart, Failure: src.Status + ": " + src.Reason}
	base := ""
	if workflow.Writes[spec.Workflow] {
		// Branch and pull request ownership, delivered or not, is settled
		// first, and carried into the recovery.
		own, err := reconcile(dataRoot, row, spec, src)
		if err != nil {
			return trace.Recovery{}, err
		}
		rs.Publication = own.pub
		if o.Restart {
			// Restoration evidence is never read: only ownership and the base.
			if base, err = restartPlan(spec, src, own, o.Base); err != nil {
				return trace.Recovery{}, err
			}
			rs.Unavailable = unrestored(n, own.patch)
		} else {
			var patch, digest, unavailable string
			if base, patch, digest, unavailable, err = restorePlan(dataRoot, row, spec, src, own); err != nil {
				return trace.Recovery{}, err
			}
			rs.Patch, rs.Digest, rs.Unavailable = patch, digest, unavailable
		}
	} else if o.Restart {
		return trace.Recovery{}, fmt.Errorf("%w: %s runs read the checkout and have no work to restart without", trace.ErrNotRecoverable, spec.Workflow)
	}

	roster, err := capture(spec.Workflow, o.Overrides)
	if err != nil {
		return trace.Recovery{}, err
	}
	b, err := Spec{
		Version: RecoveryVersion, Workflow: spec.Workflow, Request: request,
		Workspace: spec.Workspace, Roster: roster, Overrides: o.Overrides, Base: base, Recovery: rs,
	}.Marshal()
	if err != nil {
		return trace.Recovery{}, err
	}
	kind := trace.KindRecovery
	if o.Restart {
		kind = trace.KindRestart
	}
	return trace.Recovery{Source: n, Kind: kind, Resolved: request, Guidance: guidance, Spec: b}, nil
}

// restartHint is how an engineer continues run runID without the work that
// could not be restored.
func restartHint(runID string) string {
	return fmt.Sprintf("; continue without it with lathe resume --restart %s \"guidance\"", runID)
}

// restartPlan is the commit a restart starts from. A published branch is never
// rewritten from another base, so with one ref must name its head.
func restartPlan(spec Spec, src trace.Iteration, own owned, ref string) (string, error) {
	if own.pub == nil || own.pub.Head == "" {
		return restartBase(spec, src, ref)
	}
	base := own.pub.Head
	if ref != "" {
		sha, err := workspace.ResolveCommit(spec.Workspace.Path, ref)
		if err != nil {
			return "", err
		}
		if sha != base {
			return "", fmt.Errorf("%w: %s is published at %s; a restart from %s would rewrite it",
				trace.ErrNotRecoverable, own.pub.Branch, workspace.Short(base), workspace.Short(sha))
		}
	}
	return base, nil
}

// restorePlan is the commit a recovery restores the source iteration's work
// at, the saved patch and its digest to restore, and why no work is restored
// when none is. Only work beyond a published commit is restored, at it.
func restorePlan(dataRoot string, row trace.Row, spec Spec, src trace.Iteration, own owned) (base, patch, digest, unavailable string, err error) {
	if base = own.base; base == "" {
		return "", "", "", "", fmt.Errorf("%w: iteration %d records no base commit to restore its work at; resume with --restart --base <ref>", trace.ErrNotRecoverable, row.Iteration)
	}
	patch, recorded := own.patch, own.digest
	if own.published != "" {
		if patch, err = residualOf(dataRoot, row, spec, src, own); err != nil {
			return "", "", "", "", evidenceErr(err, row.ID)
		}
		base, recorded = own.published, ""
		if patch == "" {
			return base, "", "", fmt.Sprintf("iteration %d's work is already published at %s; it is validated again, not reapplied", row.Iteration, workspace.Short(base)), nil
		}
	}
	if patch, digest, unavailable, err = restorable(spec, src, patch, recorded); err != nil {
		return "", "", "", "", evidenceErr(err, row.ID)
	}
	return base, patch, digest, unavailable, nil
}

// evidenceErr refuses a recovery of run runID whose saved evidence err says is
// missing or damaged.
func evidenceErr(err error, runID string) error {
	return fmt.Errorf("%w: %v%s", trace.ErrNotRecoverable, err, restartHint(runID))
}

// restorable settles what the source iteration left to restore: a saved
// patch that is intact and touches no protected path, with its digest, or why
// nothing is restored. Anything else is missing or damaged evidence.
func restorable(spec Spec, src trace.Iteration, patch, recorded string) (savedPatch, digest, unavailable string, err error) {
	switch {
	case patch != "":
		if _, err := os.Stat(patch); err != nil {
			return "", "", "", fmt.Errorf("iteration %d's saved patch %s is missing", src.N, patch)
		}
		digest, err := workspace.PatchDigest(patch)
		if err != nil {
			return "", "", "", err
		}
		// Evidence an earlier recovery recorded is trusted only unchanged.
		if recorded != "" && digest != recorded {
			return "", "", "", fmt.Errorf("saved patch %s changed since it was recorded", patch)
		}
		if _, err := workspace.PatchPaths(spec.Workspace.Path, patch, spec.Roster.Protected); err != nil {
			return "", "", "", err
		}
		return patch, digest, "", nil
	case src.Worktree == "" || src.Preservation == trace.PreservedUnstarted:
		return "", "", fmt.Sprintf("iteration %d never created a worktree, so it wrote nothing", src.N), nil
	case src.Preservation == trace.PreservedEmpty:
		return "", "", fmt.Sprintf("iteration %d's cleanup exported its worktree and found no changes", src.N), nil
	case src.Preservation == trace.PreservedMissing:
		return "", "", "", fmt.Errorf("iteration %d's worktree was gone before its work could be saved, so that work is unavailable", src.N)
	default:
		// Removal alone, as older records show it, does not say the
		// worktree was empty.
		return "", "", "", fmt.Errorf("iteration %d's worktree is %s with no record that its work was saved or found empty", src.N, cmp.Or(src.WorktreeState, "in an unknown state"))
	}
}

// restartBase freezes the commit a restart starts from: ref when given, else
// the recorded base. A revision continues from its pull request's head, so a
// different base is refused.
func restartBase(spec Spec, src trace.Iteration, ref string) (string, error) {
	recorded := src.Base
	if recorded == "" {
		recorded = spec.Base
	}
	if ref == "" {
		if recorded == "" {
			return "", fmt.Errorf("%w: iteration %d records no base commit; restart with --base <ref>", trace.ErrNotRecoverable, src.N)
		}
		return recorded, nil
	}
	sha, err := workspace.ResolveCommit(spec.Workspace.Path, ref)
	if err != nil {
		return "", err
	}
	if spec.Workflow == workflow.NameRevise && sha != recorded {
		return "", fmt.Errorf("%w: a revision continues from its pull request's head %s; --base cannot move it", trace.ErrNotRecoverable, workspace.Short(recorded))
	}
	return sha, nil
}

// recoveryOf is the run.Recovery a worker executes with, and the earlier
// iterations' report directories.
func recoveryOf(db *trace.DB, dataRoot string, row trace.Row, rs *RecoverySpec) (*run.Recovery, error) {
	prior, err := priors(db, dataRoot, row)
	if err != nil {
		return nil, err
	}
	return &run.Recovery{Guidance: rs.Guidance, Source: rs.Source, Restart: rs.Restart, Failure: rs.Failure,
		Unavailable: rs.Unavailable, Publication: rs.Publication, Prior: prior}, nil
}
