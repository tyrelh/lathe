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

The tester discovers the test command, then lathe runs it and uses its exit code to decide success. A failing suite gets at most two builder fix rounds in the same session. Test commands inherit your environment, run under the tester timeout, and pass the shell deny list.

A guard vetoes every `write`, `edit` or `bash` call before it executes, with the tester alone receiving a shell. The shell deny list is a coarse check, not a sandbox.

Agents run on [pi](https://github.com/earendil-works/pi), which needs `MOONSHOT_API_KEY` exported somewhere non-interactive shells see it. Traces go to `$XDG_DATA_HOME/lathe/runs.db`, else `~/.local/share/lathe/runs.db`.

`lathe dash` opens on **Overview**: every run ever recorded and every dollar recorded against them, across every repository, plus the ten costliest runs and the ten costliest `provider/model` pairs.

A run's detail page shows per-phase model, tokens, and cost. Expand phase inputs or event payloads to read them in full; shell commands and tool errors are always visible. The permissions panel records kept and reverted files for each check. New runs record each agent turn's system prompt, supplied prompt, session ID, and write scope; older traces show when input context was not recorded.
