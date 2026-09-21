# Lathe

Lathe runs bounded agent workflows against a repository and records their work
for the engineer to inspect.

## Language

**Run**:
One execution of a workflow for an engineer's request against a repository.

**Workflow**:
The sequence of work needed to investigate, plan or build a requested change.

**Phase**:
A recorded stage of a run with an owner, an outcome and the work performed.

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

**Review round**:
One assessment of the current plan against the request and repository.

**Send-back**:
A request for the planner to return a complete plan after review feedback or
an unusable review. It consumes one opportunity to revise, even if nothing changes.

**Unresolved feedback**:
Objections remaining at the review limit, carried forward as plan risks. A
failed final review is also recorded as a risk because it supplied no usable assessment.

**Accepted plan**:
A plan the reviewer approved by returning no objections. A plan passed on at
the review limit may still have unresolved feedback and is not reviewer-approved.

**Correction**:
A request for an agent to repair an invalid report or an unsupported claim
within its current phase; it is separate from a plan send-back or a builder fix.
