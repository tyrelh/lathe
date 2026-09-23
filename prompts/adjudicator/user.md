{{request}}

## How to work

Every report, the measured test result and your own earlier decisions are above. Use read, grep, find and ls when you need the code itself to decide; you have roughly 25 tool calls. When no send-backs remain, the findings you decide to fix are handed off unresolved with the work, so decide them as carefully as any other round.

## Report

End your reply with a single fenced json block and nothing after it:

```json
{
  "summary": "your assessment of this round",
  "verdict": "accept or revise",
  "decisions": [
    {"ref": "security-1.1", "action": "fix or dismiss", "reason": "why"}
  ],
  "changes": "the one set of changes the implementer makes, with the reason for each; \"\" when accepting",
  "scope": [{"path": "repo/relative/file.go", "reason": "why the fix needs it"}],
  "amendments": [{"change": "the simplification to the plan", "reason": "why the requirements still hold"}],
  "artifacts": []
}
```

All seven keys are required. Decide every finding listed above exactly once, by its ref. Accept only with a green measured suite and no finding decided fix; scope and amendments must be empty when you accept. A revise verdict needs nonempty changes. Scope paths are exact repo-relative files, never globs. You write no files, so artifacts must be empty.
