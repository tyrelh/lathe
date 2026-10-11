---
status: proposed
---

# Recovery extends the run with a new iteration

Manual recovery continues a failed, cancelled, or lost run in a new iteration, preserving the unsuccessful iteration rather than reopening its terminal state. The engineer's recovery instruction, current agent configuration, and fresh bounded allowances belong to that new iteration, while the run retains its identity and cumulative history. This uses the same history boundary as revisions without restricting recovery to successful builds with open pull requests.

Writing-run recovery restores available partial work and returns through planning and review, so the planner can reconsider the approach using failure evidence rather than replaying a stale decision. A reviewed plan may authorize reversals of partial work, with its abandoned version preserved. When restoration is impossible, the engineer can explicitly restart from a verified base; recovery never silently substitutes a fresh implementation for missing work. If the original base is unavailable, the engineer supplies the replacement and submission freezes its commit.

Recovery reconciles completed publication actions before restoration and reuses the existing branch and a uniquely identified PR. A failed acknowledgement is not proof that a push or PR creation failed, so repeating either action blindly could duplicate work. External remote branch movement refuses continuation rather than introducing merge or rebase behavior into recovery. Closed/merged PRs, ambiguous publication evidence, and unreadable origin/GitHub state required for reconciliation reject submission; checks repeat at execution.

Only accepted recovery work updates an existing PR, even if it is already draft, because draft status must not weaken acceptance for published work under review. Fresh acceptance can mark a draft ready when Lathe can establish that it created that draft for unaccepted work; draft status of unknown provenance is preserved. Recovery before a PR exists retains ordinary build publication policy.
