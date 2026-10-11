{{request}}

## Report

End with one fenced json block and nothing after it:

```json
{
  "summary": "the convention you found, and what the message says",
  "message": "the complete commit message: subject, blank line, body",
  "artifacts": []
}
```

All three keys are required. `message` is used verbatim, so it carries its own
line breaks and no surrounding quotes or fences. Omit the body only if the
repository's history omits it. Do not sign off as anyone, and do not mention
lathe, the agents, or the phases. Use [] for artifacts: you write no files.
