# tz-refactor task 8: merge upstream main and bot review

Refs ptone/scion#2501, ptone/scion#2457.

## What changed

- Merged upstream main eb164b8b into `scion/tz-t8` as a merge commit.
- The one conflict was in `.github/workflows/ci.yml`. It was resolved by
  taking upstream's PR CI layout and keeping the timestamp contract step as
  the last step of the Postgres launch store job, where it runs the SQLite and
  Postgres cases. The workflow diff against upstream main is just that step
  and its comment.
- The contract test's SQLite readability scan now checks `Err()` after each
  `database/sql` row loop, so an iteration error fails the test instead of
  cutting the scan short.

## Verification

- `go build -buildvcs=false -p 2 ./...`; `go vet` with default, `no_sqlite`,
  `tzcontract` and `integration volume_test tzcontract` tags.
- `golangci-lint run --concurrency=1 --build-tags tzcontract --new-from-rev=<upstream main> ./pkg/hub/tzcontract/...`: 0 issues.
- `./hack/check-time-literals.sh` and `./hack/check-cli-time-zones.sh`: clean.
- Contract test (SQLite) under TZ=Asia/Tokyo and Asia/Kathmandu: pass.
  Fork CI covers Postgres.
