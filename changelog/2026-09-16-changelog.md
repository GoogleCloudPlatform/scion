# Release Notes (2026-09-16)

Major native chat overhaul and surface-agnostic conversation API. Thread drag-and-drop with collapsible groups, context menus replace hover toolbar, bidirectional mention translation, layout density toggle, and space emoji icons. First external contributor PRs in this cycle.

## 🚀 Features
* **Native chat overhaul (#1672, #1680, #1674, #1673, #1681, #1684, #1683):** Right-click context menu replaces hover toolbar (net -500 lines). Thread drag-and-drop reorder with named collapsible groups (native HTML5 drag API, server-persisted via user-prefs). Layout density chooser (dense/comfy, 160 CSS values replaced across 13 files). Optional emoji icons for spaces stored in project annotations. Bidirectional mention format translation between chat and agents (`@firstname-lastname` ↔ `@email`). Follow-up fixes for DnD, group creation, and dismissal persistence.
* **Conversation CLI and participant management (#1671, #1678, #1686, #1685):** Surface-agnostic `scion conversation` command with hub API endpoints — list, messages, get, create, set-default, participants, join, leave, catch-up. Auto-sets `ProjectID` on conversation create via `AgentIdentity`. All subcommands added to agent-mode allowlist.
* **Unified template authoring UX (#1665):** Replaces standalone 700-line `profile-templates.ts` with composition of shared `resource-list` and `resource-import` components. Extends shared components with `scope='user'`, `canCreate`, `canRename` affordances. Adds user-scope support to backend import/discover endpoints.
* **Hub-scope BYO service account registration (#1675):** Removes frontend guard that blocked the Register Existing button at hub scope. Backend has accepted hub-scope BYO registration since P9.

## 🐛 Fixes
* **Chat member resolution (#1669):** People section used legacy `project:slug:members` group lookup instead of role bindings (PM1 redefinition). Only project creator showed up. Replaced with `ListProjectMembers()`. Also fixes @mention resolution for non-creator members.
* **Unread blue dot on first DM open (#1668):** Read watermark was never written when messages fit in viewport because `advanceReadWatermark` required `showUnreadDivider` on initial load. Now advances unconditionally.
* **Mention fan-out display duplicates (#1667):** When @-mentioning multiple agents, one DB row per recipient was rendered in chat. Frontend now filters `type:mention` copies from the display array while retaining them in `messageMap` for dedup.
* **Claude model upgrade dialog (#1663):** `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` suppresses the blocking "Newer Opus model available" dialog at agent startup when `ANTHROPIC_DEFAULT_OPUS_MODEL` is set by broker infra.
* **Dev-auth gating for managed agents (#1664):** Localhost dev-auth preference silently swapped hub-managed agents onto dev auth, causing 401s on self-only endpoints. Gated behind `!IsHubManagedAgent()` at 3 guard sites.
* **Web asset build-time enforcement (#1660):** Compile-time sentinel embed for `assets/main.js` catches missing web build at compile time. Fixes blank page when `dist/client` contains only `.gitkeep`.
* **File preview vs edit size limits (#1640):** Separates read-only file preview limit (100MB) from edit limit (1MB). Chat file preview passes `mode=preview` for the larger limit.
* **Single-node-vm live-deployment fixes (#1655):** 9 blockers and improvements from live testing — systemd ExecStart, releases/latest 404 fallback, IAP tunnel role grant, SA name parameterization, `SESSION_SECRET` prefix consistency, health check fatal, SSH stderr capture.
* **Agent delete confirmation clarity (#1661), remove stale Sessions/Tokens columns (#1662), TypeScript compilation (#1682), mention-sticky tab removal (#1670), authzop catalog for GCP SA clone (#1657), workspace-mode backfill (#1656).**
* **A2A bridge multi-turn and artifact text (#1676, Zainab Ahmed Safeer):** Preserves artifact reply text, fixes multi-turn subscriptions, raises HTTP write timeout.
* **Telegram sender ID resolution (#1232, Juan Pablo Urzua):** Resolves registered sender's Hub user ID for outbound `SenderID`.
* **Web reply affinity channel gate (#1677, Zainab Ahmed Safeer):** Only records web reply affinity when message channel is explicitly web.
