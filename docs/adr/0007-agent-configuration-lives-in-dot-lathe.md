# Agent configuration lives in ~/.lathe

Supersedes [0006](0006-prompt-overrides-are-machine-wide.md).

What an agent is told and runs on used to come from three layers: the roster and prompts compiled into the binary, prompt overrides in the data root, and a target repository's `lathe.toml`. Reading the effective configuration meant merging all three in your head, and only prompts could be changed without a rebuild. Agent configuration now lives in plain files under `~/.lathe`, and those files are the only source. The binary seeds them on first start and never reads its own copy at capture.

## One file set per agent, no layers

Each agent has `agents/<name>/config.yaml`, `system.md` and `user.md`. Routing prompts live in `routing/<name>.md`. Every `config.yaml` names provider, model, thinking and timeout explicitly; tiers and `repo_instructions` are optional. There is no defaults file. The seeded files are the defaults, so what you read in an agent's dir is what that agent runs with. Changing a provider everywhere means editing every agent, and we accept that cost to keep a single place to look.

The four code reviewers used to share one user prompt. Each now gets its own seeded copy, so specializing a reviewer is an edit, not a roster change.

YAML replaces TOML so the dashboard can rewrite a field through `yaml.v3` nodes without dropping hand-written comments.

## Permissions stay compiled

0006 kept `roster.toml` out of the overlay because it decides each agent's tools, protected paths and `bash_denied`. That reasoning still holds and gets sharper once configuration can arrive from a GitHub repo. Agent permissions and the set of agents stay in the binary. Agent configuration can change what an agent is told and which model runs it, never what it may do. A config dir for an agent the binary doesn't know is ignored.

## No per-repository configuration

`lathe.toml` is no longer read and `lathe init` is gone. A target repository can't choose its agents' models, prompts or tiers any more; only the engineer's `~/.lathe` and per-run flags can. Tiers moved into agent configs, so routing runs in every repository for any agent that has tiers. Repositories that pinned a model in `lathe.toml` silently get the global choice.

## Seeding and drift

`~/.lathe/.seed.yaml` records, for every seeded file, the hash and the lathe version that wrote it. On load lathe writes any missing file from the binary, replaces files that still match their recorded seed when the binary's seed is newer, and leaves edited files alone, flagging them as drift. A missing or invalid file refuses capture with an error naming it. Nothing falls back to the embedded copy, since a hidden fallback is the second source this removes. Existing prompt overrides in the data root are not migrated.

Config and state are separate: `~/.lathe` holds only what is worth versioning. The trace database, runs and logs stay under the XDG data root.

## Syncing with GitHub

`~/.lathe` can be connected to `<owner>/lathe-config` from the dashboard's Settings page, using `gh` auth. Lathe creates the repository private, or adopts it if it exists. Having an `origin` remote is what turns GitHub sync on, and removing it turns sync off.

Dashboard saves and hand edits are committed locally and pushed to `main`. A rejected push is rebased once. If the rebase conflicts, the local commit goes to a `lathe/conflict-<host>-<time>` branch, the clone resets to `origin/main`, and a banner links the compare. GitHub wins `main`, and no edit is lost.

Lathe polls for changes with conditional requests and fetches before each capture. A fetched commit is validated before checkout. If it fails, the machine stays on its last good commit and a banner says so. One bad push can't stop work on every machine. Seed updates are forward-only by lathe version, so machines on different releases don't keep rewriting each other's seeds. Each iteration records the config commit it captured, alongside the snapshot that already freezes the full text. The created repository gets a `validate.yml` workflow that runs `lathe validate`.

## Known costs

Anyone who can push to the config repository steers what agents are told on every synced machine. Permissions being compiled limits that to instructions and model choice, but instructions alone can do a lot. Committing straight to `main` also rules out branch protection on that repository.

Untouched seeds follow upstream, edited files don't. After an upgrade, an edited prompt keeps shadowing a seed that may have improved, and drift is the only signal.
