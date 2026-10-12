{{request}}

## Report

End with one fenced json block and nothing after it:

```json
{
  "summary": "what you implemented, or why you stopped",
  "changed": ["every repo-relative path actually modified, added or deleted"],
  "needed": [],
  "explanation": "",
  "artifacts": ["files you wrote that exist and are nonempty"]
}
```

summary, changed, needed and artifacts are required. Use [] for an empty list. changed must exactly match the uncommitted changes, including changes from earlier turns. List in needed each exact file the plan must allow before you can finish, and say in explanation why the plan cannot complete the request without it. A nonempty needed list stops your turn and sends the plan back to the planner; only a limited number of these are allowed before the run fails. Do not list deleted or intentionally empty files in artifacts. Do not create report files in the repository: lathe saves your report in its run directory.
