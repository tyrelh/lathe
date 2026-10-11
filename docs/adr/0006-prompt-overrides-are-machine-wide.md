# Prompt overrides are machine-wide

Superseded by [0007](0007-agent-configuration-lives-in-dot-lathe.md).

Every prompt is compiled into the binary. `assets.go` embeds `roster.toml` and `prompts/`, so changing what an agent is told meant editing the source and running `make build`. A prompt override replaces one built-in prompt without a rebuild. It lives in lathe's data root at the same path as the prompt's key, so `prompts/planner/system.md` is overridden by `~/.local/share/lathe/prompts/planner/system.md`, or `$XDG_DATA_HOME/lathe/prompts/planner/system.md` when that is set. There is one layer per machine, and it applies to every repo lathe runs against. The dashboard's Agents page saves and resets overrides; they are plain files, so editing them by hand works too.

## Not per repo, and not in the source tree

A per-repo override would live in the target repository, next to `lathe.toml`. The target's content is what agents read and act on, and `lathe.toml` is deliberately limited to provider, model, thinking, `repo_instructions` and routing tiers. Prompts there would let a repository steer its own agents: tell the code reviewers what to skip, or the adjudicator what to accept. That is a much bigger grant than choosing a model, and per-repo prompts are out of scope.

Editing the source tree is what already worked, and it needs a rebuild, which is the cost this removes. The data root is the one place lathe already owns per machine; the trace database lives there too.

## Only prompts are overlaid

`config.Overlay` serves a name under `prompts/` from the data root when that file exists, and everything else from the embedded assets. `roster.toml` is never overlaid. The roster decides each agent's tools, the protected paths and `bash_denied`, which makes it the trust boundary, so it stays what the binary shipped with plus what `lathe.toml` may set. An override only changes what an agent is told.

The dashboard never takes a path from a request. A save names an agent and a role, the key comes from the roster, and a key that is not under `prompts/` or whose path would leave the data root is rejected. Symlinked directories under the data root are followed, so prompts can live in a dotfiles repo.

## Captured at submission

Overrides are read when work is captured, like the rest of the configuration. Everything that makes a snapshot loads the roster through the overlay: runs from the CLI or the dashboard, revisions, and `route-calibrate`. The snapshot freezes the prompt text each agent will use, so an edit only changes work submitted after it, and queued or running work keeps what it captured. Runs are not marked as using an override, since the snapshot already records the full prompt text.

## Shared prompts stay shared

An override belongs to a key, not an agent. The four code reviewers name the same user prompt, `prompts/code-review/user.md`, so they read and write the same override, and the dashboard warns before saving it. Specializing one reviewer is a roster change that gives it its own key, and overrides follow the keys.

A save carries the hash of the body it started from and is refused if the effective prompt changed since, whether in another tab or by hand. A body equal to the built-in removes the override rather than writing it, so an active override always differs from the built-in.

## Known costs

Overrides mask upstream changes. After an upgrade, an override keeps shadowing a built-in that may have improved. Recording the built-in's hash on save and flagging drift on the agent page is a follow-up.

Renamed keys orphan overrides. If a roster key changes, the override file at the old path is silently ignored.

Editing a prompt is a strong write. Whoever can save one steers agents that write files and run shell commands. The routes sit behind the same boundary as submitting a run: a loopback listener, a host allow-list, and same-origin JSON writes. That boundary doesn't widen, though more now depends on it. This adds no new auth.
