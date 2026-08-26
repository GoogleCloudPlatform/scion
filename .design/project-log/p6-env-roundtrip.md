# P6: Deploy-instance env var round-trip, SEED prefix, and comma guard

**Date:** 2026-08-26
**Agent:** dev-env-roundtrip
**Branch:** scion/dev-env-roundtrip

## Summary

Three fixes to `cmd/deploy_instance.go` and `cmd/deploy_instance_test.go` addressing env var correctness in the deploy-instance command.

## Changes

### Fix 1: Round-trip test (TestDeployEnvVarsRoundTrip)

Added a test proving the env vars `diGcloudDeploy` sets actually load through the config system into the structs the hub reads at startup. The critical concern was that `Auth.Proxy` (`*ProxyAuthConfig`) and `Proxy.IAP` (`*IAPAuthConfig`) are pointer fields — if koanf/mapstructure didn't allocate them, the IAP audience would be empty and the hub would fail.

The test:
- Sets env vars exactly as `diGcloudDeploy` formats them
- Loads through `config.LoadGlobalConfig("")` (the hub's startup path)
- Asserts all six conditions: `Auth.Mode`, `Auth.Proxy != nil`, `Proxy.Provider`, `Proxy.IAP != nil`, `IAP.Audience`, and admin email via bootstrap koanf

### Fix 2: Deprecated env var key

Changed `SCION_SERVER_HUB_ADMINEMAILS` to `SCION_SEED_SERVER_HUB_ADMINEMAILS` in `diGcloudDeploy`. The SEED prefix is semantically correct: the deployer seeds the admin email as an initial value that can be overridden by `settings.yaml`. Using `SCION_SERVER_` was too strong (always wins over yaml) and triggered a deprecation warning from `DetectDeprecatedServerEnv` on every startup.

### Fix 3: Comma collision guard

Added validation in `runDeployInstance` that rejects `--admin-email` values containing commas. `gcloud --set-env-vars` is comma-delimited, so a comma in the value would silently split into a second env var, breaking the command. The guard fires early with a clear error message. Two supporting tests prove the guard works and demonstrate the breakage it prevents.

## Verification

- `go build ./...` — passes
- `make fmt-check` — passes
- `go test ./cmd/... -count=1` — all tests pass
