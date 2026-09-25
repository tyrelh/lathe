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

## Workflows

- `scout "<request>"` — investigate and report. Read-only: the scout has
  `read`, `grep`, `find` and `ls`, no shell and no write tool, so it cannot
  change the repo it is pointed at.
- `plan "<request>"` — plan a change: a summary, ordered steps, the files the
  change touches, and the risks. Read-only on the same terms as the scout. The
  file list is the write scope the builder is held to, so a plan that
  names `.git`, `.env*` or key material is rejected before it is printed.

- `implement "<request>"` — plan, implement and validate a change in a clean Git repository. The builder gets write and edit tools, no shell, and may write only the plan's files. Changes remain uncommitted. A missed file ends the run as a failure naming the needed path. Each implementation round is validated by four workers at once: the tester reassesses the test command and lathe runs it under the command timeout and shell deny list with your inherited environment, while three read-only reviewers check correctness, security and unnecessary complexity. An adjudicator then weighs every report and either accepts or sends the builder back with one set of changes, up to four times. Acceptance needs a measured green suite and a usable report from every worker. Work still unaccepted after the fourth repair, or whose validation could not complete, stays in the tree and the run fails saying why. Test dirt outside the accumulated plan scope is reverted once every worker has finished; a source file changed while the workers ran invalidates the round and stops the run. Review with `git diff`, including after failure: planned changes remain in the tree.

- `build "<request>"` — the same run, then branch, commit and open a pull
  request. It needs `git` and `gh` on `PATH` and `gh` already authenticated.
  Each of the three phases is an agent judging text and lathe performing the
  git: the brancher names the branch, the committer writes the commit message,
  and the pr-author writes the title and body, while lathe runs `git checkout
  -b`, stages exactly the accepted scope and commits it, pushes, and runs
  `gh pr create`. No agent can commit or push: the guard denies both. None of
  the three sends work back, so a failure in any of them ends the run. An accepted change opens a normal pull request. Unresolved findings at the send-back limit, or incomplete validation, still commit and open a draft pull request that carries the validation report, and the run fails: the draft is unfinished work, not an accepted change. Cancellation and a source change during validation publish nothing.

- `revise <run-id> "<change>"` — another change to a build's open pull request, as the next iteration of the same run. It needs the build's latest iteration to have succeeded, the pull request still open, and the checkout clean, on the pull request's branch and at the commit lathe last pushed, with origin at that commit too. Lathe refuses rather than switching branches, merging or rebasing, and says what to restore; a manual commit or an accepted GitHub suggestion makes the run ineligible until the branch is back where lathe left it. The request must spell out the change: review comments are not collected. Every revision enters at the planner, which is shown the original request, earlier revisions, the previous plan and validation, and the branch diff. A request that needs no code change is validated as the branch stands and succeeds without a commit. Only an accepted change is committed and pushed — the push is refused if origin moved since the last check — and the pull request's title and description are left alone. Anything unaccepted, and cancellation, keep the work uncommitted in the checkout and publish nothing; that also ends the run's revisions. `revise` waits for the iteration it submitted; `--detach` returns straight away. There are no idempotency keys: if a submission's acknowledgement is lost, check `lathe show <id>` before resubmitting.

The plan, implement, build and revise workflows share a read-only review loop:

```
request → plan → review → [plan → review, up to four send-backs]
plan:      → print the reviewed plan
implement: → implement → validate → adjudicate → [implement → validate → adjudicate, up to four repairs]
build:     → branch → implement → validate → adjudicate → [...] → commit → pr (draft when unaccepted)
revise:    → implement → validate → adjudicate → [...] → commit → publish (accepted only)
```

`validate` runs `test`, `code-review-general`, `code-review-security` and `code-review-slop` at once, each traced as its own phase. A worker that fails outright is retried up to twice against the same code without spending a repair; one that still fails leaves validation incomplete.

The reviewer checks the plan against the repository, including the builder's file list. Empty feedback accepts it. The fifth review ends the loop; remaining objections or a failed final review become risks in the saved plan and builder handoff. An objection the reviewer marks blocking, because the plan cannot succeed as written, is the exception: if it survives the fifth review the run fails before the builder starts, with the plan saved for diagnosis. Failed reviews consume a send-back and remain visible as failed phases
even when the run succeeds. Planner failures and cancellation stop the run.

## Writing the request

One concrete question or one concrete change, in one sentence. The agent answers
what it was asked and stops, so "where is authentication handled and what calls
it" gets a usable answer where "review the codebase" gets a shallow one, and
"add retry with backoff to the fetch client" gets a usable plan where "improve
error handling" gets a vague one.

## Flags

Before the request, not after:

- `--issue <number|URL|owner/repo#number>` use a GitHub issue instead of a prompt for `plan`, `implement`, or `build`; bare numbers resolve against the target checkout. The worker fetches the title and body with authenticated `gh` in a traced `issue` phase before planning and saves `issue.json`. Comments are not included.
- `--repo <dir>` act on another repository
- `--detach` record the request, print the run ID, exit 0
- `--model`, `--provider`, `--thinking` override the roster for one run
- `lathe.toml` at the target's root sets the same three keys for every agent, or per agent under `[agents.<name>]`; flags beat it, and a malformed file is ignored with a warning

`implement` and `build` need a clean checkout and own it until the run stops: do
not edit files or switch branches there meanwhile. A second outstanding run of
either for the same checkout is rejected.

A build leaves the checkout on the branch it created, whether it succeeded or
failed. A failure after its commit keeps that commit on the branch and says so
in the run's reason, so the work is never lost — check `git log` before assuming
a failed build produced nothing.

## Reading the result

The run prints its status, spend and directory. `<dir>/result.json` is a scout's
structured report and `<dir>/plan.json` is a planner's, which `lathe plan` also
prints; `<dir>/implement.json` is the builder's report (also saved when it
reports needed files); `<dir>/validation.json` holds every validation round — the tester's report, lathe's measurement, each reviewer's findings, the adjudicator's decisions, any scope or plan amendments — and how validation ended (`accepted`, `unresolved`, `incomplete` or `invalidated`); `<dir>/pr.json` holds the pull request a build opened, including its URL and whether it is a draft of unaccepted work. Runs from before parallel validation have `test.json` instead. Runs from before the rename have `build.json` where `implement.json`
now is. There is no `branch.json` or `commit.json`: the branch name and the
commit sha are both in git and in the trace. `<dir>/<attempt>/raw.jsonl` is the
full event stream. `lathe runs` lists recent runs from every repo, queued and
running ones included.

`lathe show <id>` prints one run's state, outcome and report locations,
`lathe wait <id>` blocks until it is terminal and exits with its outcome, and
`lathe cancel <id>` asks it to stop. Both `show` and `wait` take `--json`.

A revised run has several iterations. Its tokens and cost are always the whole run's. `lathe show <id>` lists every iteration — request, outcome, the commit it made and where origin was — and prints the latest iteration's reports, or the previous iteration's, labelled, while the latest is still queued or running; `--iteration n` picks one. Iteration 0's reports are in the run directory as always; iteration n's are in `<dir>/iteration-n/`. `lathe wait --iteration n <id>` exits with that iteration's outcome even if another has been submitted since; plain `lathe wait <id>` follows the run's latest.
