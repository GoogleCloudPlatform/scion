# CI fix: live route inventory vs. broker settings (GoogleCloudPlatform/scion#2126, #2133)

Branch `scion/broker-settings-fix-catalog-live`, cut from `upstream-main` (`GoogleCloudPlatform/scion`
main at fetch time `e6b9ba29`). PR `ptone/scion#2334` against `ptone/scion` `main`. Part of
`ptone/scion#2061`.

## Problem

Two PRs merged upstream around the same time, and neither one's CI saw the other:

- `GoogleCloudPlatform/scion#2126` (P2.1, merged as `86fc807b`) added
  `GET/PUT /api/v1/runtime-brokers/{id}/settings`.
- `GoogleCloudPlatform/scion#2133` (`7452c53b`) added
  `TestCatalogHTTPEntryPoints_LiveMethodCheck`, a live route-inventory test that dispatches every
  `authzop.Catalog` HTTP entry point through the real server mux with fixture IDs substituted for
  path parameters, and asserts the response is never 404/405.

Result: upstream main fails in `pkg/hub`:

```
authzop_catalog_method_inventory_test.go:799: GET /api/v1/runtime-brokers/live-inventory-placeholder/settings (operation broker.read): got 404 Not Found — the catalog's declared path does not reach a live route for this operation
```

and the same for `PUT ... (operation quota.update)`. Cause: the test's generic fallback substitutes
a non-existent placeholder ID (`live-inventory-placeholder`) for any pattern not listed in
`patternOverrides`/`opPatternOverrides`, and the new settings route wasn't listed — so the request
hits `handleBrokerSettings`, which correctly 404s a lookup on a broker that doesn't exist. The
handler's 404 behavior is correct; only the test's fixture wiring is missing.

## Fix (test-only, no handler behavior change)

Reproduced first on upstream main to confirm the exact failure (`go test ./pkg/hub/ -run
TestCatalogHTTPEntryPoints_LiveMethodCheck -count=1`), then added one line to
`patternOverrides` in `pkg/hub/authzop_catalog_method_inventory_test.go`:

```go
// --- runtime broker family ---
"/api/v1/runtime-brokers/{id}":          {"id": f.runtimeBroker},
"/api/v1/runtime-brokers/{id}/settings": {"id": f.runtimeBroker},
```

pointing the settings pattern at the same `f.runtimeBroker` fixture the plain
`/api/v1/runtime-brokers/{id}` pattern already uses.

**No skip or exclusion was added anywhere**, per an explicit correction from the EM mid-task: the
brief's original fallback ("if the PUT then fails body validation, add a bodyOverrides entry... or
if a suffix/control check needs an exclusion, add one") turned out to be unnecessary. With the real
broker ID in place:

- **GET** returns 200 (`broker.read` against the real broker).
- **PUT** returns 200 with the test's default body (an empty `{}` object, no override needed):
  `brokerSettingsPutRequest` decodes to `Settings: nil, ExpectedRevision: 0`, which matches the
  fixture broker's current (no-row-yet) revision 0, and `changedBrokerSettingsKeys` finds nothing
  changed (both old and new `MaxAgents` are nil) — `handlePutBrokerSettings`'s no-op short-circuit
  returns 200 without ever reaching the `quota.update` permission check.
- The **control check** (bogus method `PROPFIND`) hits `handleBrokerSettings`'s method switch and
  gets 405, because `handleRuntimeBrokerByIDInternal`'s routing matches `subPath == "settings"`
  before looking at the method at all.
- The **suffix check** (`.../settings/live-inventory-bogus-suffix`) fails the exact-match
  `subPath == "settings"` check, falls through to `handleRuntimeBrokerByIDInternal` with an
  unrecognized subpath, and 404s.

All three checks pass for both the GET and PUT settings entries against the real route, with zero
exclusions — verified with a `-v` run showing the four requests
(`PROPFIND`/suffix/positive for each of GET and PUT) and their expected 405/404/200 codes.

## Gates run (all `SCION_*`/`CLAUDE_*` env vars unset)

- `go test ./pkg/hub/ -run TestCatalogHTTPEntryPoints_LiveMethodCheck -count=1` — was failing on
  bare upstream main with the two 404s above; passes after the fix.
- `go test ./pkg/hub/ -run 'TestCatalogHTTPEntryPoints|Authzop|Catalog' -count=1` — passes.
- `go vet ./pkg/hub/` — clean.
- `golangci-lint run --new-from-rev=upstream-main ./pkg/hub/...` — 0 issues.
- `make test-hub-sqlite` (full `pkg/hub` suite): a first run failed in `pkg/hub`, but the failing
  test's name was lost to truncation in the captured log (the capture kept only the last ~100
  lines of a very verbose run). Re-ran `go test -count=1 -timeout 25m -v -skip '...'  ./pkg/hub/`
  with output redirected to a file to identify it: the sole failure is
  `TestHandleAgentMessage_LogCapture_RawContentRedacted`, confirmed by the EM as a known
  order-dependent flake. It passes cleanly in isolation
  (`go test -run TestHandleAgentMessage_LogCapture_RawContentRedacted -count=1`, both subtests
  green). PR was opened before this confirmation finished, on the EM's explicit instruction not to
  hold an XS-priority CI fix for it.
- `go test -p 2 -count=1 ./pkg/hub/...` (requested by the EM as an additional gate): same single
  failure, `TestHandleAgentMessage_LogCapture_RawContentRedacted`; every other `pkg/hub/*`
  subpackage `ok`.
- **Bare-`upstream-main` baseline**, run in a separate `git worktree` at `upstream-main` (`e6b9ba29`,
  pre-fix) with the identical command: two failures —
  `TestCatalogHTTPEntryPoints_LiveMethodCheck` (the bug this PR fixes; absent on this branch) and
  `TestHandleAgentMessage_LogCapture_RawContentRedacted` (present here too, confirming the flake
  predates this fix and is not introduced or exposed by it). Everything else `ok`.

## Deliverables

- PR `ptone/scion#2334` (`scion/broker-settings-fix-catalog-live` -> `main`, ptone/scion), not
  draft. Head SHA `70d651eff00230124aab5c91510b08e616004e56`.
- This log entry.
- `scion message` to `broker-settings-em` with the PR number, head SHA, and test results above.
