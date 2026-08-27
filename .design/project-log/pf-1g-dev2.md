# Phase 1G Delegation Ceiling Fixes (pf-1g-dev2)

**Date:** 2026-08-25
**Agent:** pf-1g-dev2 (Scion Agent)
**Branch:** scion/auth-refactor
**Base commit:** 3597507

## Summary

Fixed 3 blockers, 3 major items, 1 minor item, and 1 urgent addition (5 sub-items)
in the Phase 1G live delegation ceiling implementation. All changes land in a single
atomic commit.

## Items Addressed

### Blockers

- **1G-1: Grandfathered bypass removed.** The `Grandfathered` flag was being used to
  bypass the live delegation ceiling check, meaning grandfathered agents were never
  subject to the ceiling. Fixed by making `Grandfathered` provenance/audit metadata
  only — all edges now get the live ceiling check regardless of flag state.

- **1G-2: No-edge allow replaced with backfill marker gate.** Previously, agents
  with no delegation edge were unconditionally allowed. Replaced with a check
  against the `migration_delegation_edge_backfill_v1` hub setting marker. Before
  the backfill runs, hub-attested (local) agents are temporarily allowed. After
  the backfill completes, all agents must have edges — missing edge = deny.

- **1G-7: Backfill edge creation failures fail the migration.** Edge creation
  errors were silently skipped with `continue`. Changed to `return fmt.Errorf(...)`
  so a single edge creation failure halts the entire migration, preventing
  half-backfilled states.

### Major Items

- **1G-3: isMintingOperation expanded.** Added `ActionMint` and `ActionAssign` to
  the set of authority-granting actions that fail closed on store errors.

- **1G-5: AncestryIsHubAttested fixed.** Converted from a method on concrete
  `HubIdentity` (always true) to a standalone function using the `FederatedIdentity`
  interface. Hub-attested = NOT federated, with nil-safe guard.

- **1G-6: Orphaned delegation handler.** When a delegation edge points to a
  delegator that cannot be found (`store.ErrNotFound`), the ceiling is now frozen
  at the agent's own granted role. Minting operations are denied; read operations
  are allowed at the frozen ceiling. This handles synthetic delegators like
  `system/migration`.

### Minor

- **server.go scheduler dispatch:** Default edge role changed from `"full"` to
  `AgentRoleNone`, with proper extraction from `agent.AppliedConfig`.

### Urgent Addition — Item 8

- **8a: Unique constraint on delegation edges.** Added `.Unique()` to the ent
  schema index on `(delegate_type, delegate_id, scope_type, scope_id, active)`.
  Regenerated ent code.

- **8b: Idempotent backfill.** Backfill now checks for existing active edges
  before creating new ones. Repeated migrations do not create duplicates.

- **8c: Ordered edge queries.** Added `Order(ent.Asc(delegationedge.FieldCreated))`
  to `GetDelegationEdgesForDelegate` for deterministic results.

- **8d: Comment correction.** Updated algorithm doc-comment to describe the
  actual branching logic (delegator resolvability, not grandfathered flag).

- **8e: Duplicate edge fail-closed.** If multiple active edges are found for the
  same scope (invariant violation), minting operations fail closed. Non-minting
  operations use the first (oldest) edge with a warning.

### Additional QA Tests

- **Federation rejection test:** `TestExtractHubClaims_AncestryDisagreesWithRootUser`
  verifies that federated ancestry disagreeing with root user is rejected.

- **Per-element rejection test:** `TestFederationAuth_AncestryPerElementRejection`
  verifies that each non-hub-attested element in a federated ancestry chain is
  individually rejected.

### Super-Admin Trap Fix

All delegation ceiling tests now assert that the test subject is NOT a system
admin, preventing tests from passing via the super-admin bypass instead of the
delegation ceiling logic.

## Files Modified

| File | Change |
|------|--------|
| `pkg/hub/authz_delegation_ceiling.go` | Core ceiling logic: removed grandfathered bypass, added backfill marker gate, orphaned delegation handler, duplicate edge detection, expanded isMintingOperation |
| `pkg/hub/identity.go` | AncestryIsHubAttested as typed predicate function |
| `pkg/store/entadapter/composite.go` | Backfill fixes: default role "none", fail on edge creation error, idempotent edge creation |
| `pkg/hub/server.go` | Scheduler dispatch default role fix |
| `pkg/ent/schema/delegationedge.go` | Unique constraint on delegation edges |
| `pkg/ent/` (generated) | Regenerated ent code for unique constraint |
| `pkg/store/entadapter/delegation_edge_store.go` | Ordered edge queries |
| `pkg/hub/delegation_ceiling_test.go` | 6 new tests + super-admin trap assertions |
| `pkg/store/entadapter/delegation_edge_backfill_test.go` | 4 new backfill tests |
| `pkg/hub/federation_auth_test.go` | 2 federation rejection tests |
| `pkg/hub/handlers_test.go` | testServer backfill marker cleanup |
| `pkg/hub/authz_bypass_agents_test.go` | bypassAgentsServer backfill marker cleanup |
| `pkg/hub/authz_test.go` | authzTestSetup comment update |

## Testing

- All `pkg/hub/` tests pass (248s)
- All `pkg/store/entadapter/` tests pass (7s)
- Full `make ci` passes clean (formatting, build, all test suites)

## Key Design Decisions

1. **Branch on delegator resolvability, not Grandfathered flag.** The core security
   fix: real delegators get live checks, synthetic/missing delegators freeze the
   ceiling, store faults fail closed for minting and open for reads.

2. **Backfill marker as migration gate.** The `migration_delegation_edge_backfill_v1`
   hub setting acts as a feature flag — before it's set, agents without edges are
   allowed (backward compatibility during migration). After it's set, missing
   edges = deny.

3. **Test infrastructure: marker cleanup.** `testServer()` runs `Migrate()` which
   sets the backfill marker. Tests that create agents directly via store (without
   edges) would be denied post-backfill. Fixed by deleting the marker after
   `Migrate()` in test setup, so only tests that explicitly set the marker are
   affected by post-backfill behavior.
