# Phase 4 Security & Review Fixes

**Date:** 2026-08-29
**Branch:** scion/msg-authz
**File:** pkg/hub/handlers_agent_message_mode.go

## Summary

Applied 7 fixes from Phase 4 security audit and code review (3 MEDIUM, 2 LOW, 2 Required):

### FIX 1 (MEDIUM-1): Federated agent bypasses D7 deny gate
- **Problem:** Identity type check `== "agent"` missed `"federated_agent"`, `"federated_service"`, etc.
- **Fix:** Replaced denylist with allowlist — only `"user"`, `"dev"`, `"federated_user"` pass through.

### FIX 2 (R1 + LOW-3): Admin gate fail-open on store error / Admin-who-is-lineage-owner denied
- **Problem:** `GetProjectMembership` errors silently skipped the admin denial check (fail-open). Also, a user who is both project admin AND lineage owner was incorrectly denied.
- **Fix:** Fail closed on store error (return 500). Added ancestry check before admin denial so lineage owners who are also admins are allowed.

### FIX 3 (MEDIUM-2): RuntimeError leaks store error details
- **Problem:** Raw `err.Error()` included in HTTP response via `RuntimeError`.
- **Fix:** Log detailed error server-side with `slog.Error`, return generic message to client.

### FIX 4 (R2 / MEDIUM-3): Remove dead `reason` field
- **Problem:** `SetMessageModeRequest.Reason` accepted but never stored — dead code.
- **Fix:** Removed `Reason` from struct and `reason` parameter from `cascadeMessageMode`.

### FIX 5 (LOW-1): Add request body size limit
- **Problem:** No body size limit on the endpoint.
- **Fix:** Added `http.MaxBytesReader(w, r.Body, 64*1024)` before JSON decode.

## Verification

- All existing tests pass (`go test ./pkg/hub/... -count=1 -timeout 300s`)
- Code compiles (`go build ./...`)
- Test file updated to remove `Reason` field usage in cascade test
