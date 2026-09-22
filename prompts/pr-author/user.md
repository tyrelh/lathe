{{request}}

## Report

End with one fenced json block and nothing after it:

```json
{
  "summary": "the template or convention you followed",
  "title": "one line, in the repository's style",
  "body": "the pull request body as markdown",
  "artifacts": []
}
```

All four keys are required. `body` is used verbatim, so it carries its own line
breaks and no surrounding quotes or fences. Do not mention lathe, the agents, or
the phases. Use [] for artifacts: you write no files.
