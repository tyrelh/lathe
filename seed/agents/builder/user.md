{{request}}

## Report

End with one fenced json block and nothing after it:

```json
{
  "summary": "what you implemented, or why you stopped",
  "changed": ["every repo-relative path actually modified, added or deleted"],
  "needed": [],
  "artifacts": ["files you wrote that exist and are nonempty"]
}
```

All four keys are required. Use [] for an empty list. changed must exactly match
the uncommitted changes, including changes from earlier turns. List any file the
plan missed in needed; a nonempty needed list ends the run as a failure. Do not
list deleted or intentionally empty files in artifacts. Do not create report
files in the repository: lathe saves your report in its run directory.
