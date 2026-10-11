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

## Agent configuration

Everything an agent is told and runs on lives in plain files under `~/.lathe`, one directory per agent:

```
~/.lathe/
  agents/<name>/config.yaml   # provider, model, thinking, timeout; optional tiers and repo_instructions
  agents/<name>/system.md
  agents/<name>/user.md       # must keep {{request}}
  routing/planner.md          # what routing asks about the planner's task
  routing/builder.md
  .seed.yaml                  # which seed lathe wrote for each file
```

```yaml
provider: moonshotai
model: kimi-k2.6
thinking: low               # off, minimal, low, medium, high or xhigh
timeout: 10m
repo_instructions: false    # don't send AGENTS.md / CLAUDE.md to this agent
```

- These files are the only source. There is no defaults layer: every `config.yaml` names provider, model, thinking and timeout, so what you read is what the agent runs with. Changing a provider everywhere means editing every agent.
- lathe writes them from the copy in the binary when a file is missing. After an upgrade to a newer release it replaces files you never edited; files you edited stay as they are, and if their seed has changed since, the dashboard flags them as drift and shows the diff. Development builds (`dev`) never replace anything.
- A missing or invalid file refuses the submission with an error naming the file. lathe never falls back to its own copy.
- Configuration is captured when work is submitted, so an edit only applies to runs and revisions submitted after it.
- `--provider`, `--model` and `--thinking` override every agent for one run. A repository's own `lathe.toml` is no longer read, and there is no per-repository configuration.
- Tools, protected paths and the shell deny list are compiled into lathe and can't be changed from these files.
- Files are written owner-only. Directories for agents lathe doesn't know are ignored. Prompt overrides from older versions in `~/.local/share/lathe/prompts/` are not migrated.

The agents you can configure are `scout`, `planner`, `plan-reviewer`, `builder`, `tester`, `code-review-general`, `code-review-security`, `code-review-slop`, `code-review-adversarial`, `adjudicator`, `brancher`, `committer` and `pr-author`.

## Model routing

The planner and builder ship with tiers, cheapest first, in their `config.yaml`, and TypeSafe's Jev model picks one each iteration based on the task. Routing runs whenever an agent's config has tiers; delete them to turn it off.

```yaml
tiers:
  - when: Mechanical change in one or two files; the plan spells out every edit
    model: gpt-6-luna
  - when: Behaviour change within one area; some judgment about existing code needed
    model: gpt-6-sol
    default: true
  - when: Cross-cutting change, concurrency, migrations, or an under-specified plan
    model: gpt-6-astra
    thinking: high
```

- Below a confidence of 0.5 routing bumps up one tier.
- You need 2 to 10 tiers with exactly one `default`. Anything a tier leaves out comes from the agent.
- No `TYPESAFE_API_KEY`, or any error from Jev, and it falls back to the default tier. Passing `--provider`, `--model` or `--thinking` turns routing off for that run.
- Your request and plan get sent to TypeSafe, and zero data retention is enterprise only. I'd think twice before turning this on for a private repo.
- Click a `route-*` phase on the dashboard to see what it picked and the probability for every tier.
- `lathe route-calibrate --agent builder` runs Jev over your past runs so you can tune the `when` text before relying on it.

## Prompts

The dashboard's Agents page lists every agent and routing phase. On an agent's page you can edit its prompts and its provider, model and thinking, one agent at a time; saves go straight to its files in `~/.lathe`.

- Each file shows its path, whether you've edited it, and drift when its seed has changed since. An edited file shows its diff from the seed.
- A save or reset is refused if the file changed since the page loaded it, whether in another tab or by hand. Saving the configuration keeps the comments in `config.yaml`.
- Reset to seed writes back the copy this binary ships.
- Each code reviewer has its own user prompt, so editing one doesn't change the others.
- A user prompt has to keep `{{request}}`. That's where your request goes.
- An edited prompt keeps shadowing its seed after you upgrade, even if the seed got better, so look over drift when you update.
