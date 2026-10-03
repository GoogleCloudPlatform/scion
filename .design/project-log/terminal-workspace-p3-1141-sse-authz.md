# Project Log: #1141 F2 SSE Authorization — Wildcard Contract Fix

**Date:** 2026-09-21
**Issue:** ptone/scion#1141
**Agent:** dev-p3-1141-sse

## Problem

The web client sends NATS-style wildcard subjects like `project.>` (meaning
"all my projects") when subscribing to SSE events in dashboard scope. The server's
`authorizeSSESubjects()` treated the `>` token as a literal project ID, causing
all project wildcard subscriptions to be denied with 403.

Affected patterns:
- `project.>` (dashboard scope) — treated `>` as project ID → lookup fails → denied
- `notification.>` and `broker.>` — these passed through the authorization check
  because they don't match the `project`/`user`/`agent` cases in the switch statement

The specific project scope (`project.<uuid>.>`) worked correctly because `tokens[1]`
is the actual project UUID.

## Solution: Server-Side Wildcard Expansion (Option A)

Added `expandSSEWildcards()` in `handleSSE()` that runs **before** both authorization
and subscription. This expands NATS-style wildcards in resource-ID positions into
concrete, authorized subjects:

1. **`project.>` → `project.<uuid1>.>`, `project.<uuid2>.>`**: Lists all projects,
   checks ActionRead via ComputeCapabilitiesBatch, and replaces the wildcard with
   one subject per accessible project. Fail-closed: if expansion returns no projects,
   the subject is silently dropped and the connection is denied.

2. **`user.>` → `user.<callerID>.>`**: Expands to the caller's own user ID (users
   can only subscribe to their own events).

3. **`notification.>` / `broker.>`**: Pass through unchanged — these categories have
   no per-resource authorization checks.

4. **Non-wildcard subjects**: Pass through unchanged to existing authorization logic.

### Belt-and-Suspenders

Added wildcard rejection in `authorizeSSESubjects()` for resource-checked categories
(`project`, `user`, `agent`). If a wildcard in resource-ID position slips past
expansion, it is explicitly denied rather than treated as a literal ID.

### Client Impact

**No client changes needed.** The client continues to send `project.>` for dashboard
scope, and the server expands it transparently. The existing specific-project subjects
(`project.<uuid>.>`) continue to work unchanged.

## Security Properties

- **Fail-closed**: Empty expansion → denied. No store → denied. No authz → denied.
- **No role broadening**: Members see only their accessible projects. Admin sees all
  (via existing super-admin short-circuit in ComputeCapabilitiesBatch).
- **No over-subscription**: The NATS subscription uses the expanded specific subjects,
  not the original wildcard. Users only receive events for projects they can read.

## Files Changed

- `pkg/hub/web.go` — Added `expandSSEWildcards()`, `expandProjectWildcard()`,
  `isNATSWildcard()` helper; updated `handleSSE()` to call expansion before authz;
  updated `authorizeSSESubjects()` with belt-and-suspenders wildcard rejection.
- `pkg/hub/sse_authz_test.go` — Added 9 new test functions covering wildcard
  expansion scenarios; extended `mockAuthzStore` with `projects` and
  `projectMemberships` fields.

## Test Cases Added

- `project.>` with 1 accessible project → authorized
- `project.>` with 0 projects → all denied (fail-closed)
- `project.>` with mixed ownership → only accessible projects expand
- `project.<uuid>.>` (specific) → unchanged behavior preserved
- `notification.>` / `broker.>` → passthrough unchanged
- Mixed wildcard + specific subjects → correct per-subject handling
- Belt-and-suspenders: unresolved wildcards in authorizeSSESubjects → denied
- `user.>` → expands to caller's own user ID
- No session user → subjects pass through (authz denies)
