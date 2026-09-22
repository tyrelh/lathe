# Lathe

Lathe runs bounded agent workflows against a repository and records their work
for the engineer to inspect.

## Language

**Run**:
One execution of a workflow for an engineer's request against a repository.

**Workflow**:
The sequence of work needed to investigate, plan, implement or ship a requested
change. `implement` ends at tested code in the working tree; `build` is the
same run plus the branch, commit and pull request that ship it.

**Phase**:
A recorded stage of a run with an owner, an outcome and the work performed.

**Node**:
One phase in a workflow graph, declared in the order the happy path runs. A
node keeps its own state, its own agent session and its own send-back budget
across every entry.

**Target**:
Where a node says to go next when it finishes. Naming nothing forwards to the
node declared after it; naming an earlier node, or the node itself, is a
send-back.

**Round**:
How many times a node has been entered, counting from zero on its first entry.

**Send-back budget**:
How many send-backs one node may issue over a run. It counts send-backs
issued, not entries made, so a node re-entered by another node's loop keeps
its own budget intact. What happens once the budget is spent is the node's own
decision: plan review forwards its remaining objections as risks, and the test
loop fails the run.

**Agent**:
A role assigned part of a workflow, with tools and instructions appropriate to
that role.

**Artifact**:
A file an agent reports having written during its work.

**Plan**:
The proposed change, its ordered steps, the files it permits the builder to
write and its known risks.

**File list / write scope**:
The exact repository files the plan permits the builder to write. Listing a
file grants permission; it does not require a change to that file.

**Branch**:
The branch a build creates for its change before anything is written, named to
the repository's own convention. Lathe creates it; the agent only names it.

**Commit message**:
The complete message — subject and body — for the single commit a build makes
once its tests pass. Lathe stages the accepted scope and makes the commit; the
agent only writes the message.

**Pull request**:
The title and body a build opens its pushed branch with, following the
repository's template and its recently merged pull requests. Lathe pushes and
runs `gh pr create`; the agent only writes the text. Its URL is the one result a
run leaves that the checkout cannot reproduce, so it is saved as `pr.json`.

**Review round**:
One assessment of the current plan against the request and repository.

**Send-back**:
A node naming an earlier node as its target, returning work to whatever
produced it: a plan to the planner after review feedback or an unusable
review, an implementation to the builder after a measured red suite. It spends
one of the sending node's budget, even if nothing changes.

**Unresolved feedback**:
Objections remaining at the review limit, carried forward as plan risks. A
failed final review is also recorded as a risk because it supplied no usable assessment.

**Accepted plan**:
A plan the reviewer approved by returning no objections. A plan passed on at
the review limit may still have unresolved feedback and is not reviewer-approved.

**Correction**:
A request for an agent to repair an invalid report or an unsupported claim
within its current phase; it is separate from a plan send-back or a builder fix.
