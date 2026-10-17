---
status: proposed
---

# Every build run belongs to a task

A build run submitted from anywhere (the Tasks board, New Run, the CLI or the lathe skill) belongs to a task, and one is created for it when it has none. The alternatives were a board of board-created tasks only, which would hide ad-hoc builds from the place an engineer tracks work, or attaching runs to tasks by hand. Auto-creation means the board fills with throwaway builds, so any task can be archived; archiving a task whose run is still live cancels the run first.

A task has at most one current run but may replace it with a fresh build when the run cannot continue, keeping earlier runs as its history. Existing builds are backfilled only when their pull request is still open or their latest iteration has not finished.
