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

Lathe is a Go binary that runs bounded agent workflows against whatever repo you run it from, and traces every run to SQLite.

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
lathe init                               # create lathe.toml in the current directory

lathe runs                               # recent runs, every repo
lathe show <id>                          # outcome, iterations and reports (--json, --iteration n)
lathe wait <id>                          # or --iteration n, for one iteration's outcome
lathe cancel <id>
lathe manager                            # queue and dashboard at http://127.0.0.1:4700
lathe route-calibrate --agent builder    # compare routing picks against past runs
```

```
plan:      request → auth → [issue] → [route-plan] → plan → review   (up to 4 send-backs)
implement: … → [route-build] → implement → validate → adjudicate   (up to 10 repairs)
build:     … → branch → [route-build] → implement → validate → adjudicate → commit → pr
revise:    request → auth → [route-plan] → plan → review → [route-build] → implement → validate → adjudicate → commit → push to the same pr
```

## Running

- Ctrl-C just detaches from a run. Use `lathe cancel` if you actually want to stop it.
- Your first run starts a manager, and workers inherit its environment. So export your provider keys (like `MOONSHOT_API_KEY` or `TYPESAFE_API_KEY`) first, and restart the manager if you change them. `LATHE_CAPACITY` sets how many runs go at once (default 1).
- `implement`, `build` and `revise` need a clean checkout. `build`, `revise` and `--issue` need `gh` logged in.
- Every run checks auth for all the providers it'll use before any agent starts. If that fails, `pi auth check --provider <provider> --json` is the quickest way to see why.
- The builder can only write the files its plan named. Agents with `web_read` can't read secrets or `.git/`, but repo source can still leave through a URL, so keep that in mind for private repos.
- A test exit of 126 or 127 means something is wrong with the environment (missing executable, bad `PATH`), so the run stops there with no repairs and no commit.
- Traces live in `~/.local/share/lathe/lathe.db`.

## Project config

Run `lathe init` in your project and you'll get a `lathe.toml` with every option filled in, routing included. Then change whatever you want:

```toml
provider = "anthropic"
model    = "claude-sonnet-5"

[agents.planner]
model = "claude-opus-5-5"

[agents.tester]
provider          = "openai-codex"
model             = "gpt-6-luna"
thinking          = "low"
repo_instructions = false   # don't send AGENTS.md / CLAUDE.md to this agent
```

Flags win, then `[agents.<name>]`, then the top level, then the built-in roster. If the file is invalid lathe ignores it and prints a warning.

The agents you can configure are `scout`, `planner`, `plan-reviewer`, `builder`, `tester`, `code-review-general`, `code-review-security`, `code-review-slop`, `code-review-adversarial`, `adjudicator`, `brancher`, `committer` and `pr-author`.

## Model routing

Give the planner or builder a few tiers, cheapest first, and TypeSafe's Jev model will pick one each iteration based on the task:

```toml
[routing]
confidence_floor = 0.5   # below this, bump up one tier

[agents.builder]
provider = "openai-codex"

[[agents.builder.tiers]]
when  = "Mechanical change in one or two files; the plan spells out every edit"
model = "gpt-6-luna"

[[agents.builder.tiers]]
when    = "Behaviour change within one area; some judgment about existing code needed"
model   = "gpt-6-sol"
default = true

[[agents.builder.tiers]]
when     = "Cross-cutting change, concurrency, migrations, or an under-specified plan"
model    = "gpt-6-astra"
thinking = "high"
```

- You need 2 to 10 tiers with exactly one `default`. Anything a tier leaves out comes from the agent.
- No `TYPESAFE_API_KEY`, or any error from Jev, and it falls back to the default tier. Passing `--provider`, `--model` or `--thinking` turns routing off for that run.
- Your request and plan get sent to TypeSafe, and zero data retention is enterprise only. I'd think twice before turning this on for a private repo.
- Click a `route-*` phase on the dashboard to see what it picked and the probability for every tier.
- `lathe route-calibrate --agent builder` runs Jev over your past runs so you can tune the `when` text before relying on it.
