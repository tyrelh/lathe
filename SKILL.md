---
name: lathe
description: Run a bounded, traced agent workflow against the current repository. Use when asked to scout, map, or investigate a codebase with lathe, to plan a change with lathe, or when the user says "lathe".
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

## Workflows

- `scout "<request>"` — investigate and report. Read-only: the scout has
  `read`, `grep`, `find` and `ls`, no shell and no write tool, so it cannot
  change the repo it is pointed at.
- `plan "<request>"` — plan a change: a summary, ordered steps, the files the
  change touches, and the risks. Read-only on the same terms as the scout. The
  file list is the write scope a builder will later be held to, so a plan that
  names `.git`, `.env*` or key material is rejected before it is printed.

## Writing the request

One concrete question or one concrete change, in one sentence. The agent answers
what it was asked and stops, so "where is authentication handled and what calls
it" gets a usable answer where "review the codebase" gets a shallow one, and
"add retry with backoff to the fetch client" gets a usable plan where "improve
error handling" gets a vague one.

## Flags

Before the request, not after:

- `--repo <dir>` act on another repository
- `--model`, `--provider`, `--thinking` override the roster for one run

## Reading the result

The run prints its status, spend and directory. `<dir>/result.json` is a scout's
structured report and `<dir>/plan.json` is a planner's, which `lathe plan` also
prints; `<dir>/raw.jsonl` is the full event stream. `lathe runs` lists recent
runs from every repo.
