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

**Status: Phase 6.** `lathe scout "<request>"` runs the one v0 workflow: a read-only agent investigates the repo you invoked it from, its typed report is validated and gated, and the whole run is traced to one global SQLite database at `$XDG_DATA_HOME/lathe/runs.db` (else `~/.local/share/lathe/runs.db`). `lathe runs` lists recent runs across every repo; `lathe dash` serves those runs at http://127.0.0.1:4700, reading the database read-only and polling for new events so a run appears while it happens; `lathe install` links the checkout into each agent's skills directory. Agents run through [pi](https://github.com/earendil-works/pi) on its built-in `moonshotai` provider (needs `MOONSHOT_API_KEY` exported somewhere non-interactive shells see it).

```sh
lathe scout "where is authentication handled and what calls it"
lathe runs
lathe dash    # in another terminal; the page updates itself while a run is going
```
