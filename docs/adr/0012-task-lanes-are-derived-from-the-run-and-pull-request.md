---
status: accepted
---

# Task lanes are derived from the run and pull request

A task's lane is computed from its current run's latest iteration and its recorded pull request state, never stored and never dragged. The only lane an engineer chooses is In progress, by starting a build or re-entering the run: a revision from Ready for review, a recovery or fresh run from Blocked. Storing lanes, or letting cards be dragged freely, would let the board disagree with what the run and GitHub actually say, and would need repair whenever a worker is lost or a hook is missed.

The first matching rule determines the lane:

- No current run → Backlog.
- Recorded merged pull request → Done.
- Latest iteration queued, starting or running → In progress.
- Recorded closed pull request → Blocked.
- Successful latest iteration with a recorded pull request URL → Ready for review.
- Otherwise → Blocked.

The manager polls `gh pr view` for build and revise runs with unknown or open pull request state and a recorded pull request URL, regardless of task lane. It records open, merged or closed; the board reads that record instead of calling GitHub per view. Recorded merged and closed states are final: they cannot be overwritten and are excluded from further polling, so a later reopening on GitHub is not reflected. Open is not final and continues to be polled.
