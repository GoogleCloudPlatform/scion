# Phase 1: Hub X-Scion-On-Behalf-Of Middleware

**Date:** 2026-07-30
**Author:** ca-dev-thread-p1
**Status:** Complete

## Summary

Added delegated requestor identity support to the hub's broker authentication
layer. When a broker (e.g., the Discord plugin) makes API calls on behalf of a
user, it sends an `X-Scion-On-Behalf-Of` header with the user's identity. The
hub middleware resolves this to a real user and sets the user identity in the
request context alongside the existing broker identity.

## What Changed

### `pkg/hub/brokerauth.go`

- **New constant:** `HeaderOnBehalfOf = "X-Scion-On-Behalf-Of"` — the header
  carrying the delegated identity assertion.

- **New interface:** `OnBehalfOfResolver` — minimal interface with
  `GetUserByEmail(ctx, email)` to decouple user lookup from the full store.
  `store.Store` implicitly satisfies this interface.

- **Updated `BrokerAuthService`:** added `onBehalfOfResolver` field, wired from
  the existing store in `NewBrokerAuthService`.

- **New method:** `resolveOnBehalfOf(ctx, r)` — parses the `scheme:identifier`
  header format, currently supports only `scheme == "user"`, looks up the user,
  checks for suspension, and constructs an `AuthenticatedUser` with
  `"integration"` client type (matching the precedent from
  `handlers_broker_inbound.go:134`).

- **Updated `BrokerAuthMiddleware`:** after HMAC signature validation, calls
  `resolveOnBehalfOf`. When a valid user is found, sets both the broker identity
  (at `brokerIdentityContextKey`) and the user identity (at both
  `userContextKey` and `identityContextKey`). When the header is absent, behavior
  is unchanged — the broker identity is the sole identity.

### `pkg/hub/audit.go`

- **Updated `AuditableBrokerAuthMiddleware`:** same on-behalf-of resolution logic
  so auditable and non-auditable middleware paths are consistent.

### `pkg/hub/brokerauth_test.go`

Seven new test cases:
1. Broker request without the header — unchanged behavior (no user identity)
2. Broker request with `user:alice@example.com` — both broker and user identity
   in context, correct email/ID/role/clientType
3. Broker request with unknown user — 403 "on-behalf-of principal not found"
4. Broker request with suspended user — 403 "on-behalf-of principal is suspended"
5. Broker request with unsupported scheme (e.g., `discord:12345`) — 400
   "unsupported on-behalf-of scheme"
6. Non-broker request with the header — header ignored, no user identity
7. Invalid header format (no colon, empty scheme, empty identifier) — 400

## Design Decisions

- **Wire format:** `scheme:identifier` (Decision Q7a from the design doc).
  Namespacing from day one makes the future B3 upgrade (hub-owned identity
  mapping) a hub-only change — the plugin already sends `<scheme>:<id>` and only
  the resolver behind it changes.

- **Error codes:** 400 for malformed header or unsupported scheme (client error),
  403 for unresolvable or suspended principal (authorization denial).

- **Context layering:** Broker identity lives at `brokerIdentityContextKey`;
  user identity overrides the generic `identityContextKey`. This ensures
  `GetBrokerIdentityFromContext` and `GetUserIdentityFromContext` both return
  values simultaneously — handlers can see both who the broker is and which user
  it's acting for.

## Backward Compatibility

When the `X-Scion-On-Behalf-Of` header is absent, the middleware behaves exactly
as before. No plugin in the field sends this header yet, so this change is
purely additive and safe to ship ahead of the plugin update.

## Related

- Design doc: `arch-scion-thread-cmd.md` (Decision Q7, Q7a)
- Fork issues: #591 (authorization gaps), #598 (route manifest)
- Next: Phase 2–8 in the Discord plugin
