# Plan review is a phase, not a gate

A plan's file list bounds what the builder can write, so omissions deserve an
independent review before implementation. Review is an agent phase because it
judges the plan against the repository; gates only check claims mechanically.
The workflow permits four send-backs, records unusable reviews as failed phases
without necessarily failing the run, and carries final unresolved objections
into the plan's risks so the saved plan and builder handoff agree.

One exception: an objection the reviewer marks blocking, because the plan cannot succeed as written (a file it needs and does not list, a step it concedes it cannot perform), is never carried forward as a risk. It goes back to the planner like any objection, and if it still stands when the send-backs run out, the run fails before the builder starts. Forwarding a plan that says it will fail only spends a builder turn finding that out.
