---
name: lathe
description: Run a bounded, traced agent workflow against the current repository. Use when asked to scout, map, or investigate a codebase with lathe, to plan or implement a change with lathe, to build a change through to a pull request, to revise that pull request with another change, or when the user says "lathe".
---

# lathe

`lathe` runs phased agent work against whatever repository you are sitting in
and traces every phase, tool call and dollar to SQLite. It is on `PATH`; run it
from inside the target repo, which it discovers itself.

```
lathe <workflow> "<request>"
```

Sequencing is the binary's job. Do not orchestrate phases by hand, and do not
substitute your own investigation for a workflow the user asked for — the point
of `lathe` is the trace it leaves behind.

The command records the run and blocks until it finishes, printing the report.
The work happens in a separate worker process, so the run survives the command
being interrupted; that detaches rather than cancels. `--detach` returns the
run ID immediately instead. A manager process drains the queue and is started
automatically if none is running.

## Worker credentials

Configure credentials for every provider in the captured agent roster, including those used only by later agents. Workers inherit the manager's environment: export provider keys before starting it (for example `MOONSHOT_API_KEY`), or log in with Pi as the worker user. Restart the manager after changing its environment. From the repository in that same environment, rerun readiness with `pi auth check --provider <provider> --json`.

`plan`, `implement`, `build` and `revise` trace an engineer-owned `auth` phase immediately after `request`, before optional issue lookup or any agent turn. Each distinct captured provider is checked once, with safe status/reason logging, under the command timeout and run cancellation. Missing credentials, unsupported Pi or malformed readiness output stop the run before planning. Auth is not repeated on planning send-backs; scout is unchanged. Readiness means credential availability, not a live API-key validity guarantee. Pi may refresh expired OAuth credentials and update its credential store.

## Workflows

- `scout "<request>"` — investigate and report. Read-only: the scout has
  `read`, `grep`, `find` and `ls`, no shell and no write tool, so it cannot
  change the repo it is pointed at.
- `plan "<request>"` — plan a change: a summary, ordered steps, the files the change touches, and the risks. Read-only on the same terms as the scout, plus `web_read` for public docs, with reads held to the repo. The file list is the write scope the builder is held to, so a plan that names `.git`, `.env*` or key material is rejected before it is printed.

- `implement "<request>"` — plan, implement and validate a change in a worktree of its own, at the commit the checkout was on when you submitted; the checkout may be dirty and is never changed. The builder gets write and edit tools, no shell, and may write only the plan's files. The change is saved as `result.patch` in the run directory when the run stops; `lathe show <id>` prints its path and base, and `git apply --index <patch>` applies it from a checkout at that base. A builder that needs a file outside the plan sends a scope request back to the planner (see below). Each implementation round is validated by five workers at once: the tester reassesses the test command, runs it under the shell deny list with your inherited environment and reports its exit code, while four read-only reviewers check correctness, security and unnecessary complexity, and try to break the change. An adjudicator then weighs every report and either accepts or sends the builder back with one set of changes, up to ten times. Acceptance needs a green suite and a usable report from every worker. Work still unaccepted after the tenth repair, or whose validation could not complete, stays in the tree and the run fails saying why. Test dirt outside the accumulated plan scope is reverted once every worker has finished; a source file changed while the workers ran invalidates the round and stops the run. Review the patch, including after failure: partial work is saved the same way and labelled unaccepted.

- `build "<request>"` — the same run, then branch, commit and open a pull
  request. It needs `git` and `gh` on `PATH` and `gh` already authenticated.
  Each of the three phases is an agent judging text and lathe performing the
  git: the brancher names the branch, the committer writes the commit message,
  and the pr-author writes the title and body, while lathe runs `git checkout
  -b`, stages exactly the accepted scope and commits it, pushes, and runs
  `gh pr create`. No agent can commit or push: the guard denies both. None of
  the three sends work back, so a failure in any of them ends the run. An accepted change opens a normal pull request. Unresolved findings at the send-back limit, or incomplete validation, still commit and open a draft pull request that carries the validation report, and the run fails: the draft is unfinished work, not an accepted change. Cancellation, a source change during validation, and terminal environment failures publish nothing.

- `revise <run-id> "<change>"` — another change to a build's open pull request, as the next iteration of the same run. It needs the build's latest iteration to have succeeded, the pull request still open from its branch, and origin's branch at the commit lathe last pushed; the local checkout does not matter. It runs detached at that commit in a worktree of its own, so no local branch moves. Lathe refuses rather than merging or rebasing, and says what to restore; a push or an accepted GitHub suggestion makes the run ineligible until origin's branch is back where lathe left it. The request must spell out the change: review comments are not collected. After request and auth preflight, every revision enters the planner, which is shown the original request, earlier revisions, the previous plan and validation, and the branch diff. A request that needs no code change is validated as the branch stands and succeeds without a commit. Only an accepted change is committed and pushed — the push is refused if origin moved since the last check — and the pull request's title and description are left alone. Anything unaccepted, and cancellation, save the work as the iteration's result patch and publish nothing; that also ends the run's revisions. `revise` waits for the iteration it submitted; `--detach` returns straight away. There are no idempotency keys: if a submission's acknowledgement is lost, check `lathe show <id>` before resubmitting.

The plan, implement, build and revise workflows share a read-only review loop:

```
request → auth → [issue] → [route-plan] → plan → review → [plan → review, up to eight send-backs]
plan:      → print the reviewed plan
implement: → [route-build] → implement → validate → adjudicate → [implement → validate → adjudicate, up to ten repairs; scope request → plan → review, up to two times]
build:     → branch → [route-build] → implement → validate → adjudicate → [...] → commit → pr (draft when unaccepted)
revise:    → [route-build] → implement → validate → adjudicate → [...] → commit → publish (accepted only)
```

`validate` runs `test`, `code-review-general`, `code-review-security`, `code-review-slop` and `code-review-adversarial` at once, each traced as its own phase. An ordinary worker failure is retried up to twice against the same code without spending a repair; one that still fails leaves validation incomplete. Reported test exits 126/127 are the exception: they produce terminal `environment_failed`, with one tester attempt, no adjudication or repairs, and no commit, push or PR, including drafts. Already-running reviewers finish before cleanup. The tester report, with its command, exit code and output, remains in `validation.json`. Install the required executable, check the worker's `PATH` and executable permissions, then rerun. This policy applies even if the suite uses those statuses for another purpose. Other nonzero test results remain red suites for adjudication, not worker failures. `environment_failed` is not retryable and must not be treated as publishable `incomplete` validation.

The reviewer checks the plan against the repository, including the builder's file list. Empty feedback accepts it. The ninth review ends the loop; remaining objections or a failed final review become risks in the saved plan and builder handoff. An objection the reviewer marks blocking, because the plan cannot succeed as written, is the exception: if it survives the last review the run fails before the builder starts, with the plan saved for diagnosis. Failed reviews consume a send-back and remain visible as failed phases
even when the run succeeds. Planner failures and cancellation stop the run.

A builder that needs a file the plan does not allow stops and reports it in `needed`; the plan goes back to the planner and reviewer (a scope request), then the builder resumes in the same session with the revised plan, skipping branch creation and routing. Two scope send-backs are allowed per iteration; the third request fails the iteration without publishing anything. Each request is saved as `scope-<n>.json` and each revised plan as `plan-<n>.json`. Malformed paths fail at once; protected paths are never granted.

A failed, cancelled or lost run can be continued with `lathe resume <id> "guidance"` (guidance required; `--detach` and `--json` work as for other commands). It appends a recovery iteration to the same run: the saved `result.patch` is restored into a fresh worktree at the recorded base, then planning, review, implementation and full validation start again with the earlier reports and your guidance. `--restart [--base <ref>]` starts over without restoring that work. Recovery is refused while any worker, launch, reservation or cleanup of the run is unsettled, when the saved work is missing or damaged (use `--restart`), when an issue's task was never captured, and when the failed iteration already made a commit, which may be published. `lathe show` prints scope requests and whether the run may be resumed.

## Writing the request

One concrete question or one concrete change, in one sentence. The agent answers
what it was asked and stops, so "where is authentication handled and what calls
it" gets a usable answer where "review the codebase" gets a shallow one, and
"add retry with backoff to the fetch client" gets a usable plan where "improve
error handling" gets a vague one.

## Flags

Before the request, not after:

- `--issue <number|URL|owner/repo#number>` use a GitHub issue instead of a prompt for `plan`, `implement`, or `build`; bare numbers resolve against the target checkout. The worker fetches the title and body with authenticated `gh` in a traced `issue` phase after auth preflight and before planning and saves `issue.json`. Comments are not included.
- `--repo <dir>` act on another repository
- `--detach` record the request, print the run ID, exit 0
- `--model`, `--provider`, `--thinking` override every agent's configuration for one run, and turn routing off
- agent configuration lives in `~/.lathe/agents/<name>/{config.yaml,system.md,user.md}` and `~/.lathe/routing/`; it is the only source, a missing or invalid file refuses the submission with the file named, and a target repository's `lathe.toml` is ignored

`implement`, `build` and `revise` run in their own worktrees under
`~/.local/share/lathe/worktrees` and never change the checkout, which may be
dirty or on another branch; several may run at once for one repository, up to
`LATHE_CAPACITY`. Worktrees are removed when a run stops, after its result is
saved; commits nothing else reaches are kept under
`refs/lathe/runs/<id>/iteration-<n>`. A fresh worktree has the shared Go caches
but not `node_modules` or other ignored files. It is not a sandbox.

A build creates its branch as the brancher's name plus the run ID's random
suffix, and leaves your checkout where it was, whether it succeeded or
failed. A failure after its commit keeps that commit on the branch and says so
in the run's reason, so the work is never lost — check `git log` before assuming
a failed build produced nothing.

## Reading the result

The run prints its status, spend and directory. `<dir>/result.json` is a scout's
structured report and `<dir>/plan.json` is a planner's, which `lathe plan` also
prints; `<dir>/implement.json` is the builder's report (also saved when it
reports needed files); `<dir>/validation.json` holds every validation round — the tester's report and exit code, each reviewer's findings, the adjudicator's decisions, any scope or plan amendments — and how validation ended (`accepted`, `unresolved`, `incomplete`, `invalidated` or `environment_failed`); `<dir>/pr.json` holds the pull request a build opened, including its URL and whether it is a draft of unaccepted work. Runs from before parallel validation have `test.json` instead. Runs from before the rename have `build.json` where `implement.json`
now is. There is no `branch.json` or `commit.json`: the branch name and the
commit sha are both in git and in the trace. `<dir>/<attempt>/raw.jsonl` is the
full event stream. `lathe runs` lists recent runs from every repo, queued and
running ones included.

`lathe show <id>` prints one run's state, outcome and report locations,
`lathe wait <id>` blocks until it is terminal and exits with its outcome, and
`lathe cancel <id>` asks it to stop. Both `show` and `wait` take `--json`.

A revised run has several iterations. Its tokens and cost are always the whole run's. `lathe show <id>` lists every iteration — request, outcome, the commit it made and where origin was — and prints the latest iteration's reports, or the previous iteration's, labelled, while the latest is still queued or running; `--iteration n` picks one. Iteration 0's reports are in the run directory as always; iteration n's are in `<dir>/iteration-n/`. `lathe wait --iteration n <id>` exits with that iteration's outcome even if another has been submitted since; plain `lathe wait <id>` follows the run's latest.
