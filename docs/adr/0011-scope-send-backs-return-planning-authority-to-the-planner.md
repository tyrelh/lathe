---
status: accepted
---

# Scope send-backs return planning authority to the planner

A builder's scope request returns to the planner and plan-reviewer instead of automatically granting files. The planner retains authority over the approach, including finding an allowed alternative to a protected-file request and directing reversals of partial work. This keeps file permission enforcement separate from the builder's judgment about how to implement the change.

A revised approach can invalidate an adjudicator finding, so the planner may dismiss it with the finding reference and a reason tied to that change. Historical findings and their dispositions remain inspectable, and fresh validation and adjudication still determine acceptance. Recovery maintains the original goal; adjacent work may be proposed as future tickets but grants no scope and creates no actual tickets.

Automatic scope send-backs keep the existing iteration and spent allowances: two scope send-backs, eight plan-review send-backs, and ten adjudicator repairs. Manual recovery instead starts a new iteration with fresh bounded allowances. This permits ordinary scope omissions to converge without repeatedly resetting the run's cost bounds.

As implemented, a nonempty `needed` list is a typed scope request only when the report decoded cleanly; it outranks correction of missing fields, never provider errors, cancellation or enforcement. Malformed, escaping, inexact or already-allowed paths end the run; protected paths may be planned around but never granted. Each request is saved as `scope-<n>.json` and each answering plan as `plan-<n>.json`. The plan must keep every uncommitted path in `files` or abandon it as a reversal; one disposition step, shared with manual recovery, applies reversals (saving the work first) and dismissals only after review forwards the plan. A final review that is unusable or still blocking at the limit ends the run. Review then forwards straight to the builder, past branch creation and routing, and rounds count completed implementations. Finding references repeat across iterations, so a dismissal records the iteration whose validation raised its finding, and a recovery applies a dismissal only to findings from that iteration; a reused reference in later validation is a new finding.
