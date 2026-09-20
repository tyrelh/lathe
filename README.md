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
  <a href="https://github.com/tyrelh/lathe/actions/workflows/lathe-ci.yml?query=branch%3Amain"><img alt="lathe-ci status" src="https://github.com/tyrelh/lathe/actions/workflows/lathe-ci.yml/badge.svg?branch=main"></a>
</p>

A Go binary that runs bounded agent workflows against whatever repository you invoke it from, and traces every run to SQLite. One install, not one per project.

```sh
# build to ~/.local/bin/lathe, link it into ~/.claude, ~/.codex and ~/.pi/agent skills
make install
```

```sh
# investigate; writes nothing
lathe scout "where is authentication handled and what calls it"

# plan a change; writes nothing
lathe plan  "add retry with backoff to the fetch client"

# plan, implement, test, and fix failures
lathe build "add retry with backoff to the fetch client"

# recent runs, every repo
lathe runs

# http://127.0.0.1:4700, live observability dashboard
lathe dash
```

`build` needs a clean working tree. The builder may write only the files its own plan named; review the result with `git diff` and commit it yourself.

The tester discovers the test command, then lathe runs it and uses its exit code to decide success. A failing suite gets at most two builder fix rounds in the same session. Test commands inherit your environment, run under the tester timeout, and pass the shell deny list. Test-generated changes outside the accumulated plan scope are reverted; planned work remains for review even if the run fails.

A guard vetoes every `write`, `edit` or `bash` call before it executes, with the tester alone receiving a shell. The shell deny list is a coarse check, not a sandbox.

Agents run on [pi](https://github.com/earendil-works/pi), which needs `MOONSHOT_API_KEY` exported somewhere non-interactive shells see it. Traces go to `$XDG_DATA_HOME/lathe/runs.db`, else `~/.local/share/lathe/runs.db`.

`lathe dash` opens on **Overview**: every run ever recorded and every dollar recorded against them, across every repository, plus the ten costliest runs and the ten costliest `provider/model` pairs. A left rail collapses to icons, the top bar names the version of the binary serving the page, and the bottom bar carries the current page's own context — displayed versus total runs on Runs, and workflow, status, tokens, spend and elapsed time on a run.

Routes are `#/overview`, `#/runs` and `#/runs/<run-id>`; older `#<run-id>` links still open their run. Theme (auto / light / dark) and the rail's collapsed state are remembered locally.

A run's detail page still shows per-phase model, tokens, and cost. Expand phase inputs or event payloads to read them in full; shell commands and tool errors are always visible. The permissions panel records kept and reverted files for each check. New runs record each agent turn's system prompt, supplied prompt, session ID, and write scope; older traces show when input context was not recorded.

### What the numbers mean

Spend is written to the database after each completed model response, so a live run's cost moves while it is still working, and an interrupted run keeps what it had already spent. A response that ended in a tool call counts; streaming partial updates do not.

Cumulative figures are lifetime totals over the local trace database — what lathe recorded, not a reconciled provider invoice. Model attribution comes from the usage events, grouped by provider and model. Runs from before v0.2 have no provider recorded and may have no model either; those show as `unknown`. Where an older run's stored total is more than its usage events explain, the excess is kept as unattributed spend: it counts toward the totals and is reported below the model ranking, but is never charged to a named model. The original totals and the signed difference stay in the database for inspection.

The model ranking counts **phases**, not runs. A phase runs one agent and so one model; a run moves between agents and spends across several, so a run count would collapse a build's planner, builder and tester phases into one. The top ten shares are of total recorded spend, so they are not expected to reach 100%.

`lathe dash` migrates the database before it starts serving, so an old trace — or no trace at all — opens as a dashboard rather than an error. Build with `make release VERSION=v0.2.0` to stamp the version; a plain `make build` reports `dev`.
