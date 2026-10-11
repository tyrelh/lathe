# Agent configuration lives in ~/.lathe

Supersedes [0006](0006-prompt-overrides-are-machine-wide.md).

What an agent is told and runs on used to come from three layers: the roster and prompts compiled into the binary, prompt overrides in the data root, and a target repository's `lathe.toml`. Reading the effective configuration meant merging all three in your head, and only prompts could be changed without a rebuild. Agent configuration now lives in plain files under `~/.lathe`, and those files are the only source. The binary seeds them and never reads its own copy at capture.

## One file set per agent, no layers

Each agent has `agents/<name>/config.yaml`, `system.md` and `user.md`. Routing prompts live in `routing/<name>.md`. Every `config.yaml` names provider, model, thinking and timeout explicitly; tiers and `repo_instructions` are optional. There is no defaults file. The seeded files are the defaults, so what you read in an agent's dir is what that agent runs with. Changing a provider everywhere means editing every agent, and we accept that cost to keep a single place to look.

Thinking must be one of the levels Pi accepts: off, minimal, low, medium, high or xhigh. Anything else is refused rather than passed through. The routing confidence floor, which only `lathe.toml` could set, is now fixed at 0.5.

The four code reviewers used to share one user prompt. Each now gets its own seeded copy, so specializing a reviewer is an edit, not a roster change.

YAML replaces TOML, decoded strictly so an unknown key is an error, and so the dashboard can rewrite a field through `yaml.v3` nodes without dropping hand-written comments. The dashboard edits prompts and provider, model and thinking, one agent at a time. Every write carries the hash of the file it was loaded from and is refused if the file changed since, a reset included.

## Permissions stay compiled

0006 kept `roster.toml` out of the overlay because it decides each agent's tools, protected paths and `bash_denied`. That reasoning still holds now that configuration is a set of editable files. Agent permissions and the set of agents are compiled into the binary and never written to disk. Agent configuration can change what an agent is told and which model runs it, never what it may do. A config dir for an agent the binary doesn't know is ignored.

## No per-repository configuration

`lathe.toml` is no longer read and `lathe init` is gone. A target repository can't choose its agents' models, prompts or tiers any more; only the engineer's `~/.lathe` and per-run flags can. Tiers moved into agent configs, so routing runs in every repository for any agent that has tiers, and a provider, model or thinking flag turns it off for that run. Repositories that pinned a model in `lathe.toml` silently get the global choice.

## Seeding and drift

`~/.lathe/.seed.yaml` records, for every seeded file, the hash and the lathe version that wrote it. Before every capture lathe writes any missing file from the binary. A file that still matches its recorded seed is replaced only when the binary's seed differs and the binary's version is a release demonstrably newer than the recorded one. A rollback, the same version, or an unordered version such as `dev` leaves the file alone, so machines on different builds don't rewrite each other's seeds. Edited files are never touched; one whose seed changed since it was recorded is drift, and the dashboard flags it with a diff from the seed. Reset writes the binary's seed back and records it.

A missing or invalid file refuses capture with an error naming it. Nothing falls back to the embedded copy, since a hidden fallback is the second source this removes. Existing prompt overrides in the data root are not migrated.

Config and state are separate: `~/.lathe` holds only configuration, written owner-only. The trace database, runs and logs stay under the XDG data root.

Syncing `~/.lathe` with GitHub is a separate issue and not part of this decision.

## Known costs

Untouched seeds follow upstream releases, edited files don't. After an upgrade, an edited prompt keeps shadowing a seed that may have improved, and drift is the only signal. Development builds never update seeds, so someone running `dev` sees seed changes only by resetting a file.
