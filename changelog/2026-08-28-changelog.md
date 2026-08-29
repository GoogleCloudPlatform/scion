# Release Notes (2026-08-28)

Permissions Phase 2 UI layer ships complete (role/binding/quota admin pages, capabilities-based nav, 30 new UAT scopes), messaging security hardening continues with DM participant guard consolidation and broadcast authorization enforcement, and four new CI security marker gates lock the handler-layer invariants.

## 🔒 Security
* **Broadcast & chat dispatch authorization hardened (#1347, #1343):** Project membership now required for broadcast calls; sender identity forced server-side; DM conversation keys derive from the authenticated caller, not the payload, closing a spoofed-sender conversation-selection attack.
* **DM participant guard consolidation (#1349, #1360):** Extracted `CheckDMParticipantKey` as a single shared predicate; all three ingresses (AddParticipant, EnsureParticipant, mergeConversation) route through it so the D-1 immutability invariant cannot diverge.
* **DMConversationKey canonicality enforcement (#1362):** Non-canonical kinds and UUIDs rejected at derivation instead of silently accepted; no normalisation on the derivation path.
* **Default agent resolver scoped (#1338):** Resolver fallback no longer binds threads to foreign-project or soft-deleted agents; error text unified to avoid leaking deletion state as an existence oracle.

## 🚀 Features
* **Permissions Phase 2 admin UI complete (#1357, #1356, #1355):** Role/binding CRUD pages, quota management admin page, and capabilities-based nav that replaces the binary role===admin check with scoped visibility (hub-admin vs super-admin).
* **Role & binding management API with CanDelegate enforcement (#1348):** Role definition CRUD with system-role immutability, role-binding CRUD with pagination, D10 guard for nonexistent permissions, and a critical CanDelegate check on updateRoleDefinition closing a privilege escalation vector.
* **UAT scopes for 8 resource types (#1346):** 30 new scopes (skill, template, harness_config, group, user, broker, gcp_service_account) and a `GET /api/v1/auth/scopes` endpoint for dynamic scope discovery.
* **Conversation stamping and publish guards (#1353):** Phase 5 dual-write conversation stamping across all 12 CreateMessage sites; SSE publish gated on successful persist; AST enumeration tests lock both invariants against silent regression.

## 🐛 Fixes
* **Single-node deploy portability (#1350, #1352):** deploy.sh fixed for macOS bash 3.2 (`${var,,}` replaced); cloudrun-sandbox runtime profile seeded on hosted tier so agents can start on fresh deploys.
* **DM participant re-registration (#1349):** New `EnsureParticipant` does insert-if-absent without clearing `left_at`, fixing a bug where leaving a DM was undone by the next message. Nil ParticipantEnsurer guard prevents eventbus panics.
* **Messaging UI visibility restored (#1341):** Frontend updated from `message` to `attach` capability check across 7 call-sites after the A1-A6 permission refactor.
* **DM participant guard migration abort (#1360):** Restamp failure now aborts the migration instead of continuing; empty-ref direct rows left keyless rather than fabricating an ACL from the listing index.

## 🧪 Tests
* **Scoped admin integration tests (#1354):** 35 tests across 6 groups covering hub-admin access/denial, project isolation, CanDelegate constraints, and combined roles.
* **UAT enforcement intersection tests (#1358):** 99+ tests across 13 functions validating scope+binding intersection, project constraints, alias expansion, and `gcp_service_account` dual-nature.

## 🔧 CI & Infrastructure
* **Security marker gates (#1339, #1361, #1363, #1366):** Four new CI gates covering conversation/participant table writes, B5/#1322/#1347 handler symbols, messagebroker/broker-inbound entry points, and Broadcasted/parseDMKeyIDs/isDMParticipant ownership — 25 rows total with function-scoped mutation testing.

## 📖 Docs
* **Conversation model design landed (#1367):** Messaging conversation-model semantic contract (2100 words) and defect inventory (366 words) committed as design records, with header noting post-Discussion-#1264 decision reversals.
* **Single-node tier design notes (#1351):** Operator prerequisites, atomicity requirements, REST credential contract, and profile-layer observations.
