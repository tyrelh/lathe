---
name: lathe
description: Run a bounded, traced agent workflow against the current repository. Use when asked to scout, map, or investigate a codebase with lathe, or when the user says "lathe". Not yet wired up — Phase 0 skeleton only.
---

# lathe

`lathe` is a software factory: a Go binary that runs phased agent work against
whatever repository you are sitting in, and streams a trace of it to SQLite.

## Status

Phase 0. Only `lathe install` and `lathe help` exist; there are no workflows to
invoke yet. Do not attempt to orchestrate agent phases by hand in place of
`lathe` — sequencing is the binary's job, not the calling agent's.

## Usage

```
lathe <workflow> "<prompt>"
```

Run it from inside the target repository; `lathe` discovers the repo root
itself. Workflows will be listed here as they land.
