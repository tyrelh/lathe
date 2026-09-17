## Request

{{request}}

## How to work

Read what you need to answer the request and stop there. Prefer `grep` and
`find` to open a narrow path into the repo over reading files speculatively.

## Report

End your reply with a single fenced `json` block, and nothing after it:

```json
{
  "summary": "one or two sentences answering the request",
  "findings": ["one fact per entry, each naming the file it came from"],
  "artifacts": []
}
```

All three keys are required. `artifacts` lists repo-relative paths of files you
wrote; a scout writes nothing, so leave it empty. Claiming a file you did not
write fails the run.
