# Reincarnate --harness: experiment description and CLI help runtime wording

**Date:** 2026-11-13
**Branch:** scion/reincarnate-harness-exp-desc
**Issue:** ptone/scion#7757 (follow-up after GoogleCloudPlatform/scion#4784, GoogleCloudPlatform/scion#4788, GoogleCloudPlatform/scion#4787, GoogleCloudPlatform/scion#4793)

## Context and Changes

Phase 1 (GoogleCloudPlatform/scion#4784) introduced `hub.reincarnate_harness_switch` (`experiments.ReincarnateHarnessSwitch`) and `scion reincarnate --harness` with home-reset support on `docker`, `podman` and `container`. Phases 3 and 4 (GoogleCloudPlatform/scion#4787, GoogleCloudPlatform/scion#4793) extended `runtimebroker.ReprovisionResetHomeRuntimes()` to also advertise `cloudrun`, `cloudrun-instances` and `kubernetes` (while `cloudrun-sandbox` and `substrate` remain refused), leaving the Phase 1 runtime list stale in two user-facing strings:

1. `pkg/experiments/registry.go` (`ReincarnateHarnessSwitch` `Description`):
   - Old: `Supported on the docker, podman and container runtimes.`
   - New: `Supported on the runtimes the agent's Runtime Broker advertises for home reset.`
2. `cmd/reincarnate.go` (`reincarnateCmd.Long`):
   - Old: `and is supported on the docker, podman and container runtimes.`
   - New: `and is supported on the runtimes the agent's runtime broker advertises for home reset.`

Referring to the runtimes advertised by the agent's Runtime Broker matches neighbouring entries in `pkg/experiments/registry.go` and `cmd/reincarnate.go`, accurately reflects mixed-version broker fleets as well as unsupported runtimes, and avoids going stale if future runtimes gain home-reset support.

Regression assertions were added to `TestReincarnateHarnessSwitchExperimentRegistered` (`pkg/experiments/registry_test.go`) and `TestReincarnateHelp_RecommendsDryRunForHarness` (`cmd/reincarnate_harness_test.go`), verified to fail against the stale strings first and pass after the update.

## Codebase Sweep

Searched Go strings, CLI help, web sources, embedded configs/skills and golden files for stale local-runtime lists tied to `ptone/scion#7757`:

- No web UI text, embedded asset or golden file carried the stale list (`docs-site` prose is covered separately in `ptone/scion#7790`).
- One internal code comment in `pkg/runtimebroker/handlers.go` (`handleInfo`, line 448: `// Home reset on the local-disk runtimes (ptone/scion#7757).`) still reflects Phase 1 wording; left untouched to keep this PR minimal to user-facing strings and their tests.

## Verification

- `go test -v -buildvcs=false ./pkg/experiments/...` and `go test -v -buildvcs=false ./cmd -run '^TestReincarnate'`: failed in RED before the fix, passed in GREEN after.
- `env -u SCION_... go test -count=1 -buildvcs=false ./pkg/experiments/... ./cmd`: passed (`pkg/experiments` 0.003s, `cmd` 150.584s).
- `go vet -buildvcs=false ./pkg/experiments/... ./cmd/...`: passed.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/experiments/... ./cmd/...`: passed, exact output `0 issues.`.
- `GOOS=windows GOARCH=amd64 go build -buildvcs=false -o /dev/null ./cmd/scion`: passed.
- `GOOS=windows GOARCH=amd64 go vet ./pkg/experiments/... ./cmd ./cmd/scion`, `GOOS=windows GOARCH=amd64 go test -c -o /dev/null ./pkg/experiments` and `GOOS=windows GOARCH=amd64 go test -c -o /dev/null ./cmd`: passed.
- `bash hack/winsec-guard.sh`: passed (`winsec-guard: ok (3132 files)`).
- `make fmt-check`: passed (`Go formatting OK.`).
- `make check-custom`: passed (`All custom checks passed.`).
- `make build`: passed (`Binary: ./build/scion`).
- Full `make ci` was not run directly because its `lint` and `test-fast` recipes compile the unsharded `pkg/hub` test package, which repository rules forbid; the individual checks above cover formatting, custom lint guards, Linux/Windows builds, vet, `golangci-lint` and full tests of the touched packages.
