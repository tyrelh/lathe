---
status: proposed
---

# Task lanes are derived from the run and pull request

A task's lane is computed from its current run's latest iteration and its pull request's state, never stored and never dragged. The only lane an engineer chooses is In progress, by starting a build or re-entering the run: a revision from Ready for review, a recovery or fresh run from Blocked. Storing lanes, or letting cards be dragged freely, would let the board disagree with what the run and GitHub actually say, and would need repair whenever a worker is lost or a hook is missed.

Done and the closed-unmerged half of Blocked depend on pull request state that lathe never recorded before, so the manager now polls `gh pr view` for the pull requests of tasks in Ready for review or Blocked and records merged or closed. The board reads that record instead of calling GitHub per view.
