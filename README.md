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
lathe init                               # create lathe.toml in the current directory

lathe runs                               # recent runs, every repo
lathe show <id>                          # outcome, iterations and reports (--json, --iteration n)
lathe wait <id>                          # or --iteration n, for one iteration's outcome
lathe cancel <id>
lathe manager                            # queue and dashboard at http://127.0.0.1:4700
```

```
plan:      request → auth → [issue] → [route-plan] → plan → review   (up to 4 send-backs)
implement: … → [route-build] → implement → validate → adjudicate   (up to 10 repairs)
build:     … → branch → [route-build] → implement → validate → adjudicate → commit → pr
revise:    request → auth → [route-plan] → plan → review → [route-build] → implement → validate → adjudicate → commit → push to the same pr
```

## Running

- Ctrl-C detaches from a run; `lathe cancel` stops it.
- The first submission starts a manager if none is running. Workers inherit its environment: configure credentials for every provider in the captured agent roster, including providers used only by later agents. Export provider keys (for example `MOONSHOT_API_KEY`) before starting the manager, or log in with Pi as the worker user. Restart the manager after changing its environment. `LATHE_CAPACITY` sets concurrent runs (default 1).
- `implement` and `build` need a clean checkout. Don't edit files or switch branches there until they finish.
- `build` needs `gh` logged in. A failure after a commit keeps that commit; terminal validation environment failures never reach commit.
- `revise` extends a build whose latest iteration succeeded and whose pull request is still open. The checkout must be clean, on the pull request's branch, and at the commit lathe last pushed, with origin at that commit too; lathe does not switch branches, merge or rebase to get there, and says what to restore. Every revision is planned and validated from scratch, and only an accepted change is committed and pushed, with the push refused if origin moved. The run keeps its ID, pull request and totals; `show` lists each iteration and `wait --iteration n` waits for one.
- The builder can only write files its plan named. The shell deny list is a rough filter, not a sandbox.
- The planner, plan-reviewer and the four code reviewers have `web_read`, which fetches one public https page as markdown for docs and references. It refuses this machine, private networks and cloud metadata addresses. An agent can put anything it reads into a URL, so these agents can only read the repo and the Go module cache, and never `.env*`, key files or `.git/`. Repo source can still leave that way, which matters for a private repo. No agent has both `web_read` and a shell. Every URL is in the run's `raw.jsonl`.
- Traces go to `~/.local/share/lathe/lathe.db`, or under `$XDG_DATA_HOME`.

Before planning, `plan`, `implement`, `build` and `revise` run a traced, engineer-owned `auth` phase: request → auth → optional issue → plan. It checks each distinct captured provider once, including later agents, and does not repeat on planning send-backs. Scout is unchanged. To diagnose readiness, run `pi auth check --provider <provider> --json` from the repository as the worker user with the worker environment. Pi must support that command; unsupported or malformed responses fail closed before any agent turn. This checks credential availability, not live API-key validity; expired OAuth credentials may be refreshed and Pi's credential store updated. The command timeout and run cancellation bound the check; raw auth output is not traced.

Measured test exits **126/127** are terminal environment failures: check required executables, the worker's `PATH` and executable permissions, then rerun. `validation.json` records `environment_failed` and retains the tester report, command, result and output tail. There is one tester attempt, no adjudication or repairs, and no commit, push or PR (not even a draft). Already-running reviewers are joined before cleanup. Other worker failures may retry and exhausted retries produce `incomplete`, which build may publish as a draft; ordinary red suites still go to adjudication. Environment failures are neither of those outcomes, even if a suite intentionally uses 126/127 for another purpose.

Use `--issue <number|URL|owner/repo#number>` instead of a prompt with `plan`, `implement`, or `build`. A bare number uses the target checkout’s GitHub remote (`--repo` still selects the local checkout). The worker needs `gh` on `PATH` and authenticated. An `issue` code phase fetches the title and body after auth preflight, saves `issue.json`, and passes that task to the planner and subsequent agents. A failed lookup stops the run before planning. Issue comments are not included.

## Project config

Run `lathe init` from the target project root to create `lathe.toml` in the current directory with the built-in global defaults, effective settings for every agent, and the roster's default [model routing](#model-routing) tiers and confidence floor, so the generated file turns routing on. It does not require a Git repo and refuses to replace an existing file. The generated per-agent settings override the top level, so edit or remove those entries when changing global values.

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

Keys: `provider`, `model`, `thinking`, `repo_instructions`, plus routing tiers and `[routing]` (see [Model routing](#model-routing)). Flags win, then `[agents.<name>]`, then the top level, then the built-in roster. Lathe ignores a malformed file, an unknown key, an unknown agent name or invalid tiers, and prints a warning. Commit it before `implement` or `build`.

The agents a block can name:

- `scout`: finds and reports where things live
- `planner`: reads the repo and writes the plan the builder implements
- `plan-reviewer`: reviews the plan before the builder gets its write scope
- `builder`: implements the plan, and is the only agent that writes files
- `tester`: finds and runs the test command every validation round
- `code-review-general`: reviews correctness, the request and plan, conventions and tests
- `code-review-security`: reviews what crosses the application's trust boundaries
- `code-review-slop`: reviews for unneeded complexity
- `code-review-adversarial`: tries to break the change through failure paths, retries, races and stale state
- `adjudicator`: decides every review finding and whether the builder goes round again
- `brancher`: names the branch for `build`
- `committer`: writes the commit message
- `pr-author`: writes the pull request title and body

Most agents also get the repo's AGENTS.md, or its CLAUDE.md, at the start of their first message. Lathe keeps it out of the system prompt so the repo can't outrank lathe's own instructions. The brancher, committer and pr-author read it themselves as one of their convention sources instead. Set `repo_instructions = false` at the top level to keep it from every agent, or in an `[agents.<name>]` block to change one agent either way.

## Model routing

Routing picks the planner's and the builder's model per iteration from the task in front of them. It's off until a repo's `lathe.toml` gives an agent tiers: the models you're willing to run it on, cheapest first, each described by the kind of task it suits.

```toml
[routing]
confidence_floor = 0.5

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

A `route-plan` phase before planning and a `route-build` phase before implementing each ask TypeSafe's Jev model (`jev-1.13.0`) one Score question: how demanding is this task, against your `when` descriptions. The most likely tier wins, ties going to the more capable one, and confidence under `confidence_floor` (default 0.5) moves the pick up one more tier. Jev never names a model; it only picks one of yours. Send-backs and repairs keep the tier, and each revision routes again.

- Only `planner` and `builder` take tiers: 2 to 10 each, exactly one `default = true`, and a non-empty `when` on every one. Describe a situation in `when` and leave out numbers and comparisons to other tiers; Jev judges each level on its own.
- A tier sets only what differs. `provider`, `model` and `thinking` it leaves out come from the agent's usual config.
- `--provider`, `--model` or `--thinking` turns routing off for that run.
- The auth phase checks every tier's provider, so whichever one is picked is ready.
- Workers need `TYPESAFE_API_KEY`. Without it, or on any API error or malformed response, routing uses the default tier and the run carries on.
- Clicking a `route-*` phase on the dashboard shows the tier picked, the reason (`routed`, `low-confidence` or `error`), confidence and score, and every tier with its probability. The phase's Inputs and Raw responses hold the exact request sent to Jev and its unedited response. Jev's spend is recorded under the `typesafe` provider.
- `route-plan` sends the request, or the issue's title and body, or a revision's brief including its branch diff. `route-build` sends the request and the accepted plan: summary, steps, file paths and risks. TypeSafe says it doesn't train on requests, but zero data retention is enterprise-only, so think about that before turning routing on for a private repo.
- The plan-reviewer is never routed, so a cheap planner always has a capable reviewer behind it.

Tune the `when` text and the floor before relying on it. From the repo, `lathe route-calibrate --agent planner|builder [--limit 30]` asks Jev about that repo's past runs through the same code the routing phases use and prints each pick next to the run's plan send-backs and builder repairs, with a blank column to fill in the tier you'd have picked. Label each run before you read Jev's answer. Past runs used fixed models, so their send-backs and repairs are only a hint.
