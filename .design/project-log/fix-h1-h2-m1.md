# Fix: H1 + H2 + M1 Security Findings

**Date:** 2026-07-30
**Branch:** `scion/ca-mgr-scion-thread`

## Summary

Three findings from code review and security audit, all addressed in a single commit.

### H1 — Include X-Scion-On-Behalf-Of in HMAC signed headers

**File:** `extras/scion-discord/internal/discord/hubclient.go`

The Discord plugin's `CreateAgent` method was setting the `X-Scion-On-Behalf-Of`
header but never listing it in `X-Scion-Signed-Headers`. This meant the
on-behalf-of header was not covered by the HMAC signature, allowing a
TLS-terminating proxy to inject or modify it without invalidating the signature.

**Fix:** Set `X-Scion-Signed-Headers: x-scion-on-behalf-of` alongside the
on-behalf-of header, before `signRequest()` is called. The HMAC signing library
(`BuildCanonicalString`) reads this header to include the listed headers in the
canonical string.

### H2 — Record on-behalf-of identity in audit event

**File:** `pkg/hub/audit.go`

The `AuditableBrokerAuthMiddleware` was logging the auth-success event before
resolving the on-behalf-of identity. The `BrokerAuthEvent.Details` map was never
populated with who was being impersonated, creating an audit gap.

**Fix:** Moved the success-event logging to after on-behalf-of resolution. When a
delegated identity is present, the event details now include
`on_behalf_of_email` and `on_behalf_of_user_id`.

### M1 — Extract shared on-behalf-of context helper

**File:** `pkg/hub/brokerauth.go`

The on-behalf-of resolution and context-setting logic was copy-pasted between
`BrokerAuthMiddleware` and `AuditableBrokerAuthMiddleware`. If one was updated
without the other, security behavior would diverge.

**Fix:** Extracted `applyOnBehalfOf(ctx, w, r, brokerIdent)` on
`BrokerAuthService`. The helper resolves the delegated identity, writes the HTTP
error on failure, and returns the updated context plus the resolved
`UserIdentity` (nil when absent). Both middlewares now call this single helper.
The auditable variant uses the returned `UserIdentity` to populate audit event
details before logging.
