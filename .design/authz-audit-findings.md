# Authorization Audit Findings Ledger — Phase 0 Baseline

**Status:** C0 baseline
**Branch:** `scion/authz-audit`
**Base:** `scion/policy-fix` @ `5dff865f`
**Date:** 2026-09-01

## Purpose

This ledger catalogs every known authorization defect discovered during QA, review,
and the post-merge investigation of the authorization foundation refactor. Each
finding is classified by security effect, severity, and disposition. The ledger is the
exit-gate artifact for Phase 0 (C0): every known critical/high defect must have
containment, an approved final fix, or an explicit documented risk decision before the
phase closes.

## Finding Classification

**Security effects:** grant-authority, change-authority, revoke-authority,
boundary-relaxation, cross-scope-disclosure, credential-access, external-effect,
missing-eligibility-gate, information-leak, denial-quality.

**Severity:** Critical, High, Medium, Low.

**Disposition:**
- `contained-c0` — temporary restrictive containment applied in C0.
- `fixed` — root cause corrected on the base branch.
- `deferred-phase-N` — deferred to a named phase with rationale.
- `accepted-risk` — explicit risk acceptance with justification.
- `not-applicable` — finding does not apply or was invalidated.

---

## Findings from QA Defect Investigation (qa-defect-investigation.md)

### F-QA-01: Mine filter includes all project-scoped bindings (RC-A related)

| Field | Value |
|---|---|
| **Source** | QA investigation §1, authz-audit-implementation-plan §Findings-1 |
| **Security effect** | cross-scope-disclosure, denial-quality |
| **Severity** | High |
| **Description** | `resolveUserRBProjectIDs` collects every project-scoped RoleBinding (owner/admin/member) without role filtering. A project-admin or project-member binding causes a project to appear in the "mine" scope, which should be reserved for projects the user owns. This misrepresents ownership and can affect product decisions about project governance. |
| **Current state** | The policy-fix branch added RoleBinding-based project ID resolution but did not filter by role name. |
| **C0 containment** | Change `resolveUserRBProjectIDs` to accept a role filter parameter. For "mine", filter to `project-owner` only. For "shared", include all project-scoped bindings excluding those already in mine. |
| **Contract decision to relax** | Phase 1 must define the exact semantics of Mine/Shared and whether admin bindings contribute to either. |
| **RS2 resolution** | D6 Mine/Shared semantics implemented in RS2. Mine = active direct project-owner RoleBinding (via `MemberOrOwnerProjectIDs` with owner-only filter). Shared = effective access minus Mine. Both project and agent list handlers use scope-pushed authorization with correct classification. Agent creator/OwnerID does not expand Mine. Tests: `TestRS2_ProjectListMineSharedClassification`, `TestRS2_AgentListMineSharedClassification`. |
| **Disposition** | `resolved-rs2` |

### F-QA-02: Project admin can manage membership mutations

| Field | Value |
|---|---|
| **Source** | QA investigation §2, authz-audit-implementation-plan §Findings-2,3,4 |
| **Security effect** | grant-authority, change-authority, revoke-authority |
| **Severity** | Critical |
| **Description** | `canDelegateProjectMembership` uses `isProjectOwnerOrAdmin`, allowing project admins to add/remove/change members. While the `GrantTypeRoleBinding` escalation check prevents admin→owner promotion (admin lacks `*.delete` perms), an admin CAN add another admin (since admin holds all admin permissions). The C0 exit gate requires: "A project admin cannot add another admin, promote an owner, demote/remove an admin, or remove an owner through any project-membership API path." |
| **Current state** | Admin passes `GrantTypeProjectMembership` check and can add project-member or project-admin roles. |
| **C0 containment** | Create `isProjectOwner` function (owner-only, not admin). Use it in place of `isProjectOwnerOrAdmin` for project membership CanDelegate checks. This temporarily restricts all membership mutations to project owners only. |
| **Contract decision to relax** | Phase 1 must define the governance matrix: which actor roles can manage which target roles. |
| **Disposition** | `contained-c0` |

### F-QA-03: CanDelegate denial leaks internal permission names

| Field | Value |
|---|---|
| **Source** | authz-audit-implementation-plan §Findings-5, QA investigation §2.2 |
| **Security effect** | information-leak, denial-quality |
| **Severity** | Medium |
| **Description** | When CanDelegate rejects an operation, `writeForbidden` passes the raw `decision.Reason` string to the HTTP response, which contains internal permission names (e.g., "actor does not hold permission: agent.delete"). These should be stable product-level codes, not raw evaluator output. |
| **Current state** | Raw reasons from `actorHoldsAllPermissions` are exposed in 403 responses. |
| **C0 containment** | Add stable error codes `ROLE_ASSIGNMENT_FORBIDDEN` and `TARGET_ROLE_PROTECTED` for membership governance denials. Log the raw reason internally via structured logging but return only the stable code to the client. |
| **Contract decision to relax** | Phase 1 defines the complete stable denial code vocabulary. |
| **Disposition** | `contained-c0` |

### F-QA-04: Generic Forbidden response lacks actionable context

| Field | Value |
|---|---|
| **Source** | QA investigation §2.2, policy-fix-review R1 |
| **Security effect** | denial-quality |
| **Severity** | Low |
| **Description** | The `Forbidden()` helper returns a generic "Insufficient permissions" with code `forbidden`. For membership operations, this provides no guidance to the caller about what is actually required. |
| **Current state** | Some paths use `writeForbidden(w, msg)` with a message, others use `Forbidden(w)`. |
| **C0 containment** | Membership denial paths now use specific codes. Generic Forbidden paths remain unchanged. |
| **Contract decision to relax** | Phase 1 defines stable denial codes per operation domain. |
| **Disposition** | `contained-c0` |

## Findings from Review Rounds

### F-REV-01: PATCH role change order (R1 finding, fixed in R2)

| Field | Value |
|---|---|
| **Source** | policy-fix-review R1 §R1 |
| **Security effect** | revoke-authority (data loss) |
| **Severity** | High |
| **Description** | Original PATCH handler did delete-then-create, risking permanent membership loss if create fails. |
| **Current state** | Fixed on base branch. Handler now does create-then-delete. |
| **Disposition** | `fixed` |

### F-REV-02: ErrDirectUserOnly returns 500 (R1 finding, fixed in R2)

| Field | Value |
|---|---|
| **Source** | policy-fix-review R1 §R2 |
| **Security effect** | denial-quality |
| **Severity** | Medium |
| **Description** | Assigning project-owner to a group principal returned 500 instead of 400. |
| **Current state** | Fixed on base branch. Handler-level validation returns 400 with clear message. |
| **Disposition** | `fixed` |

### F-REV-03: JWT suspension check fails open on store errors (R4 finding, fixed in R5)

| Field | Value |
|---|---|
| **Source** | policy-fix-review R4 §R1 |
| **Security effect** | missing-eligibility-gate |
| **Severity** | High |
| **Description** | JWT path silently continued on store errors, admitting suspended users. |
| **Current state** | Fixed on base branch (commit 5dff865). Returns 503 on non-ErrNotFound store errors. |
| **Disposition** | `fixed` |

### F-REV-04: Missing regression test for 503 store-error path (R5 finding)

| Field | Value |
|---|---|
| **Source** | policy-fix-review R5 §O1 |
| **Security effect** | missing-eligibility-gate (test gap) |
| **Severity** | Low |
| **Description** | No dedicated test for the new 503 branch in JWT auth middleware. |
| **Current state** | Test added in CT1: `TestJWTAuth_StoreError_Returns503` in `pkg/hub/auth_jwt_suspension_test.go`. Covers store error (503), suspended user (403), active user (pass-through), deleted user (ErrNotFound pass-through), and nil UserStore (pass-through). |
| **Disposition** | `fixed` — regression test added in CT1. |

## Findings from Authz-Audit Implementation Plan

### F-PLAN-01: Shared scope uses legacy OwnerID exclusion

| Field | Value |
|---|---|
| **Source** | authz-audit-implementation-plan §Phase-0, code audit |
| **Security effect** | cross-scope-disclosure |
| **Severity** | Medium |
| **Description** | The "shared" scope filter excludes projects using the legacy `Project.OwnerID` field, not the RoleBinding-based owner status. If ownership were ever transferred or if OwnerID diverges from the project-owner binding, shared could show owned projects or hide shared ones. |
| **Current state** | `ExcludeOwnerID` uses the legacy field. |
| **C0 containment** | For "shared", use the new owner-binding-filtered project IDs from the mine resolver and exclude those from the full membership set. This decouples shared from the legacy OwnerID field for the exclusion. |
| **Contract decision to relax** | Phase 1 defines Mine/Shared semantics definitively. |
| **RS2 resolution** | `ExcludeOwnerID` removed from both project and agent list handlers. Shared is now computed as effective-access-minus-Mine using RoleBinding-based sets only. `Project.OwnerID` is treated as metadata with no authorization or classification role. The list handlers no longer reference `ExcludeOwnerID` at all. Tests: `TestRS2_ProjectListMineSharedClassification`, `TestRS2_AgentListMineSharedClassification`. |
| **Disposition** | `resolved-rs2` |

### F-PLAN-02: Project admin can add another admin

| Field | Value |
|---|---|
| **Source** | authz-audit-implementation-plan §Findings-3, §Motivating-Case-Study |
| **Security effect** | grant-authority |
| **Severity** | Critical |
| **Description** | A project-admin can add another project-admin because the CanDelegate escalation check only verifies permission-subset coverage (admin holds all admin permissions). The governance matrix says only owners should be able to add/remove/promote/demote admins. |
| **Current state** | No target-role governance beyond CanDelegate permission-subset. |
| **C0 containment** | Covered by F-QA-02: restrict all membership mutations to project-owner only. |
| **Contract decision to relax** | Phase 1 governance matrix. |
| **Disposition** | `contained-c0` (same fix as F-QA-02) |

### F-PLAN-03: Revocation under-modeled — admin can remove an owner

| Field | Value |
|---|---|
| **Source** | authz-audit-implementation-plan §Findings-4 |
| **Security effect** | revoke-authority |
| **Severity** | Critical |
| **Description** | The DELETE handler's CanDelegate check uses `isProjectOwnerOrAdmin`, so an admin could remove a non-last owner's binding. Only the last-owner guard prevents total orphaning. |
| **Current state** | Admin can remove an owner (as long as at least one owner remains). |
| **C0 containment** | Covered by F-QA-02: restrict membership DELETE to project-owner only. |
| **Contract decision to relax** | Phase 1 governance matrix. |
| **Disposition** | `contained-c0` (same fix as F-QA-02) |

### F-PLAN-04: Group eligibility for project-admin diverges from frozen design

| Field | Value |
|---|---|
| **Source** | authz-audit-implementation-plan §Findings-7 |
| **Security effect** | grant-authority |
| **Severity** | Medium |
| **Description** | The frozen design permits group principals to receive up to `project-admin`, but the current `directUserOnlyProjectRoles` map blocks both `project-owner` and `project-admin` for groups. |
| **Current state** | Both owner and admin are direct-user-only in the handler. |
| **C0 containment** | Keep the current restriction (admin is direct-user-only). This is more restrictive than the final design but safe as temporary containment. |
| **Contract decision to relax** | Phase 1 must reconcile group admin eligibility with the frozen design. |
| **Disposition** | `contained-c0` (intentionally over-restrictive) |

### F-PLAN-05: Role transitions not transactionally atomic

| Field | Value |
|---|---|
| **Source** | authz-audit-implementation-plan §Findings-8 |
| **Security effect** | change-authority |
| **Severity** | Medium |
| **Description** | The PATCH handler's create-then-delete sequence avoids a missing-binding window but may leave both bindings active if deletion fails. |
| **Current state** | Fixed to create-then-delete order. Duplicate-binding window is harmless under union model. Failed delete is logged and the old binding remains (strictly additive). |
| **C0 containment** | Not addressed in C0. The brief duplicate window grants the superset of both roles' permissions, which does not amplify beyond pre-operation authority. However, a failed delete during demotion leaves the target's broader authority active indefinitely, failing the requested revocation. |
| **Residual risk** | A failed delete during role demotion silently preserves the old, broader binding. The target retains their original authority until manual intervention. This does not grant new authority but does defeat the intent of the demotion. |
| **Contract decision to relax** | Phase 3 (reference slice RS1) will implement atomic role transitions. |
| **Disposition** | `deferred-phase-3` |

### F-PLAN-06: S1 cross-scope disclosure via role-bindings admin endpoint

| Field | Value |
|---|---|
| **Source** | QA investigation §5 |
| **Security effect** | cross-scope-disclosure |
| **Severity** | High |
| **Description** | hub-member and hub-viewer had `role_binding.read`, allowing any authenticated user to enumerate all RoleBindings hub-wide. |
| **Current state** | Fixed on base branch. `role_binding.read` removed from hub-member and hub-viewer, revision bumped to 2. |
| **Disposition** | `fixed` |

### F-PLAN-07: RC-C duplicate toast/inline notification (D6)

| Field | Value |
|---|---|
| **Source** | QA investigation §3 |
| **Security effect** | denial-quality (UX) |
| **Severity** | Low |
| **Description** | 403 errors trigger both a global toast and an inline alert simultaneously. |
| **Current state** | Fixed on base branch. `suppressAccessDeniedToast` option added to `apiFetch`. |
| **Disposition** | `fixed` |

---

## Phase 2 (AF1) Baseline Mismatches

The following mismatches were discovered during AF1 audit foundation cataloging.
Each is documented here rather than silently grandfathered.

### F-AF1-01: Route metadata table does not use method-specific patterns

| Field | Value |
|---|---|
| **Source** | AF1 entry-point inventory |
| **Security effect** | missing-eligibility-gate (classification gap) |
| **Severity** | Medium |
| **Description** | Most routes in `routeMetadataTable` use path-only patterns (e.g., `/api/v1/agents/`) rather than method-specific patterns (e.g., `GET /api/v1/agents/`, `DELETE /api/v1/agents/`). A single route entry covers both reads and writes, preventing method-specific authorization at the route guard level. The catalog uses method-specific entry point patterns where the security effect differs by method. |
| **Current state** | Route metadata covers all registered mux patterns but lumps GET/POST/PATCH/DELETE under one classification. Handler-level authorization is the defense-in-depth layer. |
| **Disposition** | `baseline-af1` — catalog documents the gap; method-specific route metadata conversion is a Phase 3/AH5 deliverable. |

### F-AF1-02: Permission coverage — 78 consumed, 41 reserved/deferred

| Field | Value |
|---|---|
| **Source** | AF1 permission coverage validation |
| **Security effect** | audit-gap |
| **Severity** | Low |
| **Description** | The initial AF1 catalog discovery found 22 permissions consumed and 97 reserved/deferred out of 119 registered. Subsequent AF1 catalog expansion (89 operations) raised consumption to 78/119 consumed by catalog operations, with 41 remaining as reserved/deferred with explicit rationale per tranche. The deferred permissions fall into: route-guarded CRUD (AH2-AH5), Phase 2 D4 hub-admin conversion, agent token scopes (non-route-enforced), and deprecated policy permissions (CO1 410 Gone). |
| **Current state** | `TestRegisteredPermissionsConsumed` enforces 78/119 consumed, 41 reserved/deferred. Every registered permission is accounted for — either consumed by a catalog operation or documented as reserved/deferred with a target tranche. CI gate prevents regression. |
| **Disposition** | `baseline-af1` — tracked for resolution in AH1-AH5 tranches. |

### F-AF1-03: Security mutation call sites — 182/182 bidirectional classification enforced

| Field | Value |
|---|---|
| **Source** | AF1 security mutation scan |
| **Security effect** | audit-gap |
| **Severity** | Medium |
| **Description** | AST-based scan initially found 53 security-relevant mutation call sites. Subsequent catalog expansion and PR #1445 governance additions raised the total to 182 discovered call sites across `pkg/hub/` and `pkg/store/`. Each site is classified as either mapped to a catalog operation or explicitly exempted (store-layer internal, migration, test fixture, governance compensating action). |
| **Current state** | `TestMutationClassificationBidirectional` enforces 182/182 bidirectional agreement: every scanner-discovered site has a classification entry, and every classification entry is still discoverable by the scanner. CI gate prevents regression in either direction (unclassified sites or stale classifications). |
| **Disposition** | `resolved-af1` — bidirectional classification complete and CI-enforced. |

---

## Phase 3 RS3 Findings — Project Deletion

### F-RS3-01: deleteProject handler lacks domain-service governance

| Field | Value |
|---|---|
| **Source** | RS3 implementation audit |
| **Security effect** | delete-resource, emit-external-effect |
| **Severity** | High |
| **Description** | The `deleteProject` handler used only `authorize()` (base permission check via ActionDelete) with no ownership/ancestry governance, no actor status check, no credential ceiling, no atomic audit record, and no transactional cascade. All cascading deletes were best-effort outside any transaction, allowing partial security state on failure. |
| **RS3 resolution** | Introduced `ProjectDeletionService` as a bounded domain service. Handler delegates to service for fail-closed authorization composition (base permission + ownership governance + actor status + credential ceiling), transactional cascade of security-relevant state (role bindings, groups, secrets, env vars, skill injections, GCP SAs, templates, harness configs), atomic project row deletion, and atomic `MutationAuditRecord` within a single `WithTx` block. External effects (broker dispatch, storage file cleanup, filesystem cleanup, quota release, event emission) have explicit ordering, fire-and-forget delivery, and log-and-continue failure mode. Mutation classifications updated from handler-level exemptions to service-level operation mappings. |
| **Disposition** | `resolved-rs3` |

### F-RS3-02: Missing mutation audit record for project deletion

| Field | Value |
|---|---|
| **Source** | RS3 implementation audit |
| **Security effect** | audit-gap |
| **Severity** | High |
| **Description** | The `deleteProject` handler published a `ProjectDeleted` event but did not create a `MutationAuditRecord`. The catalog declared `project.lifecycle.delete` with `Atomic: true` audit, but no implementation existed. |
| **RS3 resolution** | `ProjectDeletionService.Delete` creates a `MutationAuditRecord` with `MutationType: "project_delete"`, before fields (project_id, project_name, project_slug, owner_id), and actor identity/credential context, written atomically within the same transaction as the project row deletion and security state cascades. |
| **Disposition** | `resolved-rs3` |

### F-RS3-03: Mutation classification incorrectly describes permission

| Field | Value |
|---|---|
| **Source** | RS3 implementation audit |
| **Security effect** | documentation-gap |
| **Severity** | Low |
| **Description** | The mutation classification for `deleteProject`'s `DeleteProject` call described it as "route-guarded by project.update permission" when the handler actually checks `ActionDelete` (project.delete permission). |
| **RS3 resolution** | Classification replaced with operation-mapped entry: `{Function: "Delete", Symbol: "DeleteProject", OperationID: "project.lifecycle.delete"}` in the service. |
| **Disposition** | `resolved-rs3` |

### F-RS3-04: Non-HTTP DeleteProject call sites not classified for governance bypass

| Field | Value |
|---|---|
| **Source** | RS3 implementation audit |
| **Security effect** | classification-gap |
| **Severity** | Medium |
| **Description** | `DeleteProject` is called from 4 rollback/compensation sites (createProject x3, handleProjectRegister x1, handleProjectClone x1). These are route-guarded exemptions that bypass end-user governance because they are rollback compensation for failed project creation — the project never became visible to other users and has no established security state. |
| **RS3 resolution** | All rollback sites retain their existing `ExemptionRouteGuarded` classification with explicit comments. The RS3 test suite includes an AST-based proof test (`TestRS3_DeleteProjectCallSiteClassification`) that enumerates all non-test `DeleteProject` call sites and asserts each is classified in the mutation classifications map. New unclassified call sites will fail CI. |
| **Disposition** | `resolved-rs3` |

---

## Summary

| Severity | Total | Fixed | Contained-C0 | Deferred | Baseline-AF1 | Resolved-AF1 | Accepted Risk |
|---|---|---|---|---|---|---|---|
| Critical | 3 | 0 | 3 | 0 | 0 | 0 | 0 |
| High | 4 | 3 | 1 | 0 | 0 | 0 | 0 |
| Medium | 7 | 1 | 3 | 1 | 1 | 1 | 0 |
| Low | 4 | 2 | 1 | 0 | 1 | 0 | 0 |
| **Total** | **18** | **6** | **8** | **1** | **2** | **1** | **0** |

## C0 Containment Changes

The following containment changes are implemented on `scion/authz-audit`:

1. **Mine/Shared filter containment** (F-QA-01, F-PLAN-01): Mine selects only active
   direct `project-owner` RoleBindings. Shared includes all effective project access
   excluding Mine.

2. **Membership mutation owner-only gate** (F-QA-02, F-PLAN-02, F-PLAN-03): POST,
   PATCH, and DELETE on project members temporarily require an active direct
   `project-owner` binding. This is enforced via a new `isProjectOwner` check replacing
   `isProjectOwnerOrAdmin` in the `canDelegateProjectMembership` path.

3. **Stable public denial codes** (F-QA-03, F-QA-04): Membership governance denials
   return stable codes (`role_assignment_forbidden`, `target_role_protected`,
   `last_owner`) instead of raw evaluator output. Internal provenance is logged.

4. **Group admin restriction preserved** (F-PLAN-04): `directUserOnlyProjectRoles`
   keeps both `project-owner` and `project-admin` as user-only, more restrictive than
   the final design.

### Temporary restriction markers

Each containment change is marked in code with a `// C0-CONTAINMENT:` comment
identifying the finding, the restriction, and the contract decision required to relax
it. These markers are searchable and must not be removed without the corresponding
Phase 1 contract decision and regression tests.

---

## RS6 Preflight — External-Effect Containment Findings

Discovered by `agent:aa-rs6-preflight` and contained by `agent:aa-msg-containment-dev`.
Preflight report: `/scion-volumes/scratchpad/projects/policy-refactor/aa-rs6-preflight-report.md`.

### Contained findings

| ID | Severity | Title | Status |
|---|---|---|---|
| F-RS6-01 | **High** | Scheduled `message` events bypass `authorizeAgentMessage` entirely and permit cross-project targeting (`handlers_scheduled_events.go`, `server.go:messageEventHandler`) | **contained** — C1: authoring-time project-scope validation and fire-time `authorizeScheduledMessageFire` gate in `authorize_scheduled_message.go` |
| F-RS6-03 | **High** | Broker inbound synthesizes an `AuthenticatedUser` from payload-supplied email with no status check (`handlers_broker_inbound.go`) | **contained** — C2a: `Status != store.UserStatusActive` check before identity construction |
| F-RS6-27 | Low | `authorize_message.go` doc comment lists "scheduled events" as legitimate system-plane, which is the origin of F-RS6-01 | **contained** — comment corrected to state scheduled events are NOT system-plane |
| F-RS6-29 | **High** | `X-Scion-On-Behalf-Of` (`brokerauth.go:resolveOnBehalfOf`) suspension check uses string literal "suspended" instead of `store.UserStatusSuspended`; any non-active status other than "suspended" passes | **contained** — C2b: replaced with fail-closed `!= store.UserStatusActive` |
| F-RS6-19 | Low | `TestAuthorizeAgentMessage_IngressParity` asserts parity by comment; the claim is incomplete | **contained** — C3: `TestExternalEffectCallSiteClassification` is a bidirectional AST-based guard over all dispatch call sites |

### Open findings (deferred to RS6 or AH)

| ID | Severity | Title | Status |
|---|---|---|---|
| F-RS6-02 | **High** | No audit record is written for any message send, contradicting the declared `AuditObligation` (`catalog.go:911-917`) | open-rs6 |
| F-RS6-04 | Medium | `agent.message.send` declares an entry point that does not exist and omits 9 real ingresses | open-rs6 |
| F-RS6-05 | Medium | `agent.message.send` `ExternalEffectPolicy` contradicts runtime: `fire_and_forget`/"no retry" versus `dispatchWithBrokerRetry` | open-rs6 |
| F-RS6-06 | Medium | `AuthBeforeEmit: true` is false for `messagebroker.go`, `notifications.go`, `server.go` (pre-containment) | open-rs6 |
| F-RS6-07 | Medium | `MessageBrokerProxy.deliverToAgent` persists and emits with no authorization | open-rs6 |
| F-RS6-08 | Medium | Notification fan-out delivers into containers with subscription-only authorization; revocation never re-evaluated | open-rs6 |
| F-RS6-09 | Medium | Attachment bytes staged before authorization with no compensation | open-rs6 |
| F-RS6-10 | Medium | Broker inbound emits before commit | open-rs6 |
| F-RS6-11 | Medium | Scheduled dispatch orphans agent row when no dispatcher available | open-rs6 |
| F-RS6-12 | Medium | Successful scheduled dispatch writes no audit record | open-rs6 |
| F-RS6-13 | Medium | `authorizeScheduledAgentCreate` checks user creator status but not agent creator status | open-rs6 |
| F-RS6-14 | Medium | Recurring schedules bypass `ClaimScheduledEvent` | open-rs6 |
| F-RS6-16 | Medium | `handleAgentMessage` performs no messaging authorization; depends on its two routers | open-rs6 |
| F-RS6-17 | Low | No stable lower_snake_case denial vocabulary for messaging | open-rs6 |
| F-RS6-18 | Low | Message outbox is vestigial (`deliverMsg` seam never invoked) | open-rs6 |
| F-RS6-20 | Low | `TestCreateMessageEnumeration` skips subdirectories and enumerates persistence, not authorization | open-rs6 |
| F-RS6-30 | Medium | Deleted users retain working sessions (`auth.go` falls through on `ErrNotFound`) | open-rs6 |
| F-RS6-33 | **High** | Runtime-broker HMAC can assert any active user identity without user consent (arbitrary identity impersonation via `X-Scion-On-Behalf-Of`); see product decisions A-5, C2c | open-rs6 |
| F-RS6-36 | Low | `authzop.MutationClassifications` contains no messaging symbol | open-rs6 |

### RS4 Credential/Secret Reference Slice — Resolved Findings

| ID | Severity | Title | Resolution | Disposition |
|---|---|---|---|---|
| F-RS4-01 | **High** | UAT mint has no issuer ceiling: any valid scope can be minted regardless of the issuer's project authority | Issuer ceiling check added — `scopeToPermissionIDs` converts scope strings to permission IDs, then `getEffectivePermissions` verifies the actor holds each one. `ErrUATScopeViolation` sentinel returned. | `resolved-rs4` |
| F-RS4-02 | **High** | Non-member can probe project existence via distinct error responses on token mint (oracle) | `IsProjectMember` check added before permission resolution — non-members and nonexistent projects both receive `ErrUATProjectForbidden`. | `resolved-rs4` |
| F-RS4-03 | **High** | UATs can manage other UATs — all 5 token routes accept scoped tokens, contradicting frozen decision A1 | `requireSessionCredential` guard rejects `ScopedUserIdentity` and `AgentIdentity` on all 5 routes with 403 "forbidden". | `resolved-rs4` |
| F-RS4-04 | **High** | Token revoke/delete has no audit record — DELETE handler's fire-and-forget `emitMutationAudit` races with the response and can silently drop | Atomic audit via `store.WithTx`: mutation + audit record in a single transaction for mint, revoke, and delete. Fire-and-forget removed. | `resolved-rs4` |
| F-RS4-05 | Medium | Token create audit record has no `after_summary` — auditor cannot reconstruct what was minted | `afterSummary` populated with `token_id`, `scopes`, `project_id` inside the transaction. Plaintext key never recorded. | `resolved-rs4` |
| F-RS4-06 | Medium | Token cap check is not concurrency-safe — concurrent mints can exceed `UATMaxPerUser` | `LockUserForTokens` (SELECT FOR UPDATE on user row) serializes concurrent token transactions inside `WithTx`. | `resolved-rs4` |
| F-RS4-07 | Medium | Error mapping uses `strings.Contains` pattern matching on error messages | Typed sentinel errors (`ErrUATScopeViolation`, `ErrUATProjectForbidden`, etc.) with `errors.Is` checks replace string matching. | `resolved-rs4` |
| F-RS4-08 | Low | `GetUATService` is exported with no production callers | Removed entirely — no production callers, exported accessor would be a latent bypass door around the bounded service. | `resolved-rs4` |
| F-RS4-09 | Low | `TestMutationAudit_CredentialRevocation` is vacuous — skips on non-201, drives DELETE which emitted nothing, logs empty results | Deleted (R1/O2); superseded by `TestRS4_MutationAudit_CredentialRevocation_Asserting` which verifies exactly one `credential_revoke` audit record. | `resolved-rs4` |

### Open product decisions

| ID | Question | Recommendation |
|---|---|---|
| A-3 | What happens to pre-existing scheduled events when C1 lands? | Fail closed + one-time cancel-and-notify sweep |
| A-5 | Is broker identity synthesis an accepted design? Per-broker allow-list needed? | Land C2a/C2b now; treat per-broker allow-list as a separate item |
| C2c | Per-broker on-behalf-of allow-list | Deferred — requires product decision on bridge trust model |
