# Release Notes (2026-08-27)

Permissions Phase 2 completed in a single day — all admin handler bypasses converted to permission-based checks across 7 PRs (A1–A7), a full quota/limits system shipped (B1–B3), two DM security fixes closed cross-project injection vectors, and the conversation model foundation landed for the next messaging layer.

## 🔒 Security
* **[Chat]:** Validate DM key ownership at all 3 message ingress points — closes cross-project DM injection vulnerability where an authenticated agent could write into another agent's DM conversation by supplying a well-formed but unauthorized DM key. Uses authenticated identity from request context to prevent spoofing (#1322). Validate agent-supplied `thread_id` against canonical DM key format at ingress — malformed DM keys rejected with 400 before dispatch or persistence (#1319).

## 🚀 Features
* **[Auth]:** Permissions Phase 2 Track 1 (Admin Handler Conversion) — **A1:** 35 admin permission IDs, `routeGuard` Decide path with `requireAdmin` fallback, hub-admin system role (#1324). **A2:** Settings/config handlers + UAT hub-resource deny (#1327). **A3:** User management handlers (#1329). **A4:** Operations handlers — super-admin-only ops properly gated (#1332). **A5:** Integrations and hooks handlers (#1333). **A6:** Resource admin handlers — 32 bypass sites converted (#1334). **A7:** Engine internals converted, `requireAdmin` deprecated — completes Track 1 (#1337).
* **[Auth]:** Permissions Phase 2 Track 2 (Quota System) — **B1:** `LimitDefinition`, `EntitlementBinding`, `UsageReservation` ent schemas, `QuotaStore` interface (15 methods), 3 seeded system limit definitions (#1323). **B2:** Advisory-lock-based quota enforcement at agent/project creation with fail-closed semantics and reservation leak protection (#1328). **B3:** 13 quota API endpoints with route guard read/write permission split and system limit modification protection (#1336).
* **[Messaging]:** Conversation model foundation — `Conversation`, `ConversationParticipant`, `MessageAddressee` ent schemas, conversation store adapter with key-derived participant immutability guard, `pkg/messaging` with conversation key derivation/resolution/authorization/backfill, `DMConversationKey` for order-independent DM keys. Not yet wired to production code; first of six tranches (#1331).

## 🐛 Fixes
* **[CLI]:** Resolve truncated schedule IDs via client-side prefix matching — all 6 schedule subcommands updated (#1318).
* **[Build]:** Right-size `cloudbuild-omni.yaml` timeout from 14400s to 2400s (measured build: 641s). Add empty `.gitignore` to fix gcloud 582.0.0 build failure (#1316).
* **[Hub]:** Fix ADC preflight (#1335). Remove reset auth button from agent detail page (#1342).
* **[Hosted]:** Move single-node Cloud Run deploy from Go command to `scripts/single-node/deploy.sh` (828 lines deleted), remove references to private project images (#1325).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 26 — Permissions Foundation, Cloud Run sandbox tier, Helm Phases 2–3, secret PATCH, security fixes (#1314). Single-node Cloud Run design doc corrections — fully qualify 18 issue references, measured capacity figures, IAP perimeter verification (#1317). Document `--image-registry` flag (#1321). Record gcloud release-track dependency (#1326). Replace unmeasured hub_id instruction with actual measurement (#1330). Single-node tutorial (#1315).
