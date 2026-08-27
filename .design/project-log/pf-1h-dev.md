# Phase 1H: Agent Credential Revocation (F1.8) — Developer Log

**Date:** 2026-08-25
**Agent:** pf-1h-dev
**Branch:** scion/auth-refactor

## Summary

Implemented Agent Credential Revocation (F1.8) for the Permissions Foundation. This feature enables the Hub to revoke agent JWT tokens before their natural 10-hour expiry by tracking issued credentials in persistent storage and validating them on each request.

## Changes

### New Files

1. **`pkg/ent/schema/agentcredential.go`** — Ent schema for the AgentCredential entity with fields for tracking token JTI hashes, issuance/expiry times, revocation state, and last-seen timestamps. Includes indexes for JTI hash lookup (unique), agent+project queries, bulk revocation, and expiry-based cleanup.

2. **`pkg/store/entadapter/credential_store.go`** — Ent-backed implementation of the `AgentCredentialStore` interface. Supports CRUD operations, bulk revocation by agent, last-seen updates, and expired credential purging.

3. **`pkg/hub/credential_revocation_test.go`** — 15 comprehensive tests covering all acceptance criteria: token persistence, revoked token denial, deleted/suspended agent denial, refresh from revoked tokens, legacy token compatibility window, refresh migration, old credential revocation on refresh, bulk revocation on delete/suspend, store operations, and purge.

### Modified Files

4. **`pkg/store/models.go`** — Added `AgentCredential` store model.

5. **`pkg/store/store.go`** — Added `AgentCredentialStore` interface (6 methods) and embedded it in the `Store` interface.

6. **`pkg/store/entadapter/composite.go`** — Wired `AgentCredentialStore` into `CompositeStore`.

7. **`pkg/hub/agenttoken.go`** — Added `CredentialRecorder` interface, `hashJTI()` helper, and credential recording in `GenerateAgentToken` and `GenerateAgentTokenWithExpiry` (nil-safe, best-effort).

8. **`pkg/hub/auth.go`** — Added `CredentialStore` field to `AuthConfig`. Extended `UnifiedAuthMiddleware` Step 1 to validate agent tokens against persistent credential state: checks revocation, updates last-seen (fire-and-forget), and implements the compatibility window for pre-table tokens. Added context keys for credential ID and legacy token flag.

9. **`pkg/hub/identity.go`** — Added `TokenID()` method to `AgentIdentity` interface and `agentIdentityWrapper`.

10. **`pkg/hub/federation_identity.go`** — Added `TokenID()` method to `FederatedAgentIdentity` (returns empty string).

11. **`pkg/hub/handlers_policies.go`** — Added `TokenID()` method to `evaluateAgentIdentity`.

12. **`pkg/hub/handlers_agents_core.go`** — Added credential revocation on agent delete (before scheduled event cancellation). Extended `handleAgentTokenRefresh` to check credential state before allowing refresh, verify legacy token agent authorization, and revoke old credentials after successful refresh.

13. **`pkg/hub/handlers_agent_lifecycle.go`** — Added credential revocation on agent suspend (before phase update).

14. **`pkg/hub/server.go`** — Wired `storeCredentialRecorder` into `AgentTokenService` and `CredentialStore` into `AuthConfig`.

### Generated Files

15. **`pkg/ent/agentcredential/`** — Generated Ent code for the AgentCredential entity.

## Design Decisions

- **JTI hashing**: Raw JTI values are never stored; only SHA-256 hex hashes are persisted, preventing token reconstruction from database contents.
- **Best-effort revocation**: Lifecycle revocation calls (delete, suspend) log and continue on failure to avoid blocking primary operations.
- **Nil-safe recorder**: The credential recorder on `AgentTokenService` is optional, preserving backward compatibility for code paths that don't have the store wired.
- **Compatibility window**: Pre-table tokens (unknown JTI hash) are accepted with a warning log but flagged as legacy. Legacy tokens can refresh once (migrating to recorded tokens) but cannot refresh into unrecorded tokens.
- **Fire-and-forget last-seen**: The `last_seen_at` update runs in a goroutine to avoid adding latency to the auth path.

## Verification

- All 15 new tests pass (`TestNewlyIssuedTokensPersisted`, `TestRevokedTokenDeniedBeforeExpiry`, `TestDeletedAgentTokenDenied`, `TestSuspendedAgentTokenDenied`, `TestRefreshFromRevokedTokenFails`, `TestCompatibilityWindowAcceptsLegacyTokens`, `TestCompatibilityWindowRefreshMigratesLegacyToken`, `TestTokenRefreshRevokesOldCredential`, `TestRevokeOnAgentDelete`, `TestRevokeOnAgentSuspend`, `TestPurgeExpiredAgentCredentials`, `TestHashJTI`, `TestCredentialRecorderNilSafe`, `TestCredentialStoreOperations`, `TestBulkRevocationByAgent`).
- `make ci` passes (formatting, vet, lint, build).
- `go test ./pkg/hub -timeout=600s -count=1` — all new tests pass; 4 pre-existing flaky tests in unrelated files fail intermittently when run in suite but pass individually.
