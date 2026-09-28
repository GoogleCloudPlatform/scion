# Release Notes (2026-09-18)

A2A standalone HA infrastructure lands (Google credential exchange, durable PostgreSQL task state, authenticated gRPC transport), three-channel release model with nightly automation and version checking, and jump-to-message from search in native chat.

## 🚀 Features
* **A2A standalone HA infrastructure (#1741, #1742, #1743, #1748):** Google credential exchange into short-lived Hub user tokens with durable identity bindings and Gemini Enterprise JSON-RPC compatibility (#1741). PostgreSQL-backed A2A SDK tasks with authoritative ownership, cross-replica correlation/resubscription, execution leases, atomic crash reap, and scoped event/artifact deduplication (#1742). Fail-closed gRPC auth with refreshable Google credentials, Hub-principal checks, Cloud Run dual headers, TLS/mTLS, and bounded JWKS caching (#1743). Deterministic multi-process integration tests with Hub/two-bridge/PostgreSQL topology (#1748).
* **Release manifest and nightly channel (#1761):** `LATEST.json` at repo root with stable/preview/nightly channels. `build-nightly.yml` with daily 2AM UTC cron, skip-if-no-commits, and 14-day cleanup.
* **Version update checking (#1760):** `scion version --check` flag with text and JSON output modes for querying update availability against the release manifest.
* **Jump-to-message from search (#1749):** Clicking a search result outside the loaded 50-message buffer fetches messages around the target via new `around` query parameter on the history endpoint. Highlight-flash animation on target, "Jump to latest" to reset, and SSE isolation for detached search windows.
* **Default agent highlight in members sidebar (#1751):** Thread default agent shows a "thread default" label below its name in the AGENTS section.
* **Conversation platform skill (#1738):** New `scion-conversation` skill documenting all 10 subcommands with cross-links to `scion-messaging`. Clarifies reading/admin vs sending distinction and `conv:` reference-prefix requirement.

## ⚡ Performance
* **Chat typing responsiveness (#1766):** Reuse date formatters and stable props, skip transcript work for typing/status-only updates, avoid closed autocomplete/menu rerenders, use native textarea content sizing. Synthetic 500-message typing updates dropped from ~34ms to <1ms; layout passes halved.

## 🐛 Fixes
* **Codex reasoning effort key (#1763):** Provisioner wrote `reasoning_effort` but codex CLI reads `model_reasoning_effort` — all codex agents ran at medium regardless of `thinking_level`. Expanded mapping from 3 buckets to 4 quartiles (low/medium/high/xhigh). Removed hardcoded medium from image config.
* **Harness-config cache invalidation (#1762):** Co-located brokers served stale harness configs after hub re-bootstrap because `ResolveWithHash` blindly trusted the caller's content hash. Now always verifies against hub; added `Cache.Invalidate` for explicit entry removal.
* **Agent creation permissions (#1754, #1753, #1756):** `ComputeScopeCapabilities` now reports `agent.create` when any project grants the permission, not just hub-scope roles (#1754). Scope capabilities keyed per resource type so the New Agent button renders correctly regardless of navigation order (#1753). Project picker lists projects where user has `agent.create` capability instead of owner-only (#1756).
* **Promote DM error display (#1750):** Error toast showed `[object Object]` — backend returns `{code, message}` but frontend typed it as string. Project name fallback added for DMs that never populated `projectSlug`.
* **Members sidebar click deselection (#1752):** Clicking empty space in the members panel deselected the current conversation. Removed the host-level click handler copied from the space rail where it is correct UX.
* **Debug panel visibility (#1755):** Debug toggle is now opt-in and only renders when debug is actually available, preventing it from covering chat sidebar action buttons.
* **Classification catalogs (#1747):** Registered new routes, mutations, and permissions from cross-project messaging, conversation management, and A2A PRs. 8 catalog tests fixed.
* **Conversation upsert guard (#1745):** Exempted conversation management API handlers from the upsert guard (Option D) — they use `CreateConversation`/`AddParticipant` directly under `UnifiedAuthMiddleware`.

## 📖 Docs
* **Release process documentation (#1759):** Three-channel model (nightly/preview/stable), branch taxonomy, operator runbook for release scripts, `LATEST.json` manifest format, CI/CD workflow architecture.
