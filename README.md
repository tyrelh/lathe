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

**Status: v0.1 Phase 3.** `lathe scout "<request>"` investigates a repository and `lathe plan "<request>"` produces a read-only plan. `lathe build "<request>"` plans and implements a change, leaving it uncommitted for review with `git diff`. Build requires a clean Git working tree. The builder has write and edit tools but no shell; the guard permits only the plan's exact file list and blocks protected paths. Missing files are reported in `needed` and fail the run without expanding permission. Git checks the builder's change claims in both directions. Tests are not run yet; the tester arrives in Phase 4.

Reports and traces stay outside the target repository: `plan.json` and `build.json` live in the run directory, and events go to SQLite at `$XDG_DATA_HOME/lathe/runs.db` (else `~/.local/share/lathe/runs.db`). `lathe runs` lists recent runs across every repo; `lathe dash` serves those runs at http://127.0.0.1:4700, reading the database read-only and polling for new events so a run appears while it happens; `lathe install` links the checkout into each agent's skills directory. Agents run through [pi](https://github.com/earendil-works/pi) on its built-in `moonshotai` provider (needs `MOONSHOT_API_KEY` exported somewhere non-interactive shells see it).

```sh
lathe scout "where is authentication handled and what calls it"
lathe plan "add retry with backoff to the fetch client"
lathe build "add retry with backoff to the fetch client"
lathe runs
lathe dash    # in another terminal; the page updates itself while a run is going
```
