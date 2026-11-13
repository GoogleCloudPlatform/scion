# Reincarnate --harness, Phase 3 (Cloud Run home leaf)

**Date:** 2026-11-12
**Branch:** scion/reincarnate-harness-p3
**Issues:** ptone/scion#7760 (phase), ptone/scion#7757 (tracking)
**Fork PR:** https://github.com/ptone/scion/pull/7788

Cloud Run home provisioning compares `RunConfig.HomeResetID` with the leaf's
reserved `.scion-home-reset-id`. A different pending ID clears every top-level
entry through `agenthome.Home.RemoveAll` before seeding. Symlinks are removed
without following their targets. The marker is written only after seeding and
harness-material cleanup succeed, so a partial clear or failed seed is retried
on the next start. Matching IDs preserve new-harness state; no pending ID keeps
the previous seed-over behavior. Invalid marker content or type triggers an
extra reset without following links; genuine I/O errors refuse the start.
The reserved marker is excluded from the broker-home copy and is not part of
the harness-material removal list.

The broker advertises `cloudrun` and `cloudrun-instances` in its reset-home
runtime capability, including registration names and heartbeat payloads.
Contract tests parse the actual broker advertisement through the hub parser
and cover both new names and the refusal for old brokers.

## Stack and agreed implementation adjustment

This phase stacks on P1, ptone/scion PR 7770, at `534e6e3` (original base
`d01b08b` plus its one-file hub lint fix). It must land after P1. Rebasing
onto the replacement P1 head required no conflicts or resolutions. Upstream
main was re-fetched before handoff and remained `3f1bdb7`.

Two unchanged commits are shared with P4, ptone/scion#7761:

- `607c63be0113b71fe549bab562647c27162598b2`: reader and RunConfig field.
- `6783eb6b7f4baf1bd2ea6164b73eb453b3c3cb40`: privatefs reader hardening.

Whichever phase lands second drops the shared commits on rebase. If P1 is
squash-merged, rebase onto upstream main and drop the P1 commits too.

The manager approved reading and validating `home-reset.json` in `pkg/agent`
and passing `HomeResetID` through `RunConfig`, instead of reading the record
inside `provisionCloudRunHome` as design section 3.5.2 originally described.
RunConfig has no broker agent-directory path, split homes cannot reliably
supply it through HomeDir, and runtime cannot import agent.

## Verification and review

The red reproductions showed old files surviving, a broker marker copied
onto the leaf, missing capability names, and invalid leaf metadata refusing
starts. The tests now exercise real Cloud Run start/provisioning with a fake
instance client and a local leaf, plus a filesystem fake that fails after a
real partial removal.

Independent review required the shared reader to reject symlinks, including
dangling links that previously looked like absent records. P4 supplied the
unchanged hardening commit above. Its bounded privatefs read refuses links,
non-regular files, privacy errors and oversized records. Regression tests
cover those cases. The reviewer fix-verified that patch and approved it;
the manager confirmed patch equivalence after rebase (message
f7032478-7fa9-493b-b509-b929b5100ec3). P3 runtime/capability code is unchanged
from reviewed head `f383f8d`.

Final checks on source head `0d38e96d9fe23902fb665460587d231c6d530051`:

All three Go test commands used this prefix:
`env -u SCION_PROJECT -u SCION_CREATOR -u SCION_MODEL -u HERMES_INFERENCE_MODEL GOGC=40`.

- `go test -buildvcs=false -timeout=30m ./pkg/agent -run 'TestReadHomeResetID|TestStartPassesPersistentHomeResetID|Test.*ResetHome' -count=1`: PASS (4.846s).
- `go test -buildvcs=false -timeout=30m ./pkg/runtime -run 'TestCloudRun' -count=1`: PASS (118.711s).
- `go test -buildvcs=false -timeout=30m ./pkg/runtimebroker -run 'TestHeartbeat.*Capabilities|TestHeartbeatService_ReportsResetHomeRuntimes|Test.*ResetHome' -count=1`: PASS (0.614s).
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/agent/... ./pkg/runtime/... ./pkg/runtimebroker/...`: PASS, actual output `0 issues.`
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 --tests=false ./pkg/hub/...`: PASS, actual output `0 issues.`
- `GOGC=40 go build -buildvcs=false -o /dev/null ./cmd/scion`: PASS, no output.
- `GOOS=windows GOARCH=amd64 GOGC=40 go build -buildvcs=false -o /dev/null ./cmd/scion`: PASS, no output.
- `bash hack/winsec-guard.sh`: PASS, `winsec-guard: ok (3130 files)`.
- `make fmt-check`: PASS, `Go formatting OK.`
- `git diff --check origin/scion/reincarnate-harness-p1..HEAD`: PASS, no output.

Additional earlier checks passed: `make fmt-check check-custom build`
(including 121 image-script cases), `make lint-ci`, and
`make vet-hub-shard SHARD=2` (no_sqlite and integration,tzcontract).
The initial `go test -buildvcs=false ./pkg/runtime ./pkg/runtimebroker`
passed runtime (340.795s) but exceeded the broker package's aggregate 10m
budget; `go test -buildvcs=false -timeout=30m ./pkg/runtimebroker` passed
(539.318s).

The independent reviewer ran `GOGC=25 make test-hub-shard SHARD=2` on frozen
head `f383f8d`: PASS, 5,375-test parity, 242s. Its named hub capability tests
also passed. Six existing opt-in/database/DEF-15 skips were reported; none
of the phase's changed tests was skipped. The manager approved reuse of
this frozen-head result (message 67212da0-519e-4820-86f5-452a8478a091).

## Limits and accepted environment failures

Full make ci/ci-full were not run because their whole-hub test compilation
is prohibited by repository instructions; the scoped/split checks above
were used instead. Windows execution tests were not run in this Linux
container; the cross-build and security guard passed. Live Cloud Run UAT is
deferred by ptone to the whole-feature end-to-end phase.

The broad `make test-fast-ci` attempt encountered inherited SCION_MODEL in
the Hermes default-model test, CPU-sensitive debounce/decode-cost failures,
and the agent package's aggregate 10m timeout. Clean named reruns passed:

- `env -u SCION_MODEL -u HERMES_INFERENCE_MODEL go test -tags no_sqlite -buildvcs=false ./harnesses -count=1`: PASS (55.900s).
- `env -u SCION_MODEL -u HERMES_INFERENCE_MODEL go test -tags no_sqlite -buildvcs=false -timeout=30m ./pkg/agent -run '^(TestMessageBuffer_DebounceResetsTimer|TestReadHomeResetID|TestStartPassesPersistentHomeResetID)$' -count=1`: PASS (1.578s).
- `go test -tags no_sqlite -buildvcs=false ./pkg/api -run '^TestCloneDepth_BoundedDecodeCostForLongNumbers$' -count=1`: PASS (0.817s).

The manager accepted these as environment-only failures (message
bd4a580a-4a5d-4273-ab16-ab11a942d729). No unrelated code was changed.
Early local broad/shard attempts also collected inputs before the final test
edits; stale generated test lists invalidated those attempts, so they are
not counted as passing gates. Final frozen-source focused checks and the
reviewer's valid frozen-head shard result supersede them, as agreed by the
manager. The earlier inherited P1 ineffassign finding was repaired in P1
and is absent from final upstream-main lint.

Raw developer evidence: `/scion-volumes/scratchpad/projects/reincarnate-harness-owner/verification/p3/`.
Independent review and fix evidence: the sibling `verification/p3-review/`
and `reviews/reincarnate-harness-p3-rev-report.md`.
