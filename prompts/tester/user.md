{{request}}

## Report

End with one fenced json block and nothing after it:

```json
{
  "summary": "the suite you discovered and what happened",
  "command": "the exact shell command to run from the repository root",
  "failures": ["observed failures useful to the builder"],
  "coverage": "unchanged, or which tests this command no longer runs and why",
  "artifacts": []
}
```

All five keys are required. command must be nonempty. Use [] for an empty list.
Do not report a passed key. Do not create report files in the repository.
Test-generated files outside the plan are cleaned up once every reviewer has finished.
