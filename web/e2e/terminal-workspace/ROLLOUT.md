# Terminal Workspace — Rollout / Rollback

## Feature Flag

- **Name**: `web.terminal_workspace`
- **Registered experiment** (ptone/scion#2217), default ON. See the [Experiments reference](../../../docs-site/src/content/docs/reference/experiments.md) for the general mechanism.
- **Header constant**: `TERMINAL_WORKSPACE_FLAG` (`web.terminal_workspace`)

## Flag Precedence

1. **Pinned** — values in `window.__SCION_FEATURES__` before boot applies server flags, as the `*.pw.ts` init scripts in this directory set (e.g. `ownership.pw.ts`, `toast-lifecycle.pw.ts`, `url-layout.pw.ts`). These beat the server; production never pins this name.
2. **Server value** — `GET /api/v1/experiments`, fetched by the client at boot (signed-in users only). This is the admin override from the Experiments tab, or the registry default if none is set. Takes the highest of the production sources whenever the fetch succeeds.
3. **localStorage override** — `scion:feature:web.terminal_workspace` (`"true"` / `"false"`). Used for dev/QA. Applies only when the experiments fetch fails, or on a signed-out page load.
4. **Default** — ON (`web.terminal_workspace` is in `DEFAULT_ON_FLAGS`), used when the experiments fetch fails.

## Admin Control (Rollout / Rollback)

Hub-wide enable and disable is through the **Experiments tab**: Admin → Server Config → Experiments, with the `hub.experiments.update` permission (and `hub.config.read` to open the page). Toggling applies to all users of this hub; each user sees the change the next time they load or refresh the page. There is no separate "Save & Reload" step for this tab.

- **Per-user override (dev/QA only)**: `localStorage.setItem('scion:feature:web.terminal_workspace', 'true')` or `'false'`. This only takes effect when the experiments fetch fails or the user is signed out; for a signed-in user on a working hub, the admin value wins.
- **Behavior on disable**: active retained sessions are preserved in memory until the next page reload. On reload, the flag is re-evaluated and the app reverts to the legacy disposable-pane mode (`/agents/{id}/terminal`). In-memory terminal state (scrollback, xterm instances, layout assignments) is discarded on page reload. The agent process continues running server-side; users can re-attach after reload.

## Prerequisites

- All P3 sibling issues merged (reconnect, ownership, teardown, layout, entry points).
- \#1661 runtime UAT (Docker/live-broker) complete.

## Flag Removal

Do not remove the flag until a separately evidenced rollout decision confirms production stability (ptone/scion#1662). Graduating it follows the retirement convention in the [Experiments reference](../../../docs-site/src/content/docs/reference/experiments.md): delete the legacy code path, delete the registry entry, add the name to `compiledRetired`, and remove it from `DEFAULT_ON_FLAGS`.

## Native Chat Interaction

- **Chat flag**: `web.native_chat` (header constant `NATIVE_CHAT_FLAG`). This is not a registered experiment; it stays controlled by `server.native_chat.enabled` and does not appear in the Experiments tab.
- Chat availability is independent: the header shows "Chat" when
  `web.native_chat` is ON, and "Terminals" when `web.terminal_workspace` is ON.
  Both modes coexist in the header's mode switch bar.
- The server's `nativeChatEnabled` setting (from `/api/v1/settings/public`)
  can disable chat flags at boot via `setFeatureFlag()`. This does NOT affect
  the terminal workspace flag, which has its own resolution path.
