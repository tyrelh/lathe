# Lathe

A software factory: a Go binary that runs phased agent work against whatever
repository you invoke it from, and traces the run to SQLite. One install, not
one per project — the repo is symlinked into each agent's skills directory and
`lathe` lives on `PATH`.

```sh
make install     # build to ~/.local/bin/lathe, then link into ~/.claude and ~/.codex and ~/.pi/agent skills
```

**Status: Phase 0.** Only `lathe install` and `lathe help` exist. Agents run
through [pi](https://github.com/earendil-works/pi) on its built-in `moonshotai`
provider (needs `MOONSHOT_API_KEY` exported somewhere non-interactive shells
see it). Orchestration, tracing, and the dashboard are still to come.
