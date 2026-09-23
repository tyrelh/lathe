# Phases choose their own target and count their own send-backs

A workflow is an ordered list of nodes. A node is entered, does its work, and
names where to go next: nothing forwards to the node declared after it, a name
jumps to that node, and an error ends the run. Declaration order is the happy
path, so a workflow still reads top to bottom.

A target declared earlier than the current node, or the node itself, is a
send-back and spends that node's budget; a target declared later does not. The
budget counts send-backs issued rather than entries made, because two
overlapping loops otherwise exhaust the inner one: every forward out of the
builder passes through the tester, so review send-backs would spend the
tester's rounds on suites that were green each time. Counting per node keeps
total work additive and known before the run starts.

The runner counts and exposes the count; it has no exhaustion policy. The two
loops want different ones — plan review forwards with residual objections
folded into risks, the test loop fails the run — so a node reads
`SendBacksLeft` and decides for itself. The runner still refuses a send-back
past budget, as a guard rather than a policy.

The graph holds one agent session per node and passes it on every re-entry, so
a resumed agent keeps the context it already paid for without a workflow
threading session ids by hand. Data between nodes stays in the workflow's own
typed locals, captured by the node closures: no payload plumbing, nothing that
loses its type on the way through.

This replaced the `verify`, `replan` and `fix` phases. `verify` merged into
`test`, which discovers a command on its first entry and measures it on every
entry — discovery is a claim, and the exit code from `Handle.Command` remains
the only thing that decides where the node goes. It also replaced
`RecoverableError`: a node that wants a failed phase inside a continuing run
calls `Entry.Failed` and returns its target, which is the outcome 0001
describes with no wrapper type at the call site.

Since 0004 the test loop is gone: `test` is one worker in the `validate` group, and the adjudicator owns the send-back to the builder. Its policy at zero is to hand the work off unaccepted with its remaining findings. The group is the one place phases run concurrently; it never sends back, so the counting rule here is unchanged.
