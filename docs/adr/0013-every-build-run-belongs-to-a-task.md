---
status: accepted
---

# Every build run belongs to a task

A new build run submitted from anywhere (the Tasks board, New Run, the CLI or the lathe skill) belongs to a task, and one is created for it when it has none. The alternatives were a board of board-created tasks only, which would hide ad-hoc builds from the place an engineer tracks work, or attaching runs to tasks by hand. Auto-creation means the board fills with throwaway builds, so any task can be archived; archiving requests cancellation of its current live run before recording the archive, without waiting for execution to stop.

A task has at most one current run: its newest submitted build. A fresh build may replace a run that cannot continue, keeping earlier runs as its history. Revisions and recoveries add iterations to the same run rather than replacing it.

The manager repeatedly backfills taskless legacy builds after completed pull request polling passes, including empty passes; this is not a one-time migration. A legacy build is eligible when its latest iteration is nonterminal or its recorded pull request state is open. Terminal builds with unknown, closed or merged pull request state remain taskless. Repeated passes leave already attached builds unchanged.
