# P6: Member type detection + smoke check diagnostics

**Date:** 2026-08-26
**Branch:** `scion/dev-p6-fixes` (based on `scion/dev-rebase-1294`)
**Files:** `cmd/deploy_instance.go`, `cmd/deploy_instance_test.go`

## Changes

### Fix 1: IAM member type detection in diBindIAPPolicy

`diBindIAPPolicy()` hardcoded `"user:"` as the IAM member prefix. The
`--admin-email` flag is documented for CI service account deploys, and
service account emails (ending in `.gserviceaccount.com`) require the
`"serviceAccount:"` prefix. Using `"user:sa@…gserviceaccount.com"` is
not a valid IAM member and the binding would fail or bind incorrectly.

Added `diIAMMemberPrefix(email string) string` helper that returns the
correct prefix based on the email suffix. Updated `diBindIAPPolicy` to
use it.

### Fix 2: Post-deploy smoke check diagnostics

The existing gate 1 (`diWaitForIAP`) and gate 2 (`diAssertPerimeter`)
already serve as the post-deploy smoke check: if the Instance is dead
(wrong port, crash loop, missing binary), Cloud Run returns 502/503
instead of the IAP 302, so both gates catch it. The problem was that
the error messages were misleading.

Changes:
- `diWaitForIAP`: tracks last-seen HTTP status and includes it in the
  timeout error message, distinguishing "IAP is slow" from "Instance is
  dead" (502/503).
- `diAssertPerimeter`: returns specific error messages for 502/503
  mentioning possible causes (Dockerfile CMD, port, container health).
- Gate 2 success now prints "Instance is serving and IAP-protected."
- Added doc comment on `diAssertPerimeter` explaining the smoke check
  dual purpose.

## Tests added

- `TestIAMMemberPrefix_UserEmail` — normal email returns `user:` prefix
- `TestIAMMemberPrefix_ServiceAccount` — `.gserviceaccount.com` returns `serviceAccount:` prefix
- `TestAssertPerimeter_CloudRunErrorPage` — 502 and 503 responses produce
  error messages mentioning Instance health and CMD

## Verification

- `go build ./...` passes
- `go test ./cmd/... -count=1` passes (all existing + new tests)
- `make fmt-check` passes
