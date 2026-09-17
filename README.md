<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./assets/lathe-logo-dark.png">
    <source media="(prefers-color-scheme: light)" srcset="./assets/lathe-logo-light.png">
    <img alt="Lathe" src="./assets/lathe-logo-light.png" width="500">
  </picture>
</p>

<p align="center">
  <strong>A software factory for agent-driven development.</strong>
</p>

A Go binary that runs phased agent work against whatever repository you invoke it from, and traces the run to SQLite. One install, not one per project — the repo is symlinked into each agent's skills directory and `lathe` lives on `PATH`.

```sh
make install     # build to ~/.local/bin/lathe, then link into ~/.claude and ~/.codex and ~/.pi/agent skills
```

**Status: Phase 2.** `lathe install` and `lathe help` are the only commands; `internal/trace` now writes runs, phases, and events to one global SQLite database at `$XDG_DATA_HOME/lathe/runs.db` (else `~/.local/share/lathe/runs.db`), shared by every repo. Agents run through [pi](https://github.com/earendil-works/pi) on its built-in `moonshotai` provider (needs `MOONSHOT_API_KEY` exported somewhere non-interactive shells see it). Orchestration and the dashboard are still to come.
