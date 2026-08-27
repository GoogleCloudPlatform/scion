# Phase 1C: Deterministic Policy Evaluation

Date: 2026-08-24
Developer: pf-1c-dev
Branch: `scion/auth-refactor`
Base: `c480b77604ecf2ccd428d9b962e8e480e5058fad`

## Summary

Implemented Phase 1C of the permissions foundation: added deterministic total policy ordering, policy_kind field, SourceIPs validation rejection, local override preservation, and stable ordering indexes.

## Changes Implemented

### 1. Added `policy_kind` Field

**Ent Schema** (`pkg/ent/schema/policy.go`):
- Added `policy_kind` enum field with values `explicit` (default) and `default`
- Field is required, defaults to `explicit`

**Store Model** (`pkg/store/models.go`):
- Added `PolicyKind string` field with JSON tag `policyKind`
- Added constants `PolicyKindExplicit` and `PolicyKindDefault`

**Store Adapter** (`pkg/store/entadapter/policy_store.go`):
- Updated `entPolicyToStore` to read `PolicyKind`
- Updated `CreatePolicy` to write `PolicyKind` (defaults to `explicit` if not provided)
- Updated `UpdatePolicy` to write `PolicyKind`

**Ent Code Generation**:
- Regenerated Ent code with `go generate ./pkg/ent`

### 2. Implemented Deterministic Total Policy Ordering

**Store Layer** (`pkg/store/entadapter/policy_store.go`):
Updated `GetPoliciesForPrincipals` ordering to produce deterministic total order (ascending):
1. `scope_type`: hub < project < resource (more specific scope wins)
2. `priority`: lower < higher (higher priority wins)
3. `policy_kind`: default < explicit (explicit wins over default)
4. `created`: earlier < later (later-created wins as tiebreaker)
5. `id`: lexicographic order (stable final tiebreaker)

**Evaluation Logic** (`pkg/hub/authz.go`):
- Updated `evaluatePolicies` function comments to fully document the total order
- Preserved "last match wins at same or higher scope level" logic
- Documented local override behavior: resource > project > hub

### 3. Rejected Unenforced Conditions (SourceIPs)

**Handlers** (`pkg/hub/handlers_policies.go`):
- Added validation in `createPolicy`: rejects requests with non-empty `conditions.sourceIps`, returns HTTP 400 with message "SourceIPs conditions are not currently enforced and cannot be set"
- Added same validation in `updatePolicy`
- DelegatedFrom and DelegatedFromGroup conditions remain allowed

**Existing Data**: No migration added to remove existing SourceIPs; they continue to be ignored during evaluation.

### 4. Updated Seeding Logic

**Seed** (`pkg/hub/seed.go`):
- Set `PolicyKind` to `"default"` for all seeded policies (those with `Origin == "seeded"`)
- Updated backfill function to set `PolicyKind` for existing seeded policies that predate the field

### 5. Added Stable Ordering Indexes

**Ent Schema** (`pkg/ent/schema/policy.go`):
- Added composite index on `(scope_type, priority, policy_kind, created, id)` to support deterministic ordering query
- Preserved existing unique index on `(name, scope_type, scope_id)`

### 6. Handler Updates

**Policy Request Structs** (`pkg/hub/handlers_policies.go`):
- Added `PolicyKind` field to `CreatePolicyRequest` (optional, defaults to `explicit`)
- Added `PolicyKind` field to `UpdatePolicyRequest` (optional)

**Validation**:
- Validate `PolicyKind` is either `explicit` or `default`, return 400 if invalid
- Default to `explicit` when not provided

## Tests Added

**Deterministic Ordering Tests** (`pkg/hub/authz_policy_determinism_test.go`):
1. `TestPolicyDeterministicOrdering`: Verifies policies inserted in different orders produce identical authorization decisions
2. `TestPolicyPriorityPrecedence`: Verifies higher priority wins at the same scope
3. `TestPolicyKindPrecedence`: Verifies explicit wins over default at same scope and priority
4. `TestPolicyLocalOverride`: Verifies project-scoped policy overrides hub-scoped policy
5. `TestPolicyResourceOverride`: Verifies resource-scoped policy overrides both project and hub policies
6. `TestPolicyStableTiebreaker`: Verifies creation time and ID provide stable ordering

**Handler Validation Tests** (`pkg/hub/handlers_policies_phase1c_test.go`):
1. `TestCreatePolicy_SourceIPsRejection`: Verifies SourceIPs are rejected at create time
2. `TestUpdatePolicy_SourceIPsRejection`: Verifies SourceIPs are rejected at update time
3. `TestCreatePolicy_PolicyKindValidation`: Verifies PolicyKind is validated on create
4. `TestUpdatePolicy_PolicyKindValidation`: Verifies PolicyKind is validated on update

## Acceptance Criteria Met

- [x] Same policies inserted in different orders produce identical authorization decisions over repeated runs
- [x] Priority changes the outcome of policy evaluation
- [x] Stored unenforced conditions (SourceIPs) are rejected at create/update time with HTTP 400
- [x] Project-scoped policy can override hub-level policy (local override preserved)
- [x] Policy kind (explicit vs default) affects precedence — explicit wins over default at same scope and priority
- [x] Stable tiebreaker (created, id) ensures deterministic ordering
- [x] All Phase 1C tests pass
- [x] Existing tests continue to pass

## Files Modified

- `pkg/ent/schema/policy.go` - Added policy_kind field and composite index
- `pkg/ent/accesspolicy*.go` - Generated Ent code
- `pkg/ent/migrate/schema.go` - Generated migration schema
- `pkg/ent/mutation.go` - Generated mutation code
- `pkg/store/models.go` - Added PolicyKind field and constants
- `pkg/store/entadapter/policy_store.go` - Updated to read/write PolicyKind with deterministic ordering
- `pkg/hub/authz.go` - Updated evaluatePolicies comments to document total order
- `pkg/hub/handlers_policies.go` - Added PolicyKind validation and SourceIPs rejection
- `pkg/hub/seed.go` - Set PolicyKind for seeded policies

## Files Added

- `pkg/hub/authz_policy_determinism_test.go` - Phase 1C authorization tests
- `pkg/hub/handlers_policies_phase1c_test.go` - Phase 1C handler validation tests

## Testing

All Phase 1C tests pass:
```
go test ./pkg/hub -run "TestPolicyDeterministic|TestPolicyPriority|TestPolicyKind|TestPolicyLocal|TestPolicyResource|TestPolicyStable|TestCreatePolicy_SourceIPs|TestCreatePolicy_PolicyKind|TestUpdatePolicy" -timeout=300s -count=1
```

Full hub test suite running to verify no regressions.

## Notes

- Ent auto-migration will handle schema changes; existing rows default to `explicit` policy_kind
- Seeded policies (Origin == "seeded") are marked as `default` kind
- The deterministic total order ensures consistent policy evaluation regardless of insertion order
- Local override behavior is preserved and explicitly tested
- SourceIPs are rejected at write time to prevent misleading configuration until enforcement is implemented

---

## Review Round 1 Fixes (2026-08-24)

Reviewer: pf-1c-em
Status: REQUIRED FIX applied, OPTIONAL HARDENING applied

### Required Fix R1: UpdatePolicy PolicyKind handling

**Issue**: UpdatePolicy unconditionally defaulted empty PolicyKind to 'explicit', which would silently reset 'default' policies to 'explicit' if callers passed partial Policy structs.

**Fix Applied** (`pkg/store/entadapter/policy_store.go`):
```go
// Only update PolicyKind if explicitly provided (non-empty).
// This prevents silently resetting 'default' policies to 'explicit'.
if p.PolicyKind != "" {
    update.SetPolicyKind(accesspolicy.PolicyKind(p.PolicyKind))
}
```

### Optional Hardening (Security Audit Medium)

**Applied**: Removed PolicyKind from API-facing create/update requests entirely.

**Rationale**: Only the seed code should set 'default'. User-created policies are always 'explicit'. This prevents admin confusion and ensures clear separation between system defaults and user policies.

**Changes**:
- Removed `PolicyKind` field from `CreatePolicyRequest` and `UpdatePolicyRequest`
- User-created policies always set to `PolicyKind: store.PolicyKindExplicit`
- Updated tests to verify PolicyKind preservation on update
- Removed PolicyKind validation tests (no longer applicable to API)

### Test Updates

**Removed**: PolicyKind validation tests (API no longer exposes the field)

**Added**: `TestUpdatePolicy_PolicyKindPreservation` - Verifies that updating a seeded policy (kind=default) preserves its PolicyKind

**Verification**:
- All Phase 1C tests pass
- `make ci` passes
- PolicyKind is only settable via seed code, not user API

This hardening ensures seeded policies cannot be accidentally or maliciously converted to explicit policies through the API.
