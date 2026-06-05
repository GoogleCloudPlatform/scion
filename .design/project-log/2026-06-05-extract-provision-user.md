# Extract provisionUser — Phase 0 auth-proxy-mode

**Date**: 2026-06-05
**Branch**: scion/auth-proxy-mode
**Commit**: refactor(hub): extract provisionUser, dedupe OAuth find-or-create

## What changed

Extracted four identical find-or-create-user blocks from OAuth handlers
into a single `provisionUser` method on `Server` in `handlers_auth.go`.

### Call sites refactored (all four)
1. `handleAuthLogin` (~line 258) — device flow login
2. `handleAuthToken` (~line 402) — OAuth code exchange (web/CLI)
3. `handleCLIAuthToken` (~line 936) — CLI-specific OAuth code exchange
4. `completeOAuthLogin` (~line 1192) — shared device flow completion

All four were semantically identical — same auth check, same find-or-create
logic, same profile backfill, same admin promotion, same hub membership
enrollment. No differences that prevented safe consolidation.

### New types
- `ExternalUserInfo` struct (Email, DisplayName, AvatarURL) — decoupled
  from `OAuthUserInfo` so the proxy middleware can reuse it
- `ErrAccessDenied` sentinel error — callers map to HTTP 403

### Tests added
8 subtests in `TestProvisionUser`: create, update, backfill, admin
promotion, domain restriction, invite-only, admin bypass, idempotency.

## Suspended-user finding

**None of the four OAuth blocks check `user.Status == "suspended"`.**
A suspended user can currently log in via any OAuth path and receive
valid tokens. The design doc says provisionUser should reject suspended
users, but adding this check would change existing OAuth behavior.

**Decision**: do NOT add the check in Phase 0. Phase 1 (proxy auth) will
add it if needed, after a separate decision on whether to also gate the
OAuth path.

## Pre-existing test failures

15 tests in `pkg/hub` fail with "invalid UUID" errors (e.g.
`TestCreateAgent_ResumeFromStoppedStatus`). These are pre-existing and
unrelated to this change — they use non-UUID string IDs that the store
now validates. All auth-related tests pass.

## Net impact
- 2 files changed, 342 insertions, 175 deletions
- ~75 fewer lines of production code (4x35 duplicated → 1x45 shared)
