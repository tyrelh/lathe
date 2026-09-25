# Revisions extend the run

A build ends at a pull request, and a person reviewing it usually wants one more change. Before this, that meant a new build: a new run, a new branch, a new pull request, and the reviewer's context split across two of each. `lathe revise <run-id> "<change>"` instead adds an iteration to the run that opened the pull request and pushes to the same branch.

## One run, many iterations

The run keeps its ID, its pull request, its phase history and its spend. An iteration is one submitted request and what became of it; a worker attempt is still one execution of it, and the two stay apart in storage and in the lifecycle code. The run row follows its latest iteration for status, reason and end, and keeps its original creation and first start. Its latest submission time is separate, and is what orders the queue and the run lists, so a revised run moves to the top without pretending it was created then.

Phase numbers carry on across iterations and are never reused, because a phase ID embeds its number. Each revision's reports go in `iteration-n/` beside the run's own, so a new plan cannot overwrite an old one. Usage was always accumulated by run, so costs stay cumulative for free; the one place that was not — the completion banner, which printed the execution's own counters — now reads the run's totals.

Every lifecycle write mirrors the run's state onto its current iteration inside the same transaction, so the two cannot disagree. Worker writes to phases, the shipped branch and commit, and an iteration's evidence are bound to the attempt that owns the run while it is executing: a stale worker writes nothing.

Existing runs became their own iteration 0 in the migration, from exactly what their rows recorded. An old build can be revised when its records name the pull request, the branch and the shipped commit. Nothing missing is inferred from the pull request as it is now: its URL alone does not say the branch is still what lathe left there.

## Eligibility is strict and checked twice

A revision needs the latest iteration to have succeeded, the pull request open, and the checkout clean, on the pull request's branch, with it and origin both at the commit lathe last shipped. Any failed, cancelled or lost iteration ends revisions, even after a successful build. Lathe does not switch branches, merge, rebase or recover work to get there; it says what to restore.

The checks run at submission and again when the worker starts, before any agent. Acceptance re-verifies the run's state, appends the iteration and requeues the run in one transaction that also enforces checkout exclusivity, so two submitters cannot both be accepted. There are no idempotency keys yet: a retry while the first revision is live is refused, one after it finished can add another iteration.

## Only accepted work is published

Build publishes an unaccepted change as a draft pull request. A revision does not: its pull request is already open for review, and pushing unaccepted work to it would present that work as finished. Unaccepted work, and cancellation, stay uncommitted in the checkout with their reports. A request that needs no code change is validated as the branch stands and succeeds without a commit, which leaves the run eligible for another.

Publication checks that the pull request is open and origin is still at the expected commit, pushes with `--force-with-lease` on that commit, and then asks origin what arrived. That covers a lost push acknowledgement. It cannot cover a pull request closed or merged between the check and the push, since git cannot see that, so the state is read again afterwards and the iteration fails if it changed, saying whether the commit was pushed. That narrow race is accepted for now.
