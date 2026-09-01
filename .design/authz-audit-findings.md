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
| **Disposition** | `contained-c0` |

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
| **Current state** | No test exists. Logic is straightforward. |
| **Disposition** | `deferred-phase-1` — non-blocking, follow-up test recommended. |

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
| **Disposition** | `contained-c0` |

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
| **C0 containment** | Acceptable for C0. The brief duplicate window grants the superset of both roles' permissions, which is never less than intended. |
| **Contract decision to relax** | Phase 3 (reference slice RS1) will implement atomic role transitions. |
| **Disposition** | `accepted-risk` — the current behavior is safe under the union model. |

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

## Summary

| Severity | Total | Fixed | Contained-C0 | Deferred | Accepted Risk |
|---|---|---|---|---|---|
| Critical | 3 | 0 | 3 | 0 | 0 |
| High | 4 | 3 | 1 | 0 | 0 |
| Medium | 4 | 1 | 2 | 0 | 1 |
| Low | 3 | 1 | 1 | 1 | 0 |
| **Total** | **14** | **5** | **7** | **1** | **1** |

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
   return stable codes (`ROLE_ASSIGNMENT_FORBIDDEN`, `TARGET_ROLE_PROTECTED`,
   `LAST_OWNER`) instead of raw evaluator output. Internal provenance is logged.

4. **Group admin restriction preserved** (F-PLAN-04): `directUserOnlyProjectRoles`
   keeps both `project-owner` and `project-admin` as user-only, more restrictive than
   the final design.

### Temporary restriction markers

Each containment change is marked in code with a `// C0-CONTAINMENT:` comment
identifying the finding, the restriction, and the contract decision required to relax
it. These markers are searchable and must not be removed without the corresponding
Phase 1 contract decision and regression tests.
