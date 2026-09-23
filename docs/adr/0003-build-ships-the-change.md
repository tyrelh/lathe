# Build ships the change

A run used to end at a dirty working tree. It planned, implemented and tested a
change, and then handed the engineer a pile of uncommitted files to review,
branch, commit and open a pull request for by hand. Those last three steps are
the same kind of judgement the rest of the factory already gives to agents: a
branch name follows the repository's convention, a commit message follows its
history, and a pull request follows its template and its recently merged ones.

So `build` now runs `branch`, `commit` and `pr` as well, and the workflow that
stops at a tested working tree is called `implement`.

## Why the wider workflow took the name

Building a change, to an engineer, ends at a pull request; implementing one ends
at code that works. The name that reaches for the larger amount of authority is
the one people already reach for, so making `build` mean the smaller thing would
have left everyone typing the wrong word.

The inner node was renamed with it. A node called `build` inside a workflow
called `build` that now means something wider is the kind of thing someone
misreads in a trace at 3am. The agent keeps its name: `builder` is a role, and
renaming it would churn the prompt directories and the system prompt text for
nothing.

```
implement: request -> plan -> review [-> plan] -> implement -> test [-> implement]
build:     request -> plan -> review [-> plan] -> branch -> implement -> test [-> implement] -> commit -> pr
```

`branch` sits immediately after the plan nodes, so it runs whether the reviewer
accepted the plan or the send-back budget ran out and the objections became
risks. `commit` and `pr` sit after `test`, which only forwards on a measured
green exit. The graph gives both placements for free; neither needs a condition.

The alternative was a `--pr` flag on the existing `build`. The roster is
captured per workflow name, so a flag would have had to travel through `Spec`
for the agent list to vary, and the CLI would still have had one word covering
two different amounts of authority. A run that pushes a branch and opens a pull
request should not be reachable by a flag on the command that does not.

## Why judgement is split from execution

In each of the three phases the agent judges the text and lathe performs the
git. The agent reads the repository's conventions and returns a branch name, a
commit message, or a title and body; lathe runs `git checkout -b`, `git add`
and `git commit`, `git push` and `gh pr create` itself.

Three things follow.

There is nothing left to gate. The rule elsewhere is that discovery is a claim
and the exit code is the measurement, and here it becomes trivially true: lathe
performed the action, so no report can lie about having performed it. The only
checks left are on the proposed text, before it is used — `BranchUsable` asks
git whether the name can be created while the agent still has a session to
correct in, and before a single file has been written.

The shell deny list stays intact. `git push` is denied to every agent, and
lathe's own push runs through `exec.Command` rather than `Handle.Command`, so it
never meets that list. `git commit` joined the list for the same reason: the
guard now enforces "lathe commits, agents do not" instead of a prompt asking for
it. No per-agent override, and no new configuration surface.

Staging is exact. Lathe stages the paths `permit.Changed` reports, which after
enforcement is the accumulated accepted scope. An agent's own `git add -A` would
sweep up anything the guard tolerated.

The cost is shell access. Three more agents need `bash` to read `git log`,
`git branch -r` and `gh pr list`, which widens shell access from one agent to
four. That is the price of the split, and it is the thing to revisit first if
the boundary ever has to get tighter.

## What the rename could have broken

A run queued as `build` before the upgrade meant implement only; after it,
`build` opens a pull request. `worker.SpecVersion` went from 1 to 2, and a
worker already refuses a specification it does not recognise, so a stale queued
run fails loudly instead of pushing a branch nobody asked for.

The duplicate-run lock guarded on a literal `'build'`, in Go and again in SQL.
With two writing workflows the exclusion has to cover both, in both directions.
`Request.Exclusive` is set from `workflow.Writes`, and the query reads an
additive `exclusive` column, which takes workflow names out of `internal/trace`
altogether — better than widening the literal to `IN ('implement', 'build')`,
which would have had to be widened again by the next writing workflow.

There is no history backfill. Run IDs embed the workflow name, so rewriting old
rows to `implement` would leave them disagreeing with their own IDs. Old runs
keep the name they were recorded under, and `lathe show` still reads their
`build.json`.

## When something fails late

A failure after `commit` keeps the commit on its branch and fails the run saying
so, so the work is not lost. The checkout is left on the new branch either way.
Both facts are in the banner `lathe build` prints about owning the checkout, and
in `SKILL.md`.

## Since 0004

`commit` and `pr` now sit after `adjudicate`, not `test`. An accepted change ships as before. An unaccepted one — findings unresolved after four repairs, or validation incomplete — is still committed and opened as a draft pull request carrying the validation report, and the run fails: the draft hands unfinished work to a person, it does not claim it passed. Cancellation, a source change during validation and any other error still end the run before anything is committed.
