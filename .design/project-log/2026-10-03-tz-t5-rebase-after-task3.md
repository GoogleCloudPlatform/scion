# tz-refactor task 5: rebase onto upstream main after task 3 merged

Date: 2026-10-03. Branch `scion/tz-t5`, fork PR ptone/scion#2686.

## What changed
- tz-refactor task 3 landed upstream as one squash commit (GoogleCloudPlatform/scion#2289, 8ab2e62e4).
  The branch still carried the old pre-squash task 3 commits.
- Ran `git rebase --onto <upstream main db688ba92> c7c8e50`, which replays only the seven task 5 commits.
  The old head was 89a230ec.
- There were no conflicts. `git range-diff` shows all seven commits as identical (`=`).
  The cumulative task 5 patch matches the old one byte for byte, apart from blob IDs and hunk line offsets.
- No code delta. The only file both tasks touch is `pkg/hub/handlers_chat_v2.go`.
  Task 5's `.UTC()` fix there (routed message `Timestamp` in `sendAgentRouted`) is in code task 3 did not change.
  Task 3 only added the 400 branch for an invalid search cursor.

- While fork CI ran, upstream main moved to 18cd8a512, so the PR became conflicting.
  It was rebased again onto 18cd8a512.
  - The only conflicts were in the CI wiring of `build: add make time-literals regression gate`.
    Upstream had added the `check-setenv-guard` and `check-route-authz-manifest` gates in the same places.
  - Resolution: keep both. `time-literals` goes into `.PHONY` and at the end of `check-custom`.
    The `Check Time Literals` CI step comes after `Check Route Authorization Manifest`.
  - The range-diff shows only that commit as changed, and only in those Makefile lines. The other seven are identical.

## Evidence
- `./hack/check-time-literals.sh` on the rebased tree: no violations (476 files scanned on 18cd8a512, 0 allowlisted).
  Upstream code added since the old base does not trip the gate.
- Upstream's new `check-setenv-guard` and `check-route-authz-manifest` both pass on the rebased tree.
  `check-annotation-prefix` fails on `pkg/hub/seed.go`, but it fails identically on unmodified upstream main 18cd8a512, so it is not from this branch.
- `go test -p 2 ./hack/checktimeliterals/...` passed.
- `go test -p 2 -count=1` on `./pkg/store/...`, `pkg/hubsync`, `pkg/runtime/cloudrun`, `pkg/sciontool/hub`, `pkg/hub/auditevent` and `pkg/hub/githubapp` passed under both `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu`.
- `go build -buildvcs=false ./...`, `go vet` and gofmt on the changed packages: clean.
- `golangci-lint --new-from-rev=<upstream main>` on the changed packages: 0 issues.
- The full `pkg/hub` suite was left to fork CI.

## Follow-ups
- None from the rebase.
