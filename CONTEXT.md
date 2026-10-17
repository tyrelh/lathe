# Lathe

Lathe runs bounded agent workflows against a repository and records their work for the engineer to inspect.

## Language

**Run**:
One execution of a workflow for an engineer's request against a repository.

**Iteration**:
One submitted request against a run and what became of it: its request, the configuration it ran with, its lifecycle and outcome, its reports, and the commits that explain where its work is. A run starts with iteration 0; each revision or recovery adds one. The run's displayed state follows its latest iteration; earlier ones stay inspectable. An iteration is not a worker attempt: the attempt is one execution of it.

**Revision**:
A further change requested against a build whose latest iteration succeeded and whose pull request is still open. It is the next iteration of the same run, not a new run: planned, validated and published to the same pull request, and published only if accepted.

**Recovery**:
An engineer-requested continuation of a failed, cancelled, or lost run as a new iteration of the same run. The unsuccessful iteration remains inspectable; the recovery has its own request, captured agent configuration, and bounded allowances. It continues the run's original resolved task with the engineer's guidance, restores saved work that was never published, and keeps the branch and pull request the run already owns. An iteration records its kind (submission, revision, recovery or restart), so a nonzero iteration is not necessarily a revision.

**Publication ownership**:
The branch, and pull request, a recovered build verifiably owns: recorded by lathe or uniquely matched on origin's repository and branch at the expected head. A recovery delivers only to what it owns, rechecks it immediately before every delivery, and carries it into later recoveries.

**Restart**:
An engineer-requested recovery from a verified base when the previous iteration's expected work cannot be restored. The new iteration records which prior work is unavailable rather than silently treating it as recovered.

**Workflow**:
The sequence of work needed to investigate, plan, implement or ship a requested change. `implement` ends at validated code in the working tree; `build` is the same run plus the branch, commit and pull request that ship it.

**Phase**:
A recorded stage of a run with an owner, an outcome and the work performed.

**Node**:
One phase in a workflow graph, declared in the order the happy path runs. A node keeps its own state, its own agent session and its own send-back budget across every entry.

**Target**:
Where a node says to go next when it finishes. Naming nothing forwards to the node declared after it; naming an earlier node, or the node itself, is a send-back.

**Round**:
How many times a node has been entered, counting from zero on its first entry.

**Send-back budget**:
How many send-backs one node may issue over a run. It counts send-backs issued, not entries made, so a node re-entered by another node's loop keeps its own budget intact. What happens once the budget is spent is the node's own decision: plan review forwards its remaining objections as risks, and adjudication hands the implementation off unaccepted with its remaining findings.

**Validation group**:
The `validate` node: the tester and the code reviewers run at once against the same uncommitted implementation, each as its own traced phase. The tree is frozen while they run, cleanup waits for all of them, and a source file changed meanwhile invalidates the round.

**Worker**:
One member of a validation group. A worker that fails to produce a usable result is retried up to twice against the same code; a report with findings, or a red suite, is a result and is not retried.

**Builder**:
The agent that writes the files named by the plan and carries out changes requested by the adjudicator.
_Avoid_: implementer

**Plan-reviewer**:
The agent that checks a plan against the repository before the builder receives its write scope.
_Avoid_: reviewer

**Code reviewer**:
A read-only worker that reports findings on an implementation from its own angle: correctness, security, unnecessary complexity, or an adversarial attempt to break the change. Angles overlap on purpose; the adjudicator weighs them.
_Avoid_: reviewer (ambiguous with the plan-reviewer)

**Finding**:
One code reviewer's objection: a stable reference lathe assigns, its source, a location where one applies, evidence, an explanation and the requested outcome.

**Adjudication**:
The adjudicator's decision on one validation round: every finding fixed or dismissed with a reason, and either acceptance or one set of changes for the builder. Only the adjudicator sends an implementation back.

**Validation outcome**:
How validation ended. Accepted needs a usable report from every worker and a green suite from the tester. Unresolved is findings still to fix after the last send-back; incomplete is a worker that exhausted its retries; invalidated is source that changed while the workers ran. `environment_failed` is a reported test exit of 126 or 127: terminal after one tester attempt, with command/result/output evidence retained and already-running reviewers joined before cleanup. Unlike retryable worker failures and publishable incomplete validation, environment failures receive no adjudication, repairs, commit, push or pull request. `build` publishes unresolved and incomplete work as a draft pull request and still fails the run.

**Agent**:
A role lathe runs in the harness, with its own tools, instructions and configuration. Most agents do part of a workflow; planned scorers will grade a run after it (ADR 0008).

**Agent configuration**:
What one agent is told and runs on, held per machine and owned by the engineer: its system and user prompts, provider, model, thinking, timeout, tiers and whether it receives the repository's instructions. It is the only source of these, captured at submission, so an edit only changes work submitted after it. Every agent's configuration is complete on its own; there is no shared defaults layer and no per-repository layer.
_Avoid_: prompt override, custom prompt, defaults

**Seed**:
The agent configuration shipped with lathe. It is written on first start, fills in anything missing after an upgrade, and replaces files the engineer never edited. It never overwrites an edit.
_Avoid_: built-in prompt, default prompt

**Drift**:
An agent configuration file the engineer edited whose seed has changed since. Lathe flags it and leaves the edit in place.

**Agent permissions**:
What an agent may do: its tools, protected paths and denied commands. Shipped with lathe and not part of agent configuration, so editing configuration never widens what an agent can do.

**Thinking**:
How much reasoning an agent's model is asked to spend.
_Avoid_: effort

**Harness**:
The coding-agent runtime an agent's phases run in; Pi today.
_Avoid_: agent (for the runtime)

**Tier**:
One of an agent's engineer-configured model choices, ordered from cheapest to most capable, each described by the kind of task it suits. One tier per agent is the default, used whenever routing cannot decide.
_Avoid_: route, model class

**Routing**:
The engineer-owned phase that picks one of an agent's tiers for the rest of an iteration, from the task in front of that agent. A run started with a provider, model or thinking flag is not routed.
_Avoid_: model selection

**Artifact**:
A file an agent reports having written during its work.

**Plan**:
The proposed change, its ordered steps, the files it permits the builder to write and its known risks.

**File list / write scope**:
The exact repository files the plan permits the builder to write. Listing a file grants permission; it does not require a change to that file.

**Scope request**:
A builder's report that the current plan omits files needed to complete the request. It asks the planner to reconsider the plan; the planner may choose another approach instead of granting the requested files. Two are answered per iteration; the third ends it.

**Disposition**:
What a reviewed plan does with work it finds: uncommitted paths it keeps in its files or abandons as reversals (saved, then reverted), and outstanding findings it dismisses with a reason. It is applied only after review, before the builder runs.

**Branch**:
The branch a build creates for its change before anything is written, named to the repository's own convention. Lathe creates it; the agent only names it.

**Commit message**:
The complete message — subject and body — for the single commit a build makes for its change, whether validation accepted it or it ships as a draft. Lathe stages the accepted scope and makes the commit; the agent only writes the message.

**Pull request**:
The title and body a build opens its pushed branch with, following the repository's template and its recently merged pull requests. Lathe pushes and runs `gh pr create`; the agent only writes the text. Its URL is the one result a run leaves that the checkout cannot reproduce, so it is saved as `pr.json`.

**Review round**:
One assessment of the current plan against the request and repository.

**Send-back**:
A node naming an earlier node as its target, returning work to whatever produced it: a plan to the planner after review feedback or an unusable review, an implementation to the builder after an adjudication asks for changes. It spends one of the sending node's budget, even if nothing changes.

**Blocking objection**:
A review objection that means the plan cannot succeed as written. It is sent back like any other, but one still standing at the review limit fails the run instead of becoming a risk.

**Unresolved feedback**:
Objections remaining at the review limit, carried forward as plan risks. A failed final review is also recorded as a risk because it supplied no usable assessment.

**Accepted plan**:
A plan the reviewer approved by returning no objections. A plan passed on at the review limit may still have unresolved feedback and is not reviewer-approved.

**Correction**:
A request for an agent to repair an invalid report or an unsupported claim within its current phase; it is separate from a plan send-back or a builder repair. A group worker's corrections and retries share one allowance.

**Scorer**:
Planned in ADR 0008: an agent that grades a finished build run with a pull request against one rubric, outside the run. Each scorer judges the whole run from its own angle, such as plan quality or review signal, and is started by the engineer.
_Avoid_: judge, grader

**Score**:
Planned in ADR 0008: one scorer's 0-100 judgment of a run, with its rationale. A run has at most one score per scorer; scoring again replaces it. Scoring is not a phase: it never changes the run's state, outcome or cost.
