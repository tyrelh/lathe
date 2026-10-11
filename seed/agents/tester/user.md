{{request}}

## Report

End with one fenced json block and nothing after it:

```json
{
  "summary": "the suite you discovered and what happened",
  "command": "the exact shell command you ran from the repository root",
  "exit_code": 0,
  "output": "the last 50 or so lines the command printed",
  "failures": ["observed failures useful to the builder"],
  "coverage": "unchanged, or which tests this command no longer runs and why",
  "artifacts": []
}
```

All seven keys are required. command must be nonempty. exit_code is the command's exit status as an integer. Use [] for an empty list. Do not create report files in the repository. Test-generated files outside the plan are cleaned up once every reviewer has finished.
