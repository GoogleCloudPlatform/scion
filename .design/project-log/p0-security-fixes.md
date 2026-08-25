# P0 Security Fixes: Dev Auth Loopback Guard (S1) & Deploy Flag Verification (S2)

**Date:** 2026-08-25
**Branch:** `scion/p0-security-fixes`
**Author:** dev-p0-security

## Summary

Implemented security fix S1 (dev auth loopback guard) and verified S2 (no
`--no-invoker-iam-check` in deploy defaults) from the Cloud Run Instances +
Sandboxes design doc (§4.11).

## S1: Dev Auth Loopback Guard

### Problem

`devAuthMiddleware` auto-logs-in any cookieless request as an admin user when
`DevAuthToken` is set. Combined with `Host = "0.0.0.0"` (the default in multiple
places) and a public `run.app` URL, this creates a publicly reachable,
unauthenticated admin UI.

### Changes

1. **`IsLoopbackHost` helper** (`pkg/hub/web.go`): New exported function that
   checks whether a host string is a loopback address (`127.0.0.0/8`, `::1`, or
   `"localhost"`). Returns false for all-interface addresses (`0.0.0.0`, `::`),
   non-loopback IPs, empty strings, and non-localhost hostnames.

2. **Primary validation in `initWebServer`** (`cmd/server_foreground.go`): After
   `webHost` is resolved, returns an error if `devAuthToken` is non-empty and the
   host is not loopback. Error message explains the conflict and how to fix it.

3. **Defense-in-depth in `NewWebServer`** (`pkg/hub/web.go`): After the host
   default is applied, calls `log.Fatalf` if dev auth is combined with a
   non-loopback host. This protects against any code path that constructs a
   `WebServer` directly without going through `initWebServer`.

4. **Test helper fix** (`pkg/hub/web_test.go`): Updated `newDevAuthWebServer` to
   explicitly set `Host: "127.0.0.1"` so existing dev-auth tests comply with the
   new guard.

### Tests

- `TestIsLoopbackHost`: Table-driven tests covering IPv4 loopback, IPv6 loopback,
  `localhost`, `0.0.0.0`, `::`, private IPs, empty string, and non-localhost
  hostnames.
- `TestNewWebServer_DevAuth_NonLoopback_Rejected`: Verifies `NewWebServer`
  succeeds with dev auth on loopback addresses and succeeds without dev auth on
  any address.
- `TestInitWebServer_DevAuth_NonLoopback_Rejected`: Table-driven tests verifying
  `initWebServer` returns an error for dev auth with `0.0.0.0`, empty host, and
  public IPs, while allowing dev auth on `127.0.0.1` and any host without dev auth.

## S2: Deploy Flag Verification

### Finding

Confirmed that `--no-invoker-iam-check` does not appear anywhere in the codebase
(grepped all `.go`, `.sh`, `.yaml`, `.yml`, `.md` files). The existing deploy script
at `scripts/cloudrun/deploy.sh` correctly uses `--no-allow-unauthenticated --iap`.

### Change

Added a security comment block before the `gcloud run deploy` command in
`scripts/cloudrun/deploy.sh` explaining why `--no-invoker-iam-check` must never be
added, referencing the design doc's S2 requirement.

## Files Changed

- `pkg/hub/web.go` — Added `IsLoopbackHost`, `"net"` import, defense-in-depth guard
- `cmd/server_foreground.go` — Added primary dev-auth loopback validation
- `pkg/hub/web_test.go` — Added `IsLoopbackHost` and `NewWebServer` guard tests,
  fixed `newDevAuthWebServer` helper
- `cmd/server_foreground_test.go` — Added `initWebServer` dev-auth rejection tests
- `scripts/cloudrun/deploy.sh` — Added preventive comment about `--no-invoker-iam-check`
