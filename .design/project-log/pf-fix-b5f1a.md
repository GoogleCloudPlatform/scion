# Fix B5F1a Test Broken by #1347 Broker Authz

**Date:** 2026-08-28
**Author:** dev-pf-fix-b5f1a

## Problem

`TestBroadcast_B5F1a_SenderOverrideStoresAuthIdentity` in
`pkg/hub/handlers_agent_messaging_test.go` was broken on main after PR #1347
added an `ActionAttach` authz check to `handleProjectBroadcast`.

The test creates an "attacker" user who sends a broadcast with a spoofed
SenderID (the victim's) and verifies the stored message uses the attacker's
authenticated identity. Because the attacker had no project membership, the new
authz check returned 403 before `broadcastDirect` ran, causing the test to fail
with "no messages stored for agent."

## Fix

Added minimum project membership for the attacker so the request passes the
authz gate and reaches `broadcastDirect`, where the sender-override logic is
actually tested:

1. `ensureHubMembership(ctx, s, attacker.ID)` — hub-level membership.
2. `project.CreatedBy = attacker.ID` — required for group/policy creation.
3. `srv.createProjectMembersGroupAndPolicy(ctx, project)` — creates members
   group and default policies.
4. `s.AddGroupMember(...)` — adds attacker to the project's members group with
   the `Member` role (minimum privilege).

No test assertions were relaxed and the #1347 authz guard was not modified.

## Verification

- `go test ./pkg/hub/ -run TestBroadcast_B5F1a -v -count=1` — PASS
- Full hub suite (`go test ./pkg/hub/ -count=1`) — checked for regressions.
