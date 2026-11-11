# tz-refactor task 5: rebase onto upstream main c0a69140a

Date: 2026-10-03. Branch `scion/tz-t5`, fork PR ptone/scion#2686.

## What changed
- Upstream main moved past the old base 18cd8a512, and the PR became conflicting.
  The branch (old head 2d5f8e0a2) was rebased onto upstream main c0a69140a.
- The only conflicts were in the CI wiring of `build: add make time-literals regression gate`.
  Upstream had added the sibling `cli-time-zones` gate (`hack/check-cli-time-zones.sh`) in the same three Makefile places and at the same CI step position.
  - Resolution: keep both. `time-literals` follows `cli-time-zones` in `.PHONY` and at the end of `check-custom`.
    The `make time-literals` target comes after the `cli-time-zones` target.
    The `Check Time Literals` CI step comes after `Check CLI Time Zones`.
- `git range-diff` shows only that commit as changed, and only in those Makefile lines.
  The other seven commits are identical.
- No code delta. Upstream changed none of the 20 files that carry this task's `.UTC()` fixes since the old base.

## Evidence
- `./hack/check-time-literals.sh` on the rebased tree: no violations (481 files scanned, 0 allowlisted).
  Upstream code added since the old base does not trip the gate.
- Upstream's `check-cli-time-zones` passes on the rebased tree.
  So do `check-annotation-prefix`, `check-setenv-guard`, `check-route-authz-manifest`, `check-authz-guards` and `check-conversation-upsert-guard`.
- `go test -p 2 -count=1 ./hack/checktimeliterals/...` passed under UTC, `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu`.
- `go test -p 2 -count=1` on `./pkg/store/...`, `pkg/hubsync`, `pkg/runtime/cloudrun`, `pkg/sciontool/hub`, `pkg/hub/auditevent` and `pkg/hub/githubapp` passed under both `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu`.
- `go build -buildvcs=false ./...`, `go vet` and gofmt: clean.
- `golangci-lint --new-from-rev=<upstream main>` on `hack/checktimeliterals`: 0 issues.
- The full `pkg/hub` suite was left to fork CI.

## Follow-ups
- None from the rebase.
