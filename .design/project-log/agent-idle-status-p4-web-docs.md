# Agent idle status, phase 4: web display, admin settings and docs

**Date:** 2026-11-18
**Branch:** scion/agent-idle-status-p4 (stacked on scion/agent-idle-status-p3)
**Issue:** ptone/scion#7943 (parent ptone/scion#7826)

## What changed

- **Hub payloads (`pkg/hub`):** `suspendReason` was on the full REST agent only, so the web could not keep it current. It is now on the SSE `AgentStatusEvent` (always sent, like `activity`, so a resume clears a merged reason as `""`; a reason-only change such as the user conversion counts as a visible change), on the compact agent list item and on the chat space-members entry (both `omitempty`). The web's compact key set follows the hub's allowlist.
- **Web display (`web/src/shared`, `chat-members.ts`, `agent-detail.ts`):**
  - `idle` joins `AgentActivity`, `ACTIVITY_DISPLAY` (💤, `moon`, neutral, no pulse) and the badge `StatusType`.
  - `suspendedActivityLabel` became `suspendedStatusLabel`, which adds the known suspend reason in parentheses, matching `scion list`: `suspended · completed (idle)`, `suspended (user)`. Unknown reasons and stale reasons on a non-suspended agent are not shown. `suspendReasonHint` explains each reason; `agentStatusBadge` puts it in the badge tooltip (joined with a retained harness-error reason).
  - The chat members badge now carries the compound label (before, a suspended member read plain "suspended" on the badge), and its tooltip adds the reason hint. All four member mappers pass `suspendReason` through.
  - The agent page has a **Suspend reason** row and, with `lifecycle.idle_suspend` on, a **Keep suspended** action for an `idle`- or `stalled`-suspended agent (POST suspend, which phase 3's hub rewrites to `user` in place). The experiment gains `LayerWeb` so the browser can decide whether to offer the action.
- **Notification defaults:** `IDLE` joins the web `DEFAULT_TRIGGERS` and `ALL_TRIGGERS` (label "Idle"), matching the hub (`newNotifySubscription`) and the CLI. `HARNESS_ERROR` was already present.
- **Admin settings (`admin-server-config.ts`):** `server.hub.auto_suspend_idle` and `server.hub.idle_suspend_after` next to the stalled settings, sent by both save builders, with the 5m minimum hint and the experiment and production-readiness notes. The registry-driven save-payload test covers both keys (the opsettings web registry fixture already listed them from phase 3).
- **Docs:** concepts (the end-of-turn rule text, identical to the preamble, a status table, the background-job case, scheduled messages waking, idle as sticky and stall-excluded, suspend reasons), agent lifecycle (Auto-Suspend of Idle Agents: settings, grace, guards, cadence, auto-wake, Keep suspended, a gating table, the production warning), server-config and admin-settings rows, the experiments entry (what is not gated), the dashboard, and a release note.
- **Extras:** `idle` in scion-discord `agentStatusEmoji` (💤) and the agent-viz canvas icons (Bootstrap `moon-fill`).

## Decisions

- The reason suffix is the raw reason (`idle`, `user`, `stalled`), as `scion list` prints it, not the design sketch's "idle policy".
- The client-side sticky set (`state.ts` `STICKY_ACTIVITIES`) still leaves out `idle`, like `blocked`: the hub keeps it against end-of-turn working.

## Not in this phase

- A `max_lifetime` reason (ptone/scion#7869) shows no suffix until the web learns it.
- Failure-path durability of idle suspends (ptone/scion#8204); the docs and admin hint keep the production warning until it lands.
