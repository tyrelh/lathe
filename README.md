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
lathe revise <id> "log the retry count"  # push another change to that build's pull request

lathe runs                               # recent runs, every repo
lathe show <id>                          # outcome, iterations and reports (--json, --iteration n)
lathe wait <id>                          # or --iteration n, for one iteration's outcome
lathe cancel <id>
lathe manager                            # queue and dashboard at http://127.0.0.1:4700
```

```
plan:      request → plan → review   (up to 4 send-backs)
implement: … → implement → validate → adjudicate   (up to 4 repairs)
build:     … → branch → implement → validate → adjudicate → commit → pr
revise:    request → plan → review → implement → validate → adjudicate → commit → push to the same pr
```

## Running

- Ctrl-C detaches from a run; `lathe cancel` stops it.
- The first submission starts a manager if none is running. Workers inherit its environment, so export `MOONSHOT_API_KEY` first. `LATHE_CAPACITY` sets concurrent runs (default 1).
- `implement` and `build` need a clean checkout. Don't edit files or switch branches there until they finish.
- `build` needs `gh` logged in. A failed build keeps its commit.
- `revise` extends a build whose latest iteration succeeded and whose pull request is still open. The checkout must be clean, on the pull request's branch, and at the commit lathe last pushed, with origin at that commit too; lathe does not switch branches, merge or rebase to get there, and says what to restore. Every revision is planned and validated from scratch, and only an accepted change is committed and pushed, with the push refused if origin moved. The run keeps its ID, pull request and totals; `show` lists each iteration and `wait --iteration n` waits for one.
- The builder can only write files its plan named. The shell deny list is a rough filter, not a sandbox.
- Traces go to `~/.local/share/lathe/lathe.db`, or under `$XDG_DATA_HOME`.

Use `--issue <number|URL|owner/repo#number>` instead of a prompt with `plan`, `implement`, or `build`. A bare number uses the target checkout’s GitHub remote (`--repo` still selects the local checkout). The worker needs `gh` on `PATH` and authenticated. An `issue` code phase fetches the title and body when the run starts, saves `issue.json`, and passes that task to the planner and subsequent agents. A failed lookup stops the run before planning. Issue comments are not included.

## Project config

A `lathe.toml` at the repo root overrides the built-in roster. The top level applies to every agent, and an `[agents.<name>]` block applies to one:

```toml
# lathe.toml at the repo root
provider = "anthropic"
model    = "claude-sonnet-5"

[agents.planner]
model = "claude-opus-5-5"

[agents.tester]
provider = "openai-codex"
model    = "gpt-6-luna"
thinking = "low"
```

Keys: `provider`, `model`, `thinking`, `repo_instructions`. Flags win, then `[agents.<name>]`, then the top level, then the built-in roster. Lathe ignores a malformed file, an unknown key or an unknown agent name, and prints a warning. Commit it before `implement` or `build`.

The agents a block can name:

- `scout`: finds and reports where things live
- `planner`: reads the repo and writes the plan the builder implements
- `plan-reviewer`: reviews the plan before the builder gets its write scope
- `builder`: implements the plan, and is the only agent that writes files
- `tester`: finds and runs the test command every validation round
- `code-review-general`: reviews correctness, the request and plan, conventions and tests
- `code-review-security`: reviews what crosses the application's trust boundaries
- `code-review-slop`: reviews for unneeded complexity
- `adjudicator`: decides every review finding and whether the builder goes round again
- `brancher`: names the branch for `build`
- `committer`: writes the commit message
- `pr-author`: writes the pull request title and body

Most agents also get the repo's AGENTS.md, or its CLAUDE.md, at the start of their first message. Lathe keeps it out of the system prompt so the repo can't outrank lathe's own instructions. The brancher, committer and pr-author read it themselves as one of their convention sources instead. Set `repo_instructions = false` at the top level to keep it from every agent, or in an `[agents.<name>]` block to change one agent either way.
