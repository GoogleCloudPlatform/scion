# Release Notes (2026-06-12)

Two major feature PRs landed: Skill Bank M5 adds federated skill resolution across GitHub, GCP Vertex AI, and external registries, while the messaging overhaul hardens error contracts, delivery feedback, and agent wake semantics across all channels.

## 🚀 Features
* **[Skill Bank — M5a: Routing Resolver]:** `RoutingSkillResolver` dispatches skill references by URI scheme to registered resolvers (`skill://`, `gh://`, `gcp-skill://`, full GitHub URLs), with the hub resolver as fallback. Wired at CLI and broker call sites, wrapped by the caching resolver (#408).
* **[Skill Bank — M5b: GitHub Resolver]:** `GitHubSkillResolver` resolves `gh://` URIs and full GitHub URLs via the GitHub Contents API, with input sanitization and response size limits (#408).
* **[Skill Bank — M5c: Federation & Registries]:** External skill registry management with CRUD admin API, federation proxy for cross-registry resolution, and trust enforcement (trusted pass-through or pinned hash verification). CLI commands under `scion skills registries`. Security hardening: 10MB body size limit, redirect-following disabled to prevent credential leakage, reusable HTTP client (#408).
* **[Skill Bank — M5d: GCP Vertex AI Resolver]:** `gcp-skill://` URI resolution via Vertex AI Skills API using Application Default Credentials. Version validation, SSRF defense (HTTPS-only, same-host download URLs, no link-local/RFC1918 targets), and 1MB metadata response limit (#408).
* **[Messaging — Error Contracts]:** Non-existent agent targets now return proper errors instead of creating orphan message rows. Scheduled events targeting deleted agents are marked as failed. Hub API 404 responses include agent slug and project context (#409).
* **[Messaging — Delivery Feedback]:** Persistence failures return 500 (was silent 200), missing recipients return 400 (removed silent creator fallback), broker dispatch failures return 502. Successful sends include `message_id`, `status`, `recipient`, and `recipient_id` in the response (#409).
* **[Messaging — Agent Phase Pre-Check]:** `handleAgentMessage` now returns 409 Conflict for non-running agents with actionable guidance (suspended: use `--wake`, stopped/error: use `scion start`) (#409).
* **[Messaging — Wake Improvements]:** Wake timeout bumped from 15s to 30s matching broker retry deadline. Distinct error for wake-success-delivery-failure. Messages to suspended agents without `--wake` now rejected with clear error (#409).
* **[Messaging — Integration Feedback]:** Telegram plugin validates default agents before routing, reports Hub delivery errors back to originating chat with error cooldown (max 1 per 5min per chat+thread+error-type) and remediation suggestions (#409).

## 🐛 Fixes
* **[Build]:** Corrected `runId` JSON key mismatch in build polling that caused polling to silently fail (#410).

## 🔧 Chores
* **[Docs]:** Added Observability section to glossary clarifying infrastructure metrics (`scion.hub.*`, `scion.db.*`) vs agent metrics (`gen_ai.*`, `agent.*`) and the telemetry pipeline (#407).
