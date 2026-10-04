# tz-refactor task 8: rebase onto upstream main

Refs ptone/scion#2501, ptone/scion#2457.

## What changed

- `scion/tz-t8` rebased onto upstream main bfd473a56, which includes the merged
  tz-refactor task 3 squash (GoogleCloudPlatform/scion#2289, 8ab2e62e4). Only this
  task's five commits were replayed (`git rebase --onto <upstream main> 48bde03`).
- No conflicts. `git range-diff` reports all five commits as identical. No code delta.

## Verification

- `go build -buildvcs=false -p 2 ./cmd/... ./pkg/hub/...`
- `go vet -tags tzcontract ./pkg/hub/tzcontract/`, `gofmt -l pkg/hub/tzcontract` (clean)
- `golangci-lint run --concurrency=1 --build-tags tzcontract --new-from-rev=<upstream main> ./pkg/hub/tzcontract/...`: 0 issues
- Contract test (`make test-tz-contract`) and fork CI on ptone/scion#2687: see the report.
