# Validation runs in parallel and is adjudicated

The test loop used to be the only check on an implementation: the tester discovered a command, lathe measured it, and a red exit sent the builder back up to four times. Nothing read the code for correctness against the request, for security, or for complexity it did not need. This adds three read-only code reviewers — `code-review-general`, `code-review-security` and `code-review-slop` — that run alongside the tester, and an adjudicator that decides what the builder does next.

```
implement: … → implement → validate → adjudicate [→ implement, up to four repairs]
build:     … → branch → implement → validate → adjudicate [→ implement] → commit → pr
```

## One parallel group, not a DAG

`validate` is one node whose workers — `test` and the three reviewers — run concurrently, each traced as its own phase with its own session and round. The group waits for every worker to finish or exhaust its retries, then forwards. It never sends back. The workflow stays an ordered list of nodes, as 0002 describes: the group is the one place that list runs anything at once, and a general-purpose graph of dependencies is left until something needs it.

Running phases in goroutines needed the runner's shared state made safe first. Phase numbering, spend totals, the accepted scope and the run's first error are behind one lock; each phase writes its own permission file for the guard; raw.jsonl writes are serialised a line at a time. A worker attempt that fails is recorded as a failed phase without failing the run, so a successful retry leaves the run healthy and the failed attempt in the trace.

## Only the adjudicator sends work back

The adjudicator is an ordinary node with a send-back budget of four, which it spends on the builder. It receives the request, the plan as amended so far, every report, lathe's measurement and its own earlier decisions; it may read the repository but not edit it. It decides every finding — fix, or dismiss with a reason — and writes one set of changes for the builder. No reviewer has a veto, and the complexity reviewer has the same authority as the other two.

Some of what it decides is checked mechanically, because it is exactly what an adjudicator might be talked out of. A gate refuses an acceptance while the measured suite is red, including when the failures look unrelated or pre-existing, refuses a decision set that misses or invents a finding, and refuses a scope addition that names a protected path, a glob or a path outside the repository. Acceptance also needs a usable report from every worker, which the node checks before it calls the agent.

The adjudicator may add files to the builder's scope and simplify the plan, with a reason for each; both are recorded and reach every later builder call and worker. It may not change what the engineer asked for.

All four workers rerun after every repair, so an acceptance always applies to the code in the tree. The fourth repair gets a complete round like any other; if it still needs changes, the work is handed off with the findings the adjudicator decided to fix.

## Retries are for infrastructure, not for findings

A worker that fails outright — a report that never validated, a timeout, a denied command — is retried up to twice against the same code, with a fresh session, while its peers' completed reports are kept. Retries share one correction allowance with the attempts before them, so the envelope corrections inside a call and the retries around it add rather than multiply. A report with findings, and a red suite, are results: they go to the adjudicator, and neither is retried. Retries spend nothing from the adjudicator's budget. A worker that exhausts them leaves validation incomplete.

## The checkout is shared, and frozen

The workers use the builder's uncommitted tree directly. Separate worktrees would need dependency provisioning per worker, which is more than a first version needs. While they run, enforcement after each turn is suspended, so a reviewer's turn ending cannot clean up files the tester's suite is still using; one sweep runs after the join.

Before the group starts, lathe fingerprints every uncommitted path. After the join and before the sweep, it compares: any fingerprinted path whose content changed, and any committed file that changed, is a source mutation. A path that is new and was never committed is test output, which the sweep removes. A mutation invalidates the round and ends the run: the reports describe code that is no longer there. The check cannot see a file a test changed and put back while a reviewer was reading it, nor a change to an ignored file; a project whose tests rewrite source will eventually need an isolated tester workspace.

## What each outcome publishes

| Outcome | `implement` | `build` |
| --- | --- | --- |
| Accepted: every report usable, measured suite green | Succeeds with the changes left in the tree | Commits and opens a pull request |
| Unresolved after four repairs | Fails, changes left in the tree, findings in validation.json | Commits and opens a draft pull request carrying the unresolved findings and the test output, and fails |
| Incomplete: a worker exhausted its retries | Fails, changes left in the tree | Opens a draft explaining what validation is missing, and fails |
| Invalidated, cancelled, or any other error | Fails | Fails before commit; nothing is published |

A run that opened a draft still fails. Publishing unaccepted work is how it gets handed to a person, not a claim that it passed, so the run's status, its reason, pr.json's `draft` and `validation` keys and `lathe show` all say it was not accepted. `implement` never commits, pushes or opens anything, whatever the outcome: publishing remains the difference between it and `build`, as 0003 describes.

This replaces the test node's own send-back loop. The measurement is unchanged — the exit code from `Handle.Command` is still the only evidence of green — but a red suite no longer routes anywhere by itself. The adjudicator reads it and cannot accept past it.

## Since: an adversarial reviewer

A fourth reviewer, `code-review-adversarial`, joins the group. Each of the other three checks one angle, and none is told to assume the change is broken and prove it, so a happy-path implementation could pass all three. The adversary goes after the failure paths: partial failure, retries, races, stale state and degraded dependencies. Every finding it raises names the input, sequence or condition that makes the code fail. Its prompt is adapted from OpenAI's `adversarial-review.md`. It is a worker like the others: it reruns after every repair, its findings go to the adjudicator, and it never sends work back.

Its angle overlaps the others' on purpose. The general, security and slop reviewers still stay on their own angles, and the adversary gives every area a second look. It has no quota of findings, and there is no separate verification step: the adjudicator traces a finding's trigger to its outcome before having it fixed, and asks for a test that reproduces a confirmed trigger, so the tester runs what a reviewer could only argue.

A reviewer may raise a dismissed finding again by citing its earlier reference and answering the reason it was dismissed. Each round's request shows reviewers the last adjudication and, in full, every finding it dismissed, whichever reviewer raised it. Earlier adjudications stay only in a reviewer's own session, unless a retry restarted it; the adjudicator's request has the full history.

The adjudicator's send-back budget goes from four to ten, with no short-circuit yet for rounds that make no progress. The worst case before work is handed off is eleven rounds of seven agent turns each. The code reviewers' prompted tool budget goes from roughly 25 calls to roughly 50, and the per-turn backstop from 40 to 80 so it stays above every prompted budget. At the speeds traces show, 80 calls do not fit in an agent's ten-minute deadline, so a deadline that expires now ends the turn the way the backstop does: the agent is asked for its report in the same session instead of the phase failing.

## Since: validation judges the code, not the handoff

The reviewers and the adjudicator judge code quality and correctness, and nothing else. A run whose request listed a local smoke test, a typecheck and build lathe does not measure, and results recorded in the pull request description as done criteria spent all ten send-backs with no finding left: the adjudicator kept sending the work back for checks only a person could do, and nobody can do them before the pull request exists. A check like that is not a finding and not a reason to send work back.

The adjudicator now lists such checks in a required `verification` key instead. Every pull request `build` opens, accepted or draft, carries them as a `## Needs manual verification` checklist taken from the last adjudication; when the list is empty the body is unchanged. Handing work to a person is no longer only what a draft does: an accepted change can hand over the checks automation could not make.

The gate gains a fourth refusal to back this up: a revise while the measured suite is green and no finding is decided fix. That is the one combination with nothing for the builder to do, so the adjudicator must accept. It is the first short-circuit for rounds that make no progress, and the only one: an adjudicator that keeps deciding the same finding fix still spends the whole budget.
