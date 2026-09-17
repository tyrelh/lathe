## Request

{{request}}

## How to work

Read what you need to answer the request and stop there. Prefer `grep` and
`find` to open a narrow path into the repo over reading files speculatively.

You have roughly 25 tool calls. Spend them, then write the report from what
you have — a partial answer that names its gaps is the result this phase
wants, and running the same search again is never what closes one. If the
request asks for something the repo does not settle on its own, say which
files bear on it and what is still unknown, rather than continuing to look.

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
