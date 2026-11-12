# Reincarnate --harness, Phase 2 (credential preflight)

**Date:** 2026-11-12
**Branch:** scion/reincarnate-harness-p2
**Issues:** ptone/scion#7759 (phase), ptone/scion#7757 (tracking)
**Stack:** ptone/scion PR 7770, P1 head d01b08b674c105601501e6ae4eb75e108c085022.

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

## Verification

- RED: stable `make test-hub-shard SHARD=2` failed only the new credential
  preflight test, all four env/file × automatic/explicit cases (200 instead of
  422); shard test-list validation passed. Full run: 615s, five existing skips.
- GREEN: shard-2 binary `hub.test -test.v -test.run '^TestReincarnateHarness_'`
  passed, including dry-run/real refusal parity, names without values, unchanged
  row/history/dispatcher on 422, explicit no-auth skips, and successful env/secret
  credentials. The complete `make test-hub-shard SHARD=2` also passed: 227s, four existing
  skips (optional soak, Ent-only group deletion, PostgreSQL-only topics, and
  the existing DEF-15 attachment case). Test-list validation: 5358 tests compiled
  as expected.
- `make fmt-check check-custom build`: passed (all custom checks; 121 image-script
  checks; Linux binary built).
- `GOGC=40 go vet -overlay /tmp/p2-vet-overlay/overlay.json -tags
  hubshard,hubshard_2 ./pkg/hub`: passed. The overlay was generated with
  `hack/hubshard/hubshard.sh -overlay /tmp/p2-vet-overlay`.
- `GOMAXPROCS=2 GOOS=windows GOARCH=amd64 go build -p 1 -buildvcs=false
  -o /dev/null ./cmd/scion`: passed.
- `bash hack/winsec-guard.sh`: passed (`winsec-guard: ok (3130 files)`).
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1
  --tests=false ./pkg/hub/...`: reported only P1's `reincarnate_patch.go:235:20:
  ineffectual assignment to caps (ineffassign)` (`1 issues: * ineffassign: 1`).
  The manager confirmed the P1 developer is fixing it and authorized staging this
  PR with lint pending that fix; P2 does not change the affected code.

`make ci` was replaced by scoped equivalents because its `lint` and `test-fast`
targets compile the entire hub test package, which this repository forbids.
Hub lint uses `--tests=false` per the repository's split-lint convention; shard
vet covers tests. No whole hub test package was compiled.
