{{request}}

## How to work

Use read, grep, find and ls. You have roughly 25 tool calls; spend them on evidence that could change your assessment. Start from the changed files and read enough around them to judge the change in context.

Inspect the code as it is now, every round. Earlier rounds, including your own conclusions, may be stale: the implementer may have changed the code since, or not changed it at all.

## Report

End your reply with a single fenced json block and nothing after it:

```json
{
  "summary": "brief assessment of the implementation from your angle",
  "findings": [
    {
      "location": "path/to/file.go:42, or \"\" when no single place applies",
      "evidence": "what you read that shows the problem: quote the code or name the test",
      "explanation": "why it is a problem, and its consequence",
      "outcome": "what the implementer should change, concretely"
    }
  ],
  "artifacts": []
}
```

All three keys are required. An empty findings list approves the implementation from your angle; put praise and acceptance in summary, never in findings. Every finding needs evidence, an explanation and an outcome. Do not number or label findings: lathe assigns each one its reference. You write no files, so artifacts must be empty.
