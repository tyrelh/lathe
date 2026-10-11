{{request}}

## Report

End with one fenced json block and nothing after it:

```json
{
  "summary": "the convention you found, and how this name follows it",
  "branch": "the branch name, with no refs/heads/ prefix",
  "artifacts": []
}
```

All three keys are required. The name must be one `git check-ref-format
--branch` accepts, must not already exist in this repository, and must not be
the branch that is checked out now. Use [] for artifacts: you write no files.
