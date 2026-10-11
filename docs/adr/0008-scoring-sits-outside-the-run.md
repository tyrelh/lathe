# Scoring sits outside the run

A scorer grades a finished build run with a pull request after the workflow is done, and the engineer starts it from the Scoring dashboard or with `lathe score <run-id>`. Scoring could have been a phase appended to the iteration, reusing phase recording, the Gantt chart and the usage path. It isn't one. A run's state, outcome and cost describe what the workflow did, and grading is a separate question about that work. A scorer that fails or stalls must not make a finished run look unfinished, and its spend must not inflate what the change cost. `PhaseUpsert` already refuses phases on a run that has completed, and `RecordUsage` adds to `runs.cost`, so routing scoring through either would mean weakening both.

A score is stored apart from the run. It records the iteration it graded, the scorer's model and cost, and a hash of the scorer's prompts, and scoring again replaces it. Scoring runs as its own job in the manager queue and counts against the same capacity as runs.

## Scorers are compiled agents

Each scorer is an agent in the compiled roster with its own seed and agent configuration, not a rubric file that one generic scorer agent reads. That means adding a scorer needs a code change. In exchange, every scorer gets its own provider, model, thinking and tiers, so a scorer can be put on a different provider from the agents it grades. Scorers are seeded and drift-flagged like every other agent.

A scorer's permissions are read-only and limited to the data directory of the run it grades, plus a snapshot lathe writes there first: the pull request's diff and state, and the cheap signals already in the trace. It cannot read the repository, the web or the shell. Transcripts hold repository content and fetched pages, so the scorer's prompt treats them as data, not instructions.

## Not sampled

Scoring is manual for now. Automatic sampling would need lathe to decide when a run is done being worked on, and a build stays open to revisions for as long as its pull request does. The Scoring dashboard lists every finished build with a pull request and shows the PR's state, so the engineer decides when a run is ready.
