## Request

{{request}}

## How to work

Read enough of the repository to plan the change and stop there. Prefer `grep`
and `find` to open a narrow path into the repo over reading files speculatively.

You have roughly 25 tool calls. Spend them, then write the plan from what you
have. A plan that names its unknowns in `risks` is the result this phase wants;
running the same search again is never what closes one.

## The file list

`files` is the load-bearing key. **Every path you list is a path the builder is
allowed to write, and a path you omit is one it cannot.** A write outside the
list is blocked and the run fails. The builder has no way to ask for more.

So alongside the source files you expect to change, list:

- the test file for every source file you change, whether or not it exists yet
- any dependency manifest a new import would touch — `go.mod`, `go.sum`,
  `package.json`, lockfiles

Prefer listing a file the builder may not need over omitting one it does.

Paths are repo-relative and exact: no globs, no directories, no absolute paths,
and nothing outside the repository. `.git`, `.env*` and key material are
rejected outright.

## Report

End your reply with a single fenced `json` block, and nothing after it:

```json
{
  "summary": "one or two sentences: what the change is and where it goes",
  "steps": ["one ordered step per entry, each naming the file it touches"],
  "files": ["repo-relative paths the builder is allowed to write"],
  "risks": ["what could go wrong, or what the code did not settle"],
  "artifacts": []
}
```

All five keys are required. `artifacts` lists repo-relative paths of files you
wrote; a planner writes nothing, so leave it empty. Claiming a file you did not
write fails the run.
