# Plan review is a phase, not a gate

A plan's file list bounds what the builder can write, so omissions deserve an
independent review before implementation. Review is an agent phase because it
judges the plan against the repository; gates only check claims mechanically.
The workflow permits four send-backs, records unusable reviews as failed phases
without necessarily failing the run, and carries final unresolved objections
into the plan's risks so the saved plan and builder handoff agree.
