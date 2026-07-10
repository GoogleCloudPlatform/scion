# Design: Co-located Broker Heartbeat Split-Brain Fix

**Issues:** #385, #411
**Status:** Draft
**Author:** bf-arch

---

## 1. Root Cause Recap

The co-located broker (hub+broker in one process) maintains two independent state-writing paths that race with each other:

1. **Control channel lifecycle** (WebSocket): On disconnect, `onDisconnect` → `ReleaseAndMarkBrokerOffline()` correctly sets `broker.Status = "offline"`, clears affinity (`ConnectedHubID`, `ConnectedSessionID`, `ConnectedAt`), and marks all `ProjectProvider.Status = "offline"`.

2. **Co-located heartbeat goroutine** (`cmd/server_foreground.go:1985-2002`): A ticker fires every 30s and calls `UpdateRuntimeBrokerHeartbeat(ctx, brokerID, store.BrokerStatusOnline)`, which unconditionally writes `broker.Status = "online"` regardless of whether the control channel WebSocket is alive.

**The split-brain:** Within 30s of a disconnect, the heartbeat overwrites `broker.Status` back to `"online"`, but `ProjectProvider.Status` remains `"offline"` (only restored by `markBrokerOnline` on reconnect). Since `getAvailableBrokersForProject()` (`handlers_agent_create_helpers.go:726-748`) requires **both** `provider.Status == "online"` AND `broker.Status == "online"`, and providers stay offline, the broker is filtered out → `onlineProviders: 0` → HTTP 422 `no_runtime_broker`.

**Secondary effects:**
- The stale affinity reaper (`reaper.go:38-69`) never fires because `LastHeartbeat` stays fresh.
- Monitoring sees `broker.Status = "online"` — the outage is invisible.
- The reconnect logic works but its success is masked by the heartbeat already showing "online".

---

## 2. Proposed Changes

### 2.1 Primary: Make the co-located heartbeat control-channel-aware

**What:** Before each heartbeat write, check whether the control channel WebSocket is alive. If disconnected, write `status = "offline"` instead of `"online"`.

**Where:**

- **New method on `runtimebroker.Server`** — `IsControlChannelConnected() bool`
  - File: `pkg/runtimebroker/server.go`
  - Iterates `s.hubConnections` under `hubMu.RLock`, returns `true` if any connection's `ControlChannel` reports `IsConnected()`.
  - For co-located mode there is exactly one hub connection, so this degenerates to a single check.

- **Modified heartbeat goroutine** — `cmd/server_foreground.go:1985-2002`
  - The goroutine already has `rhSrv` (`*runtimebroker.Server`) in closure scope (declared at line 1962).
  - Before calling `UpdateRuntimeBrokerHeartbeat`, call `rhSrv.IsControlChannelConnected()`.
  - If connected: write `store.BrokerStatusOnline` (current behavior).
  - If disconnected: write `store.BrokerStatusOffline`.

**Why write "offline" rather than skipping the heartbeat entirely?**
- Actively corrects any stale "online" from a previous tick.
- Keeps `LastHeartbeat` fresh so the reaper doesn't fire unnecessarily (affinity is already cleared by `ReleaseAndMarkBrokerOffline`).
- Makes the broker's DB state truthfully reflect its reachability, aiding monitoring.

**Why not have the heartbeat also update `ProjectProvider` records?**
- Provider records are correctly managed by the connect/disconnect lifecycle (`markBrokerOnline` / `onDisconnect`).
- Having the heartbeat also write providers would create a second path for provider state, doubling the race surface. The heartbeat should not duplicate lifecycle semantics.

**Why not replace the co-located heartbeat with `HeartbeatService`?**
- `HeartbeatService` sends heartbeats over HTTP to the hub handler, which then calls the same `UpdateRuntimeBrokerHeartbeat` store function. The HTTP round-trip is unnecessary for a co-located broker.
- `HeartbeatService` also does not check control channel liveness (its `buildHeartbeat` unconditionally sets `status = "online"`). Switching to it would not fix the bug without the same liveness check.
- The direct-DB path was likely intentional to avoid the HTTP overhead. Adding the liveness gate is a one-line change; switching heartbeat mechanisms is a larger, riskier refactor with no additional benefit.

### 2.2 Secondary: Add health feedback from control channel to `HubConnection`

**What:** Update `HubConnection.Status` when the control channel drops and reconnects, so other components (health endpoints, monitoring, the heartbeat path in a multi-hub future) can query actual connection health.

**Where:**

- **New callback on `ControlChannelClient`** — `OnConnectionStateChange func(connected bool)`
  - File: `pkg/runtimebroker/controlchannel.go`
  - Add an optional callback field to `ControlChannelConfig` or `ControlChannelClient`.
  - Invoke it from `markDisconnected()` (line 731) with `connected=false`, and from `doConnect()` after successful handshake (line 243) with `connected=true`.

- **HubConnection registers the callback** — `pkg/runtimebroker/hub_connection.go`
  - In `Start()`, after creating `ControlChannelClient`, register a callback that calls `hc.setStatus(ConnectionStatusDisconnected)` on disconnect and `hc.setStatus(ConnectionStatusConnected)` on reconnect.
  - Add a new `ConnectionStatusReconnecting` value to distinguish "actively reconnecting" from "stopped" (both are currently `ConnectionStatusDisconnected`).

**Why:**
- `HubConnection.Status` is currently set once at startup (line 170: `setStatus(ConnectionStatusConnected)`) and never updated. It is a lie after any WebSocket drop.
- The health endpoint (`/api/v1/hub-connections`) surfaces this status. Accurate status enables monitoring to detect and alert on control channel outages.
- This is a clean, low-risk change with no impact on the primary fix.

### 2.3 Non-change: Stateless Cloud Run brokers

Stateless Cloud Run brokers (`SetStatelessEmbeddedBrokerID`) already set `ControlChannelEnabled: false` and `HeartbeatEnabled: false` in `ServerConfig` (lines 1947-1948). They route via `routeHTTP` and bypass the control channel entirely. The co-located heartbeat goroutine condition (`colocatedBrokerRegistered`) is true for both stateless and stateful co-located brokers, but:

- For Cloud Run, there is no control channel to check — `IsControlChannelConnected()` will return `false` because no `ControlChannelClient` is created.
- This means the heartbeat would always write `"offline"` for Cloud Run brokers.

**Fix:** Guard `IsControlChannelConnected()` — if no `ControlChannelClient` exists (i.e., `ControlChannelEnabled` is false), return `true` unconditionally. This means:
- **Cloud Run brokers** (no control channel): heartbeat writes "online" — no behavior change.
- **Normal co-located brokers** (with control channel): heartbeat checks control channel liveness — the fix.

Alternatively, the heartbeat goroutine itself can check `rhCfg.ControlChannelEnabled` and skip the liveness check when false.

---

## 3. Phased Implementation Plan

### Phase 1: Core fix — control-channel-aware heartbeat (Priority: Critical)

| Step | File | Change |
|------|------|--------|
| 1a | `pkg/runtimebroker/server.go` | Add `IsControlChannelConnected() bool` method. Returns `true` if any hub connection has a live control channel, OR if control channel is not enabled (Cloud Run). |
| 1b | `cmd/server_foreground.go` | Modify heartbeat goroutine (lines 1985-2002) to call `rhSrv.IsControlChannelConnected()` before each write. If disconnected, write `BrokerStatusOffline`; if connected, write `BrokerStatusOnline`. |
| 1c | Tests | Unit test for `IsControlChannelConnected()` — mock hub connection with connected/disconnected control channel. Integration test simulating the split-brain scenario: close WebSocket, verify heartbeat writes "offline", reopen, verify heartbeat writes "online". |

**Estimated diff:** ~30 lines of production code, ~80 lines of test code.

### Phase 2: HubConnection health feedback (Priority: Low)

| Step | File | Change |
|------|------|--------|
| 2a | `pkg/runtimebroker/controlchannel.go` | Add `OnConnectionStateChange func(connected bool)` field to `ControlChannelClient`. Call from `markDisconnected()` and `doConnect()`. |
| 2b | `pkg/runtimebroker/hub_connection.go` | Add `ConnectionStatusReconnecting` constant. Register state change callback in `Start()` that updates `HubConnection.Status`. |
| 2c | Tests | Test that `HubConnection.GetStatus()` transitions correctly on connect/disconnect/reconnect cycle. |

**Estimated diff:** ~25 lines of production code, ~40 lines of test code.

### Phase 3: Monitoring (Priority: Low, follow-up)

No code changes — configure alerting on `HubConnection.Status != "connected"` for co-located brokers. This is operational work that depends on Phase 2.

---

## 4. Race Condition Analysis

### 4.1 Heartbeat vs. onDisconnect (the existing race — now resolved)

**Current behavior (buggy):**
```
t0: WebSocket drops
t1: onDisconnect fires → ReleaseAndMarkBrokerOffline → status="offline"
t2: Heartbeat tick → UpdateRuntimeBrokerHeartbeat("online") → status="online"  ← BUG
```

**After fix:**
```
t0: WebSocket drops → markDisconnected() → ControlChannelClient.connected = false
t1: onDisconnect fires → ReleaseAndMarkBrokerOffline → status="offline"
t2: Heartbeat tick → IsControlChannelConnected() = false → writes "offline"  ← CONSISTENT
```

Both the heartbeat and onDisconnect now write the same value. The CAS on `lock_version` serializes them, and both agree on status.

### 4.2 Heartbeat vs. markBrokerOnline (reconnect race)

**Concern:** Heartbeat checks `connected=false` at the same instant the control channel reconnects and `connected` flips to `true`.

**Analysis:** This race window is milliseconds at most. Two sub-scenarios:

1. **Heartbeat reads `connected=false` just before reconnect:** Heartbeat writes "offline". `markBrokerOnline` fires immediately after and writes "online" + restores affinity + updates providers. Net result: correct. The heartbeat's "offline" is immediately superseded.

2. **Heartbeat reads `connected=true` just after reconnect but before `markBrokerOnline`:** Heartbeat writes "online". `markBrokerOnline` then writes "online" + affinity + providers. Net result: correct (both agree).

Neither sub-scenario creates split-brain. The split-brain only existed because the heartbeat **always** wrote "online" regardless of connection state — a 30-second window, not a millisecond one.

### 4.3 CAS ordering between heartbeat and lifecycle operations

Both `UpdateRuntimeBrokerHeartbeat` and `ReleaseAndMarkBrokerOffline`/`ClaimRuntimeBrokerConnection` use CAS on `lock_version`. If they conflict:

- `ReleaseAndMarkBrokerOffline` additionally checks `ConnectedHubID`/`ConnectedSessionID` — it only proceeds if this hub+session still owns the broker. A concurrent heartbeat bumping `lock_version` causes a CAS retry, not a correctness issue.
- `UpdateRuntimeBrokerHeartbeat` retries up to 5 times (same CAS loop). A concurrent lifecycle write causes a retry, and on retry the heartbeat re-reads the broker but still writes the status it was given. Since the heartbeat now uses the correct status (online/offline based on liveness), the retried write is still correct.

**Conclusion:** No new races are introduced. The existing CAS mechanisms handle all interleavings correctly.

### 4.4 Cloud Run / stateless broker path

Stateless Cloud Run brokers have no `ControlChannelClient`. `IsControlChannelConnected()` returns `true` when no control channel is enabled, so the heartbeat continues to write "online" unconditionally — identical to current behavior. No regression.

---

## 5. Testing Strategy

### 5.1 Unit Tests

**`pkg/runtimebroker/server_test.go`:**
- `TestIsControlChannelConnected_NoConnections` — returns `true` when no hub connections exist (degenerate case; implies no control channel needed).
- `TestIsControlChannelConnected_ControlChannelNotEnabled` — returns `true` when connections exist but none have ControlChannel set (Cloud Run path).
- `TestIsControlChannelConnected_Connected` — returns `true` when at least one connection has a live control channel.
- `TestIsControlChannelConnected_Disconnected` — returns `false` when all connections have disconnected control channels.

**`pkg/runtimebroker/hub_connection_test.go` (Phase 2):**
- `TestHubConnectionStatus_OnDisconnect` — verify `GetStatus()` transitions to `ConnectionStatusReconnecting` when control channel drops.
- `TestHubConnectionStatus_OnReconnect` — verify `GetStatus()` transitions back to `ConnectionStatusConnected`.

### 5.2 Integration Test

**`cmd/server_foreground_test.go` or a dedicated `broker_heartbeat_test.go`:**

Scenario: Simulate the split-brain failure mode.

1. Start a co-located hub+broker.
2. Verify broker registers as online, providers are online.
3. Force-close the WebSocket control channel (e.g., cancel the ControlChannelClient's context or close the underlying connection).
4. Wait for the onDisconnect callback to fire → verify `broker.Status = "offline"` and `provider.Status = "offline"`.
5. Wait for the next heartbeat tick (≤30s) → verify `broker.Status` remains `"offline"` (not overwritten to "online").
6. Re-establish the control channel (allow reconnect loop to succeed).
7. Verify `markBrokerOnline` restores `broker.Status = "online"` and `provider.Status = "online"`.
8. Verify the next heartbeat tick writes `broker.Status = "online"`.

### 5.3 Manual Verification

Reproduce the original issue scenario:
1. Run hub+broker locally.
2. Create a project and register a provider.
3. Kill the WebSocket with network manipulation (`iptables` or close the WS port).
4. Attempt agent creation → should get 422 (correct — broker is truly unreachable).
5. Verify `broker.Status = "offline"` in DB.
6. Restore network → control channel reconnects.
7. Attempt agent creation → should succeed.

### 5.4 Existing Test Coverage

`pkg/store/entadapter/broker_affinity_test.go` already tests the CAS mechanics of `ClaimRuntimeBrokerConnection` and `ReleaseAndMarkBrokerOffline`. These tests do not need modification as the fix does not change the store layer.

---

## 6. Risk Assessment

### 6.1 Low Risk

**Heartbeat writes "offline" during normal operation (false negative):**
- Could happen if `IsControlChannelConnected()` returns `false` when the control channel is actually healthy.
- Mitigation: `ControlChannelClient.connected` is set to `true` synchronously after WebSocket handshake and only set to `false` on read error. There is no scenario where the flag is false while the WebSocket is alive.

**Brief "offline" flicker during reconnect:**
- Between WebSocket drop and successful reconnect (1s-60s backoff), the heartbeat correctly writes "offline". When reconnect succeeds, `markBrokerOnline` restores online status. This is correct behavior — the broker IS offline during that window.

### 6.2 Medium Risk

**Multi-hub mode:**
- `IsControlChannelConnected()` checks all hub connections and returns `true` if any is connected. This is correct for the heartbeat's purpose: the broker is reachable if at least one hub can reach it.
- However, the co-located heartbeat writes to a single database. If one hub connection is up and another is down, the heartbeat writes "online" — which is correct for the connected hub but may not reflect reachability from the disconnected one.
- This is an existing limitation of the single `broker.Status` field and is out of scope for this fix.

### 6.3 Rollback Plan

The fix is a single `if/else` branch in the heartbeat goroutine plus a new method on `runtimebroker.Server`. To roll back:
- Revert the commit (or change the heartbeat to always write `"online"` regardless of `IsControlChannelConnected()`).
- No database schema changes, no protocol changes, no stored state changes.

---

## 7. Open Questions

### 7.1 Resolved by design

**Q: Is there a reason the co-located broker uses direct-DB heartbeat instead of `HeartbeatService`?**
A: Yes — performance optimization. The co-located broker shares a process with the hub, so direct DB writes avoid unnecessary HTTP round-trips. The `HeartbeatService` also carries per-agent status data, which is redundant for the co-located path (the hub already has direct access to agent state). The fix preserves this optimization.

**Q: Should the co-located broker be treated as "stateless local" (like Cloud Run)?**
A: No. The co-located broker on a persistent VM has state (local containers, worktrees). The stateless path bypasses affinity and control-channel routing, which would break agent lifecycle management for non-ephemeral brokers. The fix correctly gates on control channel liveness, which naturally handles both paths.

### 7.2 For user input

**Q: Should the heartbeat interval change during disconnect?**
The current 30s tick continues during disconnect. We could increase the interval (e.g., 60s) when disconnected to reduce noisy "offline" writes during reconnection windows. This is a minor optimization; the default of keeping the 30s tick is simpler and produces a more predictable `LastHeartbeat` pattern. **Recommendation: keep 30s.** Deferring to user if there's a preference.

**Q: Should we add structured logging for the state transition?**
When the heartbeat transitions from writing "online" to writing "offline" (or vice versa), we could log an INFO message like `"co-located heartbeat: control channel disconnected, writing offline status"`. This would aid debugging. **Recommendation: yes, log on transition (not every tick).** The goroutine can track the previous state to detect transitions.

---

## 8. Implementation Details

### 8.1 `IsControlChannelConnected` method

```go
// IsControlChannelConnected reports whether the broker has at least one live
// control-channel WebSocket. Returns true when no control channel is configured
// (e.g. Cloud Run stateless brokers) so callers can treat "no channel" the same
// as "channel healthy".
func (s *Server) IsControlChannelConnected() bool {
    s.hubMu.RLock()
    defer s.hubMu.RUnlock()

    if len(s.hubConnections) == 0 {
        return !s.config.ControlChannelEnabled
    }

    for _, conn := range s.hubConnections {
        conn.mu.RLock()
        cc := conn.ControlChannel
        conn.mu.RUnlock()
        if cc != nil && cc.IsConnected() {
            return true
        }
    }
    return !s.config.ControlChannelEnabled
}
```

### 8.2 Modified heartbeat goroutine

```go
if colocatedBrokerRegistered {
    wg.Add(1)
    go func() {
        defer wg.Done()
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()
        var prevOnline bool
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                ccUp := rhSrv.IsControlChannelConnected()
                status := store.BrokerStatusOnline
                if !ccUp {
                    status = store.BrokerStatusOffline
                }
                if ccUp != prevOnline {
                    if ccUp {
                        log.Printf("Co-located heartbeat: control channel restored, writing online for %s", brokerName)
                    } else {
                        log.Printf("Co-located heartbeat: control channel down, writing offline for %s", brokerName)
                    }
                    prevOnline = ccUp
                }
                if err := s.UpdateRuntimeBrokerHeartbeat(ctx, brokerID, status); err != nil {
                    log.Printf("Warning: failed to update internal heartbeat for %s: %v", brokerName, err)
                }
            }
        }
    }()
}
```

### 8.3 Connection state callback (Phase 2)

In `ControlChannelConfig`, add:
```go
OnConnectionStateChange func(connected bool)
```

In `markDisconnected()`:
```go
func (c *ControlChannelClient) markDisconnected() {
    c.mu.Lock()
    c.connected = false
    cb := c.config.OnConnectionStateChange
    c.mu.Unlock()
    // ... existing stream cleanup ...
    if cb != nil {
        cb(false)
    }
}
```

In `doConnect()`, after successful handshake:
```go
c.mu.Lock()
c.connected = true
c.connectedAt = time.Now()
cb := c.config.OnConnectionStateChange
c.mu.Unlock()
if cb != nil {
    cb(true)
}
```

In `HubConnection.Start()`, when creating the config:
```go
ccConfig := ControlChannelConfig{
    // ... existing fields ...
    OnConnectionStateChange: func(connected bool) {
        if connected {
            hc.setStatus(ConnectionStatusConnected)
        } else {
            hc.setStatus(ConnectionStatusReconnecting)
        }
    },
}
```

---

## 9. Summary

| Change | File(s) | Phase | Risk |
|--------|---------|-------|------|
| Add `IsControlChannelConnected()` | `pkg/runtimebroker/server.go` | 1 | Low |
| Gate heartbeat on control channel liveness | `cmd/server_foreground.go` | 1 | Low |
| Add connection state callback | `pkg/runtimebroker/controlchannel.go` | 2 | Low |
| Update `HubConnection.Status` on state changes | `pkg/runtimebroker/hub_connection.go` | 2 | Low |
| Add `ConnectionStatusReconnecting` | `pkg/runtimebroker/hub_connection.go` | 2 | Low |

Total estimated diff: ~55 lines of production code across Phase 1 and Phase 2. No schema changes, no protocol changes, no breaking changes.
