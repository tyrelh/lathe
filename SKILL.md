---
name: lathe
description: Run a bounded, traced agent workflow against the current repository. Use when asked to scout, map, or investigate a codebase with lathe, or when the user says "lathe".
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

## Writing the request

One concrete question, in one sentence. The scout answers what it was asked and
stops, so "where is authentication handled and what calls it" gets a usable
answer where "review the codebase" gets a shallow one.

## Flags

Before the request, not after:

- `--repo <dir>` act on another repository
- `--model`, `--provider`, `--thinking` override the roster for one run

## Reading the result

The run prints its status, spend and directory. `<dir>/result.json` is the
agent's structured report; `<dir>/raw.jsonl` is the full event stream.
`lathe runs` lists recent runs from every repo.
