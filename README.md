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

<p align="center">
  <a href="https://github.com/tyrelh/lathe/actions/workflows/ci.yml?query=branch%3Amain"><img alt="ci status" src="https://github.com/tyrelh/lathe/actions/workflows/ci.yml/badge.svg?branch=main"></a>
</p>

Lathe is a Go binary that runs bounded agent workflows against the repo you invoke it from. It traces every run to SQLite.

```sh
make install   # build ~/.local/bin/lathe and link it into your agents' skills

lathe scout "where is auth handled"      # investigate; writes nothing
lathe plan "add retry to the client"     # plan; writes nothing
lathe plan --issue 41                    # plan from an issue in this repo
lathe implement --issue tyrelh/lathe#41   # implement from a GitHub issue
lathe implement "add retry"              # plan, implement, test; leave it uncommitted
lathe build "add retry"                  # implement, then branch, commit and open a PR
lathe build --detach "add retry"         # queue it and exit

lathe runs                               # recent runs, every repo
lathe show <id>                          # outcome and reports (--json too)
lathe wait <id>
lathe cancel <id>
lathe manager                            # queue and dashboard at http://127.0.0.1:4700
```

```
plan:      request → plan → review   (up to 4 send-backs)
implement: … → implement → test      (up to 4 fix rounds)
build:     … → branch → implement → test → commit → pr
```

## Running

- Ctrl-C detaches from a run; `lathe cancel` stops it.
- The first submission starts a manager if none is running. Workers inherit its environment, so export `MOONSHOT_API_KEY` first. `LATHE_CAPACITY` sets concurrent runs (default 1).
- `implement` and `build` need a clean checkout. Don't edit files or switch branches there until they finish.
- `build` needs `gh` logged in. A failed build keeps its commit.
- The builder can only write files its plan named. The shell deny list is a rough filter, not a sandbox.
- Traces go to `~/.local/share/lathe/lathe.db`, or under `$XDG_DATA_HOME`.

Use `--issue <number|URL|owner/repo#number>` instead of a prompt with `plan`, `implement`, or `build`. A bare number uses the target checkout’s GitHub remote (`--repo` still selects the local checkout). The worker needs `gh` on `PATH` and authenticated. An `issue` code phase fetches the title and body when the run starts, saves `issue.json`, and passes that task to the planner and subsequent agents. A failed lookup stops the run before planning. Issue comments are not included.

## Project config

```toml
# lathe.toml at the repo root
provider = "anthropic"
model    = "claude-sonnet-5"

[agents.planner]
model = "claude-opus-5-5"
```

Keys: `provider`, `model`, `thinking`. Flags win, then `[agents.<name>]`, then the top level, then the built-in roster. Lathe ignores a malformed file and prints a warning. Commit it before `implement` or `build`.
