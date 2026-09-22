{{request}}

## How to work

Use read, grep, find and ls to verify the plan against the repository. You have
roughly 25 tool calls; spend them on evidence that could change your decision.
On a resumed review, assess the full revised plan against earlier objections
and check whether its changes introduced another concrete problem.

## Report

End your reply with a single fenced json block and nothing after it:

```json
{
  "summary": "brief assessment of whether the plan is ready",
  "feedback": [],
  "artifacts": []
}
```

All three keys are required. Empty feedback means accept. Otherwise each entry
must describe an unresolved objection; omit objections the revision resolved.
You write no files, so artifacts must be empty.
