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

Two workflows, both read-only. `lathe scout "<request>"` investigates the repo you invoked it from and reports what is there. `lathe plan "<request>"` plans a change to it: a summary, ordered steps, the exact files the change may touch, and the risks. Every report is typed and validated, and a plan naming a protected path such as `.git` or `.env*` is rejected.

The write boundary runs before the tool does. lathe spawns [pi](https://github.com/earendil-works/pi) with `--no-extensions` and a compiled-in guard extension that vetoes a `write`, `edit` or `bash` call ahead of execution: outside the repo, onto a protected path, or onto a file the run has no permission for. Read-only agents get an empty allow list, so the tool layer enforces that they change nothing.

Every run traces to one global SQLite database at `$XDG_DATA_HOME/lathe/runs.db` (else `~/.local/share/lathe/runs.db`). `lathe runs` lists recent runs across every repo. `lathe dash` serves them at http://127.0.0.1:4700, reading the database read-only and polling for new events so a run shows up while it happens. `lathe install` links the checkout into each agent's skills directory.

Agents run on pi's built-in `moonshotai` provider, which needs `MOONSHOT_API_KEY` exported somewhere non-interactive shells see it.

```sh
lathe scout "where is authentication handled and what calls it"
lathe plan "add retry with backoff to the fetch client"
lathe runs
lathe dash    # in another terminal; the page updates itself while a run is going
```
