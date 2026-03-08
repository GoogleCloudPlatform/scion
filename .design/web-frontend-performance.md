# Web Frontend Responsiveness Improvements

**Created:** 2026-03-08

## Problem Statement

The web frontend feels "single-threaded" during user interactions. When a user clicks a button — for example, to delete an agent — no other clicks in the UI register until that operation completes. The root cause is that every mutating action (start, stop, delete) awaits a synchronous full-data reload before re-enabling the UI. This blocks interaction for the entire round-trip duration (action request + reload request + render).

This document analyzes the current interaction flow, identifies the specific bottlenecks, and proposes a layered improvement strategy that balances responsiveness with data consistency.

---

## Current Architecture

### What Works Well

The system already has solid real-time infrastructure:

- **SSE + StateManager** (`web/src/client/state.ts`): A singleton `StateManager` maintains a full in-memory `Map` of agents, groves, and brokers. It subscribes to SSE events scoped to the current view and merges incoming deltas into the map, notifying listeners via `EventTarget`.
- **View-scoped subscriptions**: The SSE scope follows navigation (one grove-level subscription, not per-entity), keeping connection count low.
- **Server concurrency**: Go's `net/http` handles each request in its own goroutine. The `ChannelEventPublisher` uses non-blocking channel sends with a 64-element buffer per subscriber. No global locks during request handling.
- **Fire-and-forget publishing**: Event publish calls never block the HTTP response. The server returns (e.g., 204 for delete) immediately after the store operation succeeds.

### Where the Bottleneck Lives

The primary bottleneck is client-side — every component follows the same blocking pattern. However, observed delays of 5–10 seconds suggest that server-side contention (e.g., database locking under concurrent agent activity) may also contribute. See [Server-Side Response Time Concerns](#server-side-response-time-concerns) for further analysis. The client-side pattern:

```
User clicks action button
  -> Button enters loading/disabled state
  -> await apiFetch(action)         // network round-trip 1
  -> await this.loadData()          // network round-trip 2 (full reload)
    -> this.loading = true          // page shows loading spinner
    -> fetch all entities from API
    -> this.loading = false
  -> Button exits loading state
```

During this entire sequence (two serial network requests), the component is either showing a loading spinner or has buttons disabled. No other interactions can proceed.

### Affected Components

| Component | File | Pattern | Loading Scope |
|-----------|------|---------|---------------|
| **Agents list** | `web/src/components/pages/agents.ts:296` | `await this.loadAgents()` after every action | Per-agent buttons disabled, but full page reload sets `this.loading = true` showing a spinner for the entire list |
| **Agent detail** | `web/src/components/pages/agent-detail.ts:714,754` | `this.actionLoading = true` (single boolean) + `await this.loadData()` | All action buttons disabled during any single operation |
| **Grove detail** | `web/src/components/pages/grove-detail.ts:817,857` | `await this.loadData()` after agent actions and stop-all | Full page reload |
| **Grove detail (files)** | `web/src/components/pages/grove-detail.ts:713,747` | `await this.loadWorkspaceFiles()` after file operations | File list reload |

### Why Full Reloads Exist

The full reload pattern was likely introduced because SSE delta merging alone led to incomplete renders. This is a real concern:

1. **SSE deltas are partial**: A status event contains `{ agentId, status, sessionStatus, timestamp }` — not the full agent object with capabilities, configuration, grove name, broker name, etc.
2. **Race conditions**: The SSE event may arrive before the action response, or the action may complete before the SSE event is published. Components need complete objects to render correctly.
3. **Capabilities**: The `_capabilities` field (which controls which buttons are shown) comes from the REST API response, not from SSE events. Without a reload, capability-gated UI elements may not update.
4. **New entities**: When an agent is created, SSE publishes an `AgentCreatedEvent` with minimal fields (`agentId`, `name`, `template`, `groveId`, `status`). The component needs the full agent object to render a card with all metadata.

These are legitimate reasons for fetching full state. The issue is not that reloads happen — it's that they **block the UI while they happen**.

---

## Proposed Improvements

### Strategy: Decouple Action Feedback from Data Refresh

The core principle is: **acknowledge the user's action immediately, then converge to full state in the background**. This is achieved through three layers, each independently valuable:

### Layer 1: Optimistic Local State Updates (Highest Impact)

**Goal**: The UI responds instantly to user actions without waiting for any network response.

**Approach**: For start/stop, apply the expected transitional state (`starting`/`stopping`) to the local component state before awaiting the API response. For delete, keep a spinner on the action button until the server confirms — optimistically removing an agent from the list creates a worse experience if it reappears after a server error.

#### Delete

Delete does **not** use optimistic removal. Instead, the delete button shows a per-button spinner while the request is in flight. Once the server confirms (2xx response), the agent is removed from the local list and a background refresh runs for full consistency. On failure, the spinner clears and an error is shown.

```typescript
private async handleAgentAction(agentId: string, action: 'delete'): Promise<void> {
  // 1. Show spinner on the delete button for this agent
  this.actionLoading[agentId] = true;
  this.requestUpdate();

  try {
    const response = await apiFetch(`/api/v1/agents/${agentId}`, { method: 'DELETE' });
    if (!response.ok) throw new Error(/* ... */);

    // 2. Server confirmed — remove from local list
    this.agents = this.agents.filter(a => a.id !== agentId);

    // 3. Background reload for full consistency (see Layer 2)
    this.backgroundRefresh();
  } catch (err) {
    this.showError(err);
  } finally {
    delete this.actionLoading[agentId];
    this.requestUpdate();
  }
}
```

#### Start / Stop

```typescript
// Optimistic status update
const agentIndex = this.agents.findIndex(a => a.id === agentId);
if (agentIndex >= 0) {
  const updated = { ...this.agents[agentIndex] };
  updated.status = action === 'start' ? 'starting' : 'stopping';
  updated.phase = action === 'start' ? 'starting' : 'stopping';
  this.agents = [...this.agents];
  this.agents[agentIndex] = updated;
}
```

**Tradeoffs**:
- (+) Start/stop show instant visual feedback — the agent shows "starting"/"stopping" immediately.
- (+) Delete shows a clear spinner on the button, confirming the action is in progress without misleading the user about completion.
- (+) Other agents remain fully interactive since no global loading state is set.
- (-) Optimistic updates for start/stop require knowing the correct intermediate status values (`starting`, `stopping`). Incorrect guesses could flash a wrong status badge.

**Risk mitigation**: Delete uses server-confirmed removal, avoiding any flash of incorrect state. Optimistic start/stop carry minimal risk since `starting`/`stopping` are real transitional states that the server will converge to within seconds.

#### Agent Detail Page: Per-Action Loading

Additionally, on the agent detail page, replace the single `actionLoading` boolean with per-action tracking so that clicking "stop" doesn't disable the "terminal" button:

```typescript
// Before
private actionLoading = false;

// After
private actionLoading: Record<string, boolean> = {};
// Usage: this.actionLoading['stop'] = true;
// Render: ?disabled=${this.actionLoading['stop']}
```

### Layer 2: Background Refresh with Silent Reload (Medium Impact)

**Goal**: After an optimistic update, converge to full server state without blocking the UI.

**Approach**: Replace the synchronous `await this.loadAgents()` with a non-blocking background refresh that does not set `this.loading = true`.

```typescript
/**
 * Refresh data from the server without showing a loading state.
 * Used after optimistic updates to converge local state with server truth.
 */
private backgroundRefresh(): void {
  // Fire-and-forget — do not await
  this.fetchAndMerge().catch(err => {
    console.warn('Background refresh failed:', err);
  });
}

private async fetchAndMerge(): Promise<void> {
  const response = await apiFetch('/api/v1/agents');
  if (!response.ok) return; // Silent failure — SSE will eventually converge

  const data = await response.json();
  const freshAgents = Array.isArray(data) ? data : data.agents || [];

  // Merge rather than replace: preserve any optimistic state
  // that the server hasn't confirmed yet
  this.agents = freshAgents;
  this.scopeCapabilities = data._capabilities;
  stateManager.seedAgents(freshAgents);
}
```

**Key differences from current `loadAgents()`**:
- Does **not** set `this.loading = true` — no spinner, no skeleton, no disabled buttons.
- Is not awaited by the action handler — the user can keep interacting.
- Failures are silent — the SSE stream provides a secondary convergence path.

**Tradeoffs**:
- (+) UI remains interactive throughout the refresh.
- (+) Still fetches full state including capabilities, so the UI converges correctly.
- (-) There's a brief window where the UI shows optimistic state that may differ from server state. For most actions this window is <500ms.
- (-) If the background refresh races with another user action, the merge could overwrite optimistic state from the second action. This is acceptable because the SSE stream will deliver the correct final state.

#### Alternative Considered: No Background Refresh (SSE-Only Convergence)

We could skip the background refresh entirely and rely solely on SSE deltas. The `StateManager` already handles `agent.deleted`, `agent.status`, and `agent.created` events.

**Why this is insufficient alone**:
- SSE deltas don't carry capabilities (`_capabilities`), so capability-gated buttons (start, stop, delete, terminal) may not update correctly after a status change.
- SSE deltas for status changes carry a limited subset of agent fields. If the action changes multiple fields (e.g., stop clears `startedAt`, resets counters), the delta may not include all of them.
- The 64-element channel buffer could drop events under load, leaving the UI permanently stale until the next navigation.
- The concern about incomplete rendering that led to the current reload pattern would remain unaddressed.

**Verdict**: Background refresh is the right balance — it provides full-state convergence without blocking the UI. SSE deltas provide near-instant visual updates; the background refresh provides completeness.

### Layer 3: SSE-Driven Rendering (Refinement)

**Goal**: Let SSE deltas update the UI in real-time even before the background refresh completes.

The components already listen for SSE events via `stateManager.addEventListener('agents-updated', ...)`. The `onAgentsUpdated()` handler in `agents.ts` (line 203) already merges SSE deltas into the local agent list and handles deletions.

The improvement here is to ensure that SSE-driven updates and optimistic updates don't conflict:

```typescript
private onAgentsUpdated(): void {
  const updatedAgents = stateManager.getAgents();
  const deletedIds = stateManager.getDeletedAgentIds();

  // Start from current local state (which may include optimistic updates)
  const agentMap = new Map(this.agents.map(a => [a.id, a]));

  // Apply SSE deltas — these override optimistic guesses with real server state
  for (const agent of updatedAgents) {
    const existing = agentMap.get(agent.id);
    agentMap.set(agent.id, { ...existing, ...agent } as Agent);
  }

  // Remove confirmed deletions
  for (const id of deletedIds) {
    agentMap.delete(id);
  }

  this.agents = Array.from(agentMap.values());
}
```

This is close to the current implementation. The main change is the mental model: SSE updates are the **primary** real-time path for status changes, and the background refresh is a consistency backstop.

**Tradeoffs**:
- (+) Status changes (running -> stopping -> stopped) appear within seconds via SSE, without waiting for the background refresh.
- (-) SSE deltas don't include capabilities, so button visibility may not update until the background refresh completes. This is a brief cosmetic gap, not a functional issue.

---

## Detailed Change Plan by Component

### `agents.ts` (Agents List Page)

| Current | Proposed |
|---------|----------|
| `handleAgentAction` awaits `loadAgents()` | Per-button spinner for delete (server-confirmed removal); optimistic status for start/stop; fire-and-forget `backgroundRefresh()` |
| `handleStopAll` awaits `loadAgents()` | Mark all running agents as "stopping" optimistically, then background refresh |
| `loadAgents()` sets `this.loading = true` | Only set `this.loading = true` on initial page load, not on refreshes |
| Per-agent `actionLoading` disables buttons for the acting agent | Keep per-agent loading, but clear it after the API response (not after reload) |

### `agent-detail.ts` (Agent Detail Page)

| Current | Proposed |
|---------|----------|
| Single `actionLoading = false` boolean | Per-action `actionLoading: Record<string, boolean>` |
| `handleAction` awaits `loadData()` after start/stop | Optimistic status update + background refresh |
| `handleAction` redirects on delete | Keep redirect — no change needed (navigating away) |
| `loadData()` sets `this.loading = true` | Only on initial load |

### `grove-detail.ts` (Grove Detail Page)

| Current | Proposed |
|---------|----------|
| `handleAgentAction` awaits `loadData()` | Optimistic update of agent in local list + background refresh |
| `handleStopAll` awaits `loadData()` | Mark agents as "stopping" + background refresh |
| Per-agent `actionLoading` | Keep, but clear after API response |

---

## Additional Responsiveness Considerations

### Confirmation Dialogs

All destructive actions currently use `window.confirm()`, which is synchronous and blocks the browser's main thread (no rendering, no event handling) until the user responds. This is fine for infrequent operations like delete, but contributes to the "frozen" feel.

**Potential improvement**: Replace `window.confirm()` with Shoelace's `<sl-dialog>` for a non-blocking confirmation flow. This would allow the rest of the UI to remain interactive while the dialog is open.

**Tradeoff**: More implementation effort, but better UX consistency with the rest of the design system. This is lower priority than the core optimistic update work.

### Error Feedback

All error paths currently use `window.alert()`, which is also synchronous and blocking. Consider replacing with Shoelace's `<sl-alert>` toast notifications:

```typescript
// Instead of: alert('Failed to stop agent');
// Use: this.showToast('Failed to stop agent', 'danger');
```

This keeps the user informed without blocking interaction.

### Loading Indicators During Background Refresh

Even with background refreshes, users benefit from subtle indicators that data is being synced. Consider a small, non-blocking indicator (e.g., a thin progress bar at the top of the page, or a subtle "syncing" badge) that appears during background refreshes without disabling any interactive elements.

---

## State Convergence Model

The proposed model has three convergence paths, ordered by speed:

```
Action triggered
  |
  |-- (1) Optimistic update: ~0ms (immediate local state change)
  |
  |-- (2) SSE delta: ~50-500ms (server publishes event after store write)
  |       - Updates status, phase, activity
  |       - Does NOT include capabilities
  |
  |-- (3) Background refresh: ~200-2000ms (full REST API fetch)
  |       - Updates everything including capabilities
  |       - Seeds StateManager for future SSE delta merging
  |
  v
  UI is fully consistent
```

In the happy path for start/stop, the user sees the optimistic status update instantly, the SSE delta confirms it within a second, and the background refresh fills in any gaps (capabilities, full metadata) within a couple of seconds. For delete, the user sees a per-button spinner until the server confirms removal, then the agent is removed from the list. In all cases, the rest of the UI remains interactive.

### Failure Modes

| Failure | Impact | Mitigation |
|---------|--------|------------|
| API action fails (start/stop) | Optimistic status is rolled back, error toast shown | User sees brief flash of transitional status, then correction |
| API action fails (delete) | Spinner clears, error toast shown | No incorrect state shown — agent was never removed from the list |
| SSE connection lost | No real-time deltas | Background refresh still works; SSEClient reconnects with exponential backoff |
| Background refresh fails | Capabilities may be stale | SSE deltas still update status; next user-initiated navigation triggers a fresh load |
| SSE event dropped (buffer full) | Specific state change missed | Background refresh catches it; `StateManager` logs a warning |
| All three fail | UI shows optimistic state indefinitely | Extremely unlikely; next page navigation resets everything |

---

## Implementation Priority

1. **Layer 1 (optimistic updates)** — Highest impact, moderate effort. Focus on `agents.ts` and `grove-detail.ts` first since they affect multi-agent views where the blocking is most noticeable.
2. **Per-action loading on agent-detail** — Small, isolated change with clear benefit.
3. **Layer 2 (background refresh)** — Extract `backgroundRefresh()` as a shared pattern. This pairs naturally with Layer 1.
4. **Replace `window.confirm/alert`** — Cosmetic but contributes to perceived responsiveness.
5. **Layer 3 (SSE refinement)** — Mostly already in place; just ensure optimistic state and SSE state merge cleanly.

---

## Non-Goals

- **Eliminating the REST API fetch entirely**: SSE deltas are partial by design. Full REST fetches remain necessary for capabilities and complete metadata. The goal is to make them non-blocking, not to remove them.
- **Client-side caching/persistence**: No localStorage or IndexedDB caching of agent state. The `StateManager` in-memory map is sufficient; it's populated on each page load.
- **Debouncing SSE updates**: The current per-event rendering is fine for typical agent counts. If performance degrades with hundreds of agents, batching SSE updates with `requestAnimationFrame` could help, but that's a separate concern.
- **Major server-side refactoring**: The server's event publishing, request handling, and API response formats do not need redesign. However, server-side contention may contribute to observed delays (see below).

---

## Server-Side Response Time Concerns

The client-side improvements described above address the UI blocking pattern, but observed delays of 5–10 seconds suggest that server-side contention may also be a factor. The client-side changes will mask slow responses by keeping the UI interactive, but they do not fix the underlying latency if the server itself is slow to respond.

### Potential Causes

- **Database locking**: SQLite uses file-level locking. Concurrent writes (e.g., multiple agents updating status simultaneously) could cause write contention, with requests queuing behind an exclusive lock. Under load, this could add seconds of latency to REST responses.
- **Store-layer serialization**: If the store implementation holds a mutex across the full read-modify-write cycle for agent updates, read requests (like the full agent list fetch) may block behind ongoing writes.
- **Event publishing backpressure**: Although the `ChannelEventPublisher` uses non-blocking sends, if subscriber goroutines are slow to drain their 64-element buffers, the publisher may interact with slow consumers in ways that indirectly delay request completion.

### Recommended: Client-Side API Response Time Logging

To distinguish client-side rendering delays from server-side latency, add timing instrumentation to `apiFetch`:

```typescript
const API_SLOW_THRESHOLD_MS = 2000;

async function apiFetch(url: string, options?: RequestInit): Promise<Response> {
  const start = performance.now();
  const response = await fetch(url, options);
  const elapsed = performance.now() - start;

  if (elapsed > API_SLOW_THRESHOLD_MS) {
    console.warn(
      `[api] Slow response: ${options?.method ?? 'GET'} ${url} took ${elapsed.toFixed(0)}ms`
    );
  }

  return response;
}
```

This surfaces slow API responses in the browser console without requiring server-side changes, making it easy to identify whether observed delays are network/server-bound or client-rendering-bound.

### Recommended: Server-Side Request Duration Logging

On the server side, add middleware or per-handler logging that warns when a request exceeds a duration threshold:

```go
func loggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        next.ServeHTTP(w, r)
        elapsed := time.Since(start)
        if elapsed > 2*time.Second {
            log.Warn().
                Str("method", r.Method).
                Str("path", r.URL.Path).
                Dur("elapsed", elapsed).
                Msg("slow request")
        }
    })
}
```

This would surface database locking or other server-side contention as actionable warnings in the server logs, rather than leaving slow responses undiagnosed.