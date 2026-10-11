# Writing runs use worktrees

## Context

`implement`, `build` and `revise` used to run in the person's checkout. Only one could be outstanding per repository, each needed a clean tree, and the person could not touch the repository while it ran. ADRs 0003–0005 assumed that model.

## Decision

Each writing iteration runs in a detached git worktree of the source checkout at `<data>/worktrees/<run-id>/iteration-<n>`, created at a recorded base commit: HEAD at submission, or for a revision the commit lathe last pushed. The checkout is never changed. It may be dirty, on another branch, or both, and several writing runs for one repository run at once, up to `LATHE_CAPACITY`. Read-only runs (`scout`, `plan`) still read the checkout and wait for it.

The worker reserves and locks the worktree, not the checkout. It records the worktree's path and base together before creating it, so a crash at any point leaves recovery enough to export what is there.

### Handing results back

`implement` leaves its change uncommitted in the worktree; when the run stops, lathe saves it as `result.patch` in the run directory (the iteration's report directory for a revision). The patch is taken against the base with a temporary index, so neither index changes: commits, edits, deletions, new files git does not ignore, binary content and modes are included; ignored files and protected paths are not. Apply it from a checkout at the base commit with `git apply --index <patch>`. `lathe show` and the dashboard print its path and base. A patch from a failed, cancelled or lost run is labelled unaccepted. An export that finds nothing removes any earlier patch.

Commits that no branch, remote-tracking branch or tag reaches — a revision's unpushed commit, for one — are kept under `refs/lathe/runs/<id>/iteration-<n>` before the worktree is removed.

The worktree is removed only after both succeed. If the export fails, or the worktree exists but its base was never recorded, it is kept and recorded as pending with the reason, and the run does not succeed.

### Gated subprocess admission

Every subprocess of a writing run — Pi, its auth check, and lathe's own git and gh — starts through a supervisor helper (a hidden mode of the lathe binary) in a process group of its own. The worker records an intent before starting the helper. The helper reports its PID, group and a loopback stop endpoint guarded by a random token, then waits. The worker records that registration, and only then sends GO, after which the helper starts the payload in the requested directory. An intent without a registration therefore proves that nothing ran. A registration is refused once the run is no longer the attempt's, which is how a lost run's late launches are revoked.

The helper holds a private control pipe from the worker for the whole invocation; neither control descriptor reaches the payload. EOF on it (the worker died), cancellation, or an authenticated stop request makes the helper kill its whole group. The payload's output and exit status, including 126 and 127, come back unchanged. A launch is recorded drained only once its group has no running member; exited members not yet reaped do not count. The `pi_pid` event stays diagnostic.

### Lost runs

On startup the manager settles what the last one left, before it admits anything. A worker confirmed gone is marked lost first, which revokes its attempt. Its unregistered intents are cancelled, since none admitted a payload. Each registered supervisor is asked to stop through its endpoint, and its group is waited for; the worker's death has usually closed the control pipe and ended it already. Then, under the worktree's lock, the result is exported, unreachable commits kept, and the worktree and its entry removed with `git worktree remove`; no other worktree entry of the repository is pruned. Pending cleanup from earlier failures is retried the same way. A group that will not stop, a held lock, or a failed export or removal leaves the worktree recorded pending and holds back that repository's queued runs, with the reasons, until it is resolved and the manager restarted; other repositories dispatch as usual, and nothing is deleted on a guess. Cleaning up an earlier iteration of a run that is executing again leaves the live attempt's launches alone. A worker that is still alive keeps its run and its worktree.

### Compatibility

Specifications are version 4, which records the base. Queued version 3 writing runs are refused after their claim and before any reservation, git or agent, with a message to resubmit: lathe will not guess their base from today's HEAD. Version 3 scout and plan runs still execute. Each attempt records its execution mode (checkout, worktree or reject); attempts from before this one are checkout executions, and a legacy worker still running at upgrade keeps the checkout and is not relaunched. A successful build from before this change can still be revised from its recorded pull request, branch and commit.

## Consequences

- Isolation is a working directory, not a sandbox. The guard, protected paths and `permit.Enforce` behave as before inside the worktree; nothing stops a tool from reading or writing elsewhere, and a deliberately daemonised process can leave its group. A container around Pi is the follow-up behind the `Launcher` seam.
- Worktrees share refs, objects and configuration with the checkout. A build's branch appears in the person's repository; a revision never moves their local branch. Pushes set no upstream, so concurrent builds do not contend for the shared configuration, and a fetch that finds its remote-tracking ref locked by another retries. Submodules are not initialised, sparse checkout and LFS follow the repository's own configuration, and checkout hooks run in the worktree.
- Shared Go caches carry over; `node_modules` and other ignored, checkout-local dependencies do not, so a fresh worktree pays a cold start. Each concurrent worktree costs a full working tree of disk; there is no retention limit yet.
- The data directory, database and run directories are owner-only, because the database holds supervisor stop tokens and patches hold source.
