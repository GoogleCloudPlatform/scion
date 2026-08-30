# Release Notes (2026-08-29)

Per-agent messaging authorization ships end-to-end (D1-D10 message modes, conversation model at handler ingress, CLI grammar, admin UI), a credential-leak prevention sweep removes secrets from error output, argv, and deploy scripts, and the conversation model advances through tranches C4-C6 with dual-write for webchat topics and reachability guards.

## 🔒 Security
* **Credential leak prevention (#1364, #1387, #1385, #1383):** Runtime error and debug paths no longer expose secrets — argv and env redacted from sandbox error output, access token removed from curl's argv in deploy.sh, and agent secrets now fetched from the hub at init rather than passed via command-line arguments.
* **Caller-supplied conversation_id authorization (#1403):** Server now authorizes conversation_id values supplied by callers, preventing unauthorized access to conversations by ID guessing.
* **Broker inbound sender authorization unified (#1411):** All broker inbound senders authorized through a single uniform path, eliminating inconsistencies across entry points.
* **@email path validation (#1407):** ResolveConversation now validates @email paths before processing, preventing malformed email addresses from reaching the resolver.

## 🚀 Features
* **Per-agent messaging authorization (D1-D10) (#1371, #1382, #1374):** Complete message-mode system with per-agent authorization controls, conversation envelope/delivery/validation library, and admin UI with mode badges, reachability indicators, mode controls, and templates.
* **Conversation model at handler ingress (#1391, #1380, #1381):** Conversation model enforced at handler ingress with authz reachability guard (C5), webchat topics dual-written into the conversation model (C4), and CLI conversation-reference grammar with flag deprecation (C6).
* **Per-type UAT manage scope aliases (#1404):** Adds manage scope aliases per resource type, reducing the number of individual scopes users need to remember.
* **Agent env var provenance classification (#1384):** Each agent environment variable now carries provenance metadata (hub-injected, user-defined, runtime-derived), enabling audit and debugging of variable origins.
* **Permissions UI polish (#1395, #1394, #1396):** Role-binding display names with reusable principal picker, view-permissions modal for roles, and admin nav visibility fixed for plain members with hub-admin route guard.
* **CountUnbackfilledMessages store method (#1373):** New MessageStore method to count messages needing backfill, supporting the conversation model migration.

## 🐛 Fixes
* **Race condition guards (#1409, #1405):** channelRegistry, pluginManager, dispatcher, and webChatStore reads now guarded with s.mu, fixing data races surfaced by the new nightly race detection job.
* **Guard scope and cross-project sentinel defects (#1379):** Closed DEF-39b and DEF-40 — guard scope was too narrow in some paths, and cross-project sentinels could leak.
* **Legacy-pending sentinel removed (#1401):** Removed the legacy-pending sentinel and split the validation choke point, cleaning up a source of confusion in the messaging pipeline (DEF-41).
* **Backfill 'message' action into existing policies (#1377):** Existing project member policies now include the 'message' action, ensuring messaging works for projects created before the authorization system.
* **Agent token file write check (#1386):** Agent refuses to start when the token file cannot be written, failing fast instead of silently running without credentials.
* **CLI precondition ordering (#1402):** Preconditions hoisted above conversation resolve, preventing confusing errors when prerequisites are missing (DEF-48).
* **Extras module repair (#1398):** Six broken modules in extras/ repaired with a new CI gate to prevent future breakage.
* **Hook response contract (#1400):** Data-driven hook response contract for antigravity, replacing ad-hoc response parsing.
* **Harness provider routing (#1397):** OpenCode vertex-ai provider routing fixed — GITHUB_TOKEN collision no longer breaks routing.

## 🔧 CI & Infrastructure
* **Nightly race detection (#1408):** New CI job runs tests with `-race` flag nightly, surfacing data races before they hit production.
* **Full test suite reporting job (#1388, #1392):** Reporting-only full test suite job added; pipefail ensures the test step reports the real exit code.
* **Authz gate hardening (#1410, #1376, #1372):** AST validation added for DEF-50/DEF-37/DEF-56 authz gates; security marker gate updated for authorizeAgentMessage; conversation upsert guard widened for kind='group' topic mints.

## 📖 Docs
* **Message mode glossary (#1389):** Glossary entries added for message modes and related messaging authorization terminology.
