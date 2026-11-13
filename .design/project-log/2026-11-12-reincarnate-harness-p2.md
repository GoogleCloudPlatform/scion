# Reincarnate --harness, Phase 2 (credential preflight)

**Date:** 2026-11-12
**Branch:** scion/reincarnate-harness-p2
**Issues:** ptone/scion#7759 (phase), ptone/scion#7757 (tracking)
**Base:** upstream main ceded71, including P1 merged in
GoogleCloudPlatform/scion#4784 (74a52f1). Rebased onto upstream main on
2026-11-13 without conflicts, dropping all P1 commits from this phase's range.
Only P2 commits remain in `git log upstream-main..HEAD`.

## Behavior

After building the fresh config for a harness switch, the hub checks target-harness
credentials with `hasRequiredAuthCredentials`, using the fresh applied config and
the target's auth metadata. Missing credentials produce 422
`harness_credentials_missing` before the agent row, history or dispatcher changes.
Dry runs and real requests take the same path. Explicit `NoAuth` and
`--harness-auth none` skip the probe.

The existing drop-to-shell fallback refusal uses the same response formatter.
Errors list only config-declared env keys, file names and alternative keys, sorted
and deduplicated; automatic auth selection lists requirements across declared auth
types. No credential values are read into the response. The hint names hub secrets
and `--harness-auth`. The existing CLI API-error path already prints that message,
so this phase needs no CLI change.

## Limitations

The hub cannot see credentials present only in a broker's environment and can
refuse those requests even when the broker could authenticate. Hosted deployments
use hub secrets. The broker's reprovision gather remains the backstop if the hub
accepts credentials the broker cannot obtain, and runs before the home reset.
The experiment and runtime capability list remain those supplied by P1.

## Verification before P1 upstream merge

- RED: stable `make test-hub-shard SHARD=2` failed only the new credential
  preflight test, all four env/file × automatic/explicit cases (200 instead of
  422); shard test-list validation passed. Full run: 615s, five existing skips.
- GREEN: shard-2 binary `hub.test -test.v -test.run '^TestReincarnateHarness_'`
  passed, including dry-run/real refusal parity, names without values, unchanged
  row/history/dispatcher on 422, explicit no-auth skips, and successful env/secret
  credentials. The complete `make test-hub-shard SHARD=2` passed after the final P1 rebase:
  207s, four existing
  skips (optional soak, Ent-only group deletion, PostgreSQL-only topics, and
  the existing DEF-15 attachment case). Test-list validation: 5358 tests compiled
  as expected.
- `make fmt-check check-custom build`: passed (all custom checks; 121 image-script
  checks; Linux binary built).
- `GOGC=40 go vet -overlay /tmp/p2-final-vet-overlay/overlay.json -tags
  hubshard,hubshard_2 ./pkg/hub`: passed. The overlay was generated with
  `hack/hubshard/hubshard.sh -overlay /tmp/p2-final-vet-overlay`.
- `GOMAXPROCS=2 GOOS=windows GOARCH=amd64 go build -p 1 -buildvcs=false
  -o /dev/null ./cmd/scion`: passed.
- `bash hack/winsec-guard.sh`: passed (`winsec-guard: ok (3130 files)`).
- Final `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1
  --tests=false ./pkg/hub/...`: passed, exact output `0 issues.`. The inherited
  P1 ineffassign reported at initial submission was fixed on the P1 branch and
  resolved by the conflict-free rebase; P2 did not change that code.
- After the P1 update, fmt/custom/Linux build, shard-overlay vet, Windows build
  and winsec were re-run and passed. The final full shard run also passed (207s).


`make ci` was replaced by scoped equivalents because its `lint` and `test-fast`
targets compile the entire hub test package, which this repository forbids.
Hub lint uses `--tests=false` per the repository's split-lint convention; shard
vet covers tests. No whole hub test package was compiled.

## Verification after P1 upstream merge (2026-11-13)

Rebased onto upstream main `ceded71b43b1373a2833f03d455a67bb34e34e50` after
P1 landed in GoogleCloudPlatform/scion#4784 (`74a52f1`). No conflicts or manual
resolutions. All P1 commits were dropped; the range above upstream main contains
only this phase's implementation and project-log commits.

- `hack/hubshard/hubshard.sh -overlay /tmp/p2-landed-overlay`, then
  `GOGC=25 go test -p 1 -overlay /tmp/p2-landed-overlay/overlay.json -tags
  hubshard,hubshard_2 -run '^TestReincarnateHarness_' -count=1 ./pkg/hub`:
  passed (`ok .../pkg/hub 1.998s`). This reruns all 12 focused harness-switch
  tests from shard 2, including the credential preflight and P1 regressions.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1
  --tests=false ./pkg/hub/...`: passed, exact output `0 issues.`.
- `GOMAXPROCS=2 GOOS=windows GOARCH=amd64 go build -p 1 -buildvcs=false
  -o /dev/null ./cmd/scion`: passed.
- `bash hack/winsec-guard.sh`: passed (`winsec-guard: ok (3130 files)`).
- `make fmt-check check-custom build`: passed; all custom checks passed and
  the Linux binary built. Scoped checks continue to replace the forbidden
  unsharded hub compilation in `make ci`.

Fork CI is monitored on the pushed PR head; its final status is recorded in the
handoff evidence rather than generating a new commit just to record CI status.
