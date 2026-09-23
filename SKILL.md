---
name: lathe
description: Run a bounded, traced agent workflow against the current repository. Use when asked to scout, map, or investigate a codebase with lathe, to plan or implement a change with lathe, to build a change through to a pull request, or when the user says "lathe".
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

- `implement "<request>"` — plan, implement and test a change in a clean Git
  repository. The builder gets write and edit tools, no shell, and may write only
  the plan's files. Changes remain uncommitted. A missed file ends the run as a
  failure naming the needed path. The tester discovers a command; lathe runs it
  under the tester timeout and shell deny list with your inherited environment. A
  red exit gets up to four fix rounds in the builder's same session. Test dirt
  outside the accumulated plan scope is reverted. Review with `git diff`,
  including after failure: planned changes remain in the tree.

- `build "<request>"` — the same run, then branch, commit and open a pull
  request. It needs `git` and `gh` on `PATH` and `gh` already authenticated.
  Each of the three phases is an agent judging text and lathe performing the
  git: the brancher names the branch, the committer writes the commit message,
  and the pr-author writes the title and body, while lathe runs `git checkout
  -b`, stages exactly the accepted scope and commits it, pushes, and runs
  `gh pr create`. No agent can commit or push: the guard denies both. None of
  the three sends work back, so a failure in any of them ends the run.

The plan, implement and build workflows share a read-only review loop:

```
request → plan → review → [plan → review, up to four send-backs]
plan:      → print the reviewed plan
implement: → implement → test → [implement → test, up to four fixes]
build:     → branch → implement → test → [implement → test] → commit → pr
```

The reviewer checks the plan against the repository, including the builder's
file list. Empty feedback accepts it. The fifth review ends the loop; remaining
objections or a failed final review become risks in the saved plan and builder
handoff. Failed reviews consume a send-back and remain visible as failed phases
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
reports needed files); `<dir>/test.json` records the discovered command and
observations; `<dir>/pr.json` holds the pull request a build opened, including
its URL. Runs from before the rename have `build.json` where `implement.json`
now is. There is no `branch.json` or `commit.json`: the branch name and the
commit sha are both in git and in the trace. `<dir>/<attempt>/raw.jsonl` is the
full event stream. `lathe runs` lists recent runs from every repo, queued and
running ones included.

`lathe show <id>` prints one run's state, outcome and report locations,
`lathe wait <id>` blocks until it is terminal and exits with its outcome, and
`lathe cancel <id>` asks it to stop. Both `show` and `wait` take `--json`.
