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

A Go binary that runs bounded agent workflows against whatever repository you invoke it from, and traces every run to SQLite. One install, not one per project.

```sh
make install     # build to ~/.local/bin/lathe, link it into ~/.claude, ~/.codex and ~/.pi/agent skills
```

```sh
lathe scout "where is authentication handled and what calls it"   # investigate; writes nothing
lathe plan  "add retry with backoff to the fetch client"          # plan a change; writes nothing
lathe build "add retry with backoff to the fetch client"          # plan it, then implement it
lathe runs                                                        # recent runs, every repo
lathe dash                                                        # http://127.0.0.1:4700, live
```

`build` needs a clean working tree and writes only the files its own plan named; review the result with `git diff` and commit it yourself. A guard vetoes every `write`, `edit` or `bash` call before it executes, so read-only agents change nothing by enforcement rather than by convention.

Agents run on [pi](https://github.com/earendil-works/pi), which needs `MOONSHOT_API_KEY` exported somewhere non-interactive shells see it. Traces go to `$XDG_DATA_HOME/lathe/runs.db`, else `~/.local/share/lathe/runs.db`.
