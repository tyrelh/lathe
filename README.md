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

# plan, implement, test, and fix failures; leave it uncommitted
lathe implement "add retry with backoff to the fetch client"

# the same, then branch, commit and open a pull request
lathe build "add retry with backoff to the fetch client"

# record the request, print the run ID, exit
lathe build --detach "add retry with backoff to the fetch client"

# queued, running and finished runs, every repo
lathe runs

# one run: state, outcome, reports, artifact locations (--json too)
lathe show <id>

# block until a run finishes; exit with its outcome
lathe wait <id>

# ask a run to stop
lathe cancel <id>

# the scheduler and http://127.0.0.1:4700, in the foreground
lathe manager
```

Submission and execution are separate processes. Every workflow command
records a run in the database and then block until it finishes; the work itself
happens in a worker process, so interrupting the wait detaches from the run
rather than cancelling it — `lathe cancel` is how you stop one. A long-lived
manager drains the queue and serves the dashboard; if none is running, a
submission starts one in the background and it stays up until it is killed.

Workers inherit the manager's environment: API keys, `PATH`, test settings. That
is the environment of whatever started the manager — a shell running
`lathe manager`, or the first submission when none was running — and it stays
fixed until the manager is restarted. Export what agents and test suites need
before either. `LATHE_CAPACITY` sets how many runs may execute at once; it
defaults to 1.

`implement` and `build` need a clean working tree, and the checkout belongs to
lathe until the run stops: do not edit files or switch branches there while one
is running, because permission cleanup can revert changes made during the run,
including yours. Lathe records the branch at submission and fails the run if it
changed before execution started; newer commits on that branch are fine. A
second outstanding run of either for the same checkout is rejected immediately.
The builder may write only the files its own plan named.

`implement` stops there: review the result with `git diff` and commit it
yourself. `build` goes on to branch, commit and open a pull request, which needs
`git` and `gh` on `PATH` with `gh` already authenticated. In each of those three
phases an agent judges the text — the branch name, the commit message, the pull
request title and body, each read off what the repository already does — and
lathe performs the git itself, staging exactly the scope the run accumulated. No
agent can commit or push: the guard denies both. A build ends on the branch it
created, and a failure after its commit leaves that commit there rather than
losing the work.

The plan, implement and build workflows share a read-only review loop:

```
request → plan → review → [plan → review, up to four send-backs]
plan:      → print the reviewed plan
implement: → implement → test → [implement → test, up to four fixes]
build:     → branch → implement → test → [implement → test] → commit → pr
```

The reviewer checks the plan against the repository, including the builder's
file list. Empty feedback accepts it. The fifth review ends the loop; remaining
objections or a failed final review become risks in the saved plan and builder
handoff. Failed reviews consume a send-back and remain visible as failed phases
even when the run succeeds. Planner failures and cancellation stop the run.

The tester discovers the test command, then lathe runs it and uses its exit code to decide success. A failing suite gets at most four builder fix rounds in the same session. Test commands inherit your environment, run under the roster's `command_timeout`, and pass the shell deny list.

A guard vetoes every `write`, `edit` or `bash` call before it executes. Four agents get a shell: the tester, and the three that read git and `gh` to judge a branch name, a commit message and a pull request. The shell deny list is a coarse check, not a sandbox.

Agents run on [pi](https://github.com/earendil-works/pi), which needs `MOONSHOT_API_KEY` in the manager's environment. Traces go to `$XDG_DATA_HOME/lathe/lathe.db`, else `~/.local/share/lathe/lathe.db`; per-attempt logs, raw streams and reports sit beside it under `runs/`, and the manager's own log is `manager.log`.

A crashed worker holds capacity until it is confirmed stopped. There is no
recovery command: the manager prints the run, the process, its log and the
exact `kill` to use, and you restart the manager afterwards.

The dashboard opens on **Overview**: every run ever recorded and every dollar recorded against them, across every repository, plus the ten costliest runs and the ten costliest `provider/model` pairs.

A run is listed from the moment it is queued, with queue time and execution
time shown separately. A run's detail page shows per-phase model, tokens, and cost. Expand phase inputs or event payloads to read them in full; shell commands and tool errors are always visible. The permissions panel records kept and reverted files for each check. New runs record each agent turn's system prompt, supplied prompt, session ID, and write scope; older traces show when input context was not recorded.

Expand a planner or plan-reviewer phase to read its plan or review first, with
raw responses and inputs in separate disclosures underneath. Each phase keeps
its own result, including plans later revised. Structured output is available
for new runs; older phases indicate when it was not recorded.
