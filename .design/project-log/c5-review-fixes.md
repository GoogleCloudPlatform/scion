# C5 Hub Handler Wiring — Review Fixes (PR #1391)

**Date:** 2026-08-29  
**Branch:** `scion/ca-msg-c5-handlers`  
**PR:** #1391  

## Summary

Applied all 8 review items from the code review on the C5 hub handler wiring PR.

## Changes

### R-1 (MUST FIX): handlers_messages.go — slug where UUID required
- Changed `agentID` (raw handler param, may be a slug) to `agent.ID` (resolved UUID) in the `ResolveDMConversationForRead` call at the read-switch path in `handleAgentMessages`. The `agent` variable is already resolved via `GetAgent` earlier in the function.

### R-2 (MUST FIX): webChatStore lock — 5 sites
- Added `s.mu.RLock(); wcs := s.webChatStore; s.mu.RUnlock()` pattern at all 5 sites:
  1. `handlers_agent_messaging.go` — outbound message conversation resolution
  2. `handlers_agent_messaging.go` — Phase 11 conversation resolution
  3. `handlers_agent_messaging.go` — handleAgentMessage conversation resolution
  4. `handlers_broker_inbound.go` — Phase 11 broker edge conversation resolution
  5. `handlers_broker_inbound.go` — broker inbound thread conversation resolution

### R-3 (MUST FIX): hack/check-authz-reachability.sh — `grep -E`
- Changed `grep -q` with `\|` alternation (GNU-only) to `grep -E -q` with `|` (POSIX ERE).
- Added self-test: after the fail-closed scan loop, verify that `dispatch_hits > 0`. If zero files matched the dispatch pattern, the recogniser is broken and the check reports a failure.

### R-4 (fix): handlers_agent_messaging.go — nil check on conv
- Added `&& conv != nil` guard to the `GetConversation` result check (DEF-11 CLI pre-resolved ConversationID path).

### R-5 (fix): handlers_broker_inbound.go — nil check on u
- Added `&& u != nil` guard to the `GetUserByEmail` result check when resolving sender user ID.

### R-6 (fix): messaging/divergence.go — guard result before .Items
- Added nil guards on all three `ListMessages` results before accessing `.Items`:
  - Thread query result
  - DM forward-direction query result (result1)
  - DM reverse-direction query result (result2)

### R-7 (fix): handlers_agent_messaging.go — duplicate GetAgent
- Hoisted the `GetAgent(ctx, id)` call before the mentions block.
- Removed the duplicate call that was at line 682.
- Used `agent` directly in the mentions block instead of `primaryAgent`.
- Added `&& mentionAgent != nil` guard on `GetAgentBySlug` result.

### R-8 (fix): handlers_broker_inbound.go — use `log` not `s.messageLog`
- Changed `LogDivergence(s.messageLog, ...)` to `LogDivergence(log, ...)`.
- Changed `CheckConversationConsistency(..., s.messageLog)` to `CheckConversationConsistency(..., log)`.
- The `log` variable carries broker_id and plugin_name context from the request.

## Verification

- `gofmt` clean on all modified files
- `go build ./pkg/hub/... ./pkg/messaging/...` succeeds
- `hack/check-authz-reachability.sh` passes (including new self-test)
