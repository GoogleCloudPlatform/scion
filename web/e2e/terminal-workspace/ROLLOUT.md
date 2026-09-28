# Terminal Workspace — Rollout / Rollback

## Feature Flag

- **Name**: `web.terminal_workspace`
- **NOT** in `DEFAULT_ON_FLAGS` — defaults to OFF when absent.
- **Header constant**: `TERMINAL_WORKSPACE_FLAG` (`web.terminal_workspace`)

## Injection Precedence

1. **Server injection** — `window.__SCION_FEATURES__['web.terminal_workspace']`
   set by the Go template. Takes highest precedence.
2. **localStorage override** — `scion:feature:web.terminal_workspace` (`"true"` / `"false"`).
   Used for dev/QA. Checked only when server injection is absent.
3. **Default** — OFF (not in `DEFAULT_ON_FLAGS`).

## Rollout (Enable)

- **Server**: inject `window.__SCION_FEATURES__ = { 'web.terminal_workspace': true }` in
  the Go SPA template. Applies to all users on page load.
- **Per-user override**: `localStorage.setItem('scion:feature:web.terminal_workspace', 'true')`.

## Rollback (Disable)

- **Server (global)**: set `'web.terminal_workspace': false` in the injected features.
  This overrides any retained `localStorage` value of `true`. Simply removing the
  key is insufficient for global rollback if users have set localStorage overrides
  — a retained `localStorage` value of `"true"` would still enable the flag via the
  fallback precedence chain.
- **Per-user**: `localStorage.setItem('scion:feature:web.terminal_workspace', 'false')`.
  For full per-user rollback, also clear any cached value:
  `localStorage.removeItem('scion:feature:web.terminal_workspace')`.
- **Behavior**: active retained sessions are preserved in memory until the next
  page reload. On reload, the flag is re-evaluated and the app reverts to the
  legacy disposable-pane mode (`/agents/{id}/terminal`). In-memory terminal state
  (scrollback, xterm instances, layout assignments) is discarded on page reload.
  The agent process continues running server-side; users can re-attach after reload.

## Prerequisites

- All P3 sibling issues merged (reconnect, ownership, teardown, layout, entry points).
- \#1661 runtime UAT (Docker/live-broker) complete.

## Flag Removal

Do NOT remove the flag or add it to `DEFAULT_ON_FLAGS` until a separately
evidenced rollout decision confirms production stability.

## Native Chat Interaction

- **Chat flag**: `web.native_chat` (header constant `NATIVE_CHAT_FLAG`).
- Chat availability is independent: the header shows "Chat" when
  `web.native_chat` is ON, and "Terminals" when `web.terminal_workspace` is ON.
  Both modes coexist in the header's mode switch bar.
- The server's `nativeChatEnabled` setting (from `/api/v1/settings/public`)
  can disable chat flags at boot via `setFeatureFlag()`. This does NOT affect
  the terminal workspace flag, which has its own injection path.
