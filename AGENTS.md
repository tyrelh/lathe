# Project conventions

## Comments

Comments should be concise. Function documentation comments should only talk about function i/o and relevant side effects. Should not explain context outside the function.

## Formatting

Go code must be gofmt-clean. CI checks it separately from the tests.

```sh
make fmt      # lists unformatted files and fails if any
gofmt -w .    # fixes them
```

## Tests

Run the suite from the repository root. It needs Go 1.27; Node is optional, and the guard tests skip without it.

```sh
make test                                       # go test ./..., what CI runs
go test ./internal/workflow                     # one package
go test -run TestRevisionPublication ./internal/workflow   # one test
go test -count=1 ./...                          # skip the test cache
```

`internal/workflow` takes about two minutes uncached because its tests run whole workflows against stub agents; the other packages finish in seconds. A fast `make test` usually means that package was cached.
