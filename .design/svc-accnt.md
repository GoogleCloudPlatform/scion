# Design: Service Account Rework — IAM-gated assignment & hub-level SAs

**Status:** Approved for implementation. All design questions resolved.
**Date:** 2026-07-28 (original); updated 2026-08-03 for PT v3 and settled rulings.
**Branch:** `scion/svc-accnt-lead` @ `03511aef`
**Original (full provenance):** `origin/scion/sa-arch:.design/svc-accnt.md` @ `d933bc41`
**Delegation artifact:** `design-em-delegation.md` (implementation phases and dependency graph)
**Related:** `ptone/scion` #591, #595-#600 (authorization track)

This document is the authoritative design for the service-account rework on the working
branch. It incorporates all rulings from ptone (2026-07-28 and 2026-08-03), the P2 Policy
Troubleshooter design, and the D1-D7 settled decisions from the engineering-manager
delegation document. The original design on `origin/scion/sa-arch` is preserved for
detailed provenance and correction history.

> **Which tree this document describes.**
> Every line number and source quotation is relative to `origin/scion/svc-accnt-lead`.
> As of `origin/main`, main contains neither #591 nor #595. Both fixes exist only on
> the integration branch. #595 is a hard merge predecessor for this project.

---

## 1. Problem & Goals

### Problem

Service-account assignment and metadata passthrough give Scion agents real GCP authority.
The current and in-flight design has three classes of remaining risk:

1. **Passthrough identity is under-gated.** `PATCH /api/v1/agents/{id}` can switch an
   agent to `passthrough` without the create-path broker-owner/admin restriction, and no
   passthrough path has an `actAs` check for the broker host service account.
2. **Hub-scoped service accounts need a sharper policy model.** #19 Option B says a hub
   member who passes `actAs` may assign hub-scoped SAs, but Q1 says the IAM check is off by
   default. Combined naively, that makes hub-scoped assignment available to any hub member
   when `gcpIamCheckMode=off`.
3. **Minting creates GCP authority without enough checks.** The mint path does not verify
   the requester can create SAs or use Vertex/agent-platform authority, does not grant the
   requester `actAs` on the minted SA, and can record a minted SA as verified after a failed
   IAM mutation.

### Goals

- Require a two-layer gate for every explicit SA assignment:
  - Scion Hub policy layer (`ActionAssign`);
  - GCP IAM layer (`iam.serviceAccounts.actAs`) via Policy Troubleshooter.
- Preserve the settled P2 decision: Policy Troubleshooter is the single checker mechanism
  for all caller types (human and agent), `UNKNOWN*` fails closed, and there is no
  `getIamPolicy` fallback.
- Close passthrough PATCH parity immediately and design passthrough `actAs` as its own phase.
- Resolve #19 Option B so hub-scoped SAs cannot become assignable to every hub member when
  IAM enforcement is off.
- Make minting a permission-checked, auditable flow whose IAM mutations are not swallowed.

### Success Criteria

- No assignment or passthrough surface can be reached without the intended gate.
- Hub-scoped assignment has explicit semantics for `gcpIamCheckMode=off`.
- Minted SAs are either fully usable by design or visibly failed/unverified.
- Cache invalidation is wired for Hub-initiated IAM changes and SA deletion.

---

## 2. Non-Goals

- No continuous revalidation of already-running agents after `actAs` is revoked. The gate
  is admission-time only.
- No replacement of Policy Troubleshooter with impersonated `testIamPermissions`.
- No `getIamPolicy` fallback.
- No broad redesign of the policy engine. Ptone accepted the narrow code-baseline approach
  for hub-scoped assignment.
- No public disclosure of mechanism details for ptone/scion#51 in this document.
- No attempt to solve GitHub-auth-to-GCP-principal mapping.

---

## 3. Settled Decisions (D1-D7)

These decisions are the load-bearing commitments from ptone's rulings on 2026-07-28 and
2026-08-03. Full reasoning is in `design-em-delegation.md` section 3 and `decisions-log.md`.

### D1 -- `actAs` Is the GCP Permission

The caller may assign target SA `Y` only if the caller's GCP principal has
`iam.serviceAccounts.actAs` on `Y`. This is the GCP-native expression of "same or lesser";
Scion does not compute effective privilege subsets.

### D2 -- Policy Troubleshooter Is the Only GCP Checker

Use Policy Troubleshooter v3 for **all** caller types (human and agent). Map the
`AccessState` enum:

| PT v3 `AccessState` | Meaning | Checker `ActAsOutcome` |
|---|---|---|
| `ACCESS_STATE_GRANTED` | Caller has the permission; no deny overrides | `ActAsAllowed` |
| `ACCESS_STATE_NOT_GRANTED` | No allow binding grants the permission | `ActAsDenied` |
| `ACCESS_STATE_UNKNOWN_INFO_DENIED` | Allow exists but deny/PAB overrides; partial resolution | `ActAsDenied` |
| `ACCESS_STATE_UNKNOWN_CONDITIONAL` | Access depends on a runtime condition PT cannot evaluate statically | `ActAsIndeterminate` |
| Unrecognised / zero value / transport error | Future enum value, response corruption, or gRPC failure | `ActAsIndeterminate` |

**There is no fallback to `getIamPolicy`.** PT answers, or the result is indeterminate,
which is denied at the gate.

**This supersedes** the original design's recommendation of option (e) (impersonated
`testIamPermissions`) for agent callers. The ruling chose uniformity: one mechanism, one
set of failure modes, one set of permissions to document.

**The Hub's `tokenCreator` grant** enables impersonated probes and is what makes
`VerifyImpersonation` work. It is NOT the permission being checked on the caller. The
permission checked is `iam.serviceAccounts.actAs` (`roles/iam.serviceAccountUser`).
These are two different service accounts, two different permissions, at two different
points:

| | Which SA | Which permission | Who holds it |
|---|---|---|---|
| Enables the probe | the **caller's** SA | `tokenCreator` | the **Hub** (already, verified) |
| Is being tested | the **target** SA | `actAs` | the **caller** |

### D3 -- Two Layers Are Both Required

Hub policy and GCP IAM answer different questions:

- Hub policy: "may this Scion caller assign this Scion resource?"
- GCP IAM: "may this GCP principal act as this GCP service account?"

Neither layer subsumes the other.

### D4 -- Hub-Scoped Assignment Requires Mode Coupling

**Ruled by ptone:** deny assignment of hub-scoped SAs whenever
`gcpIamCheckMode != enforce`.

This is assignment-time coupling, not registration-time coupling. Registration-time checks
are insufficient because the mode can be switched off later.

`gcpIamCheckMode=off` remains a transitional escape hatch for project-scoped assignment
while operators roll out PT prerequisites. The design must not depend on that escape hatch
existing forever; ptone noted it may be fully disabled in the future.

### D5 -- Hub-Scoped Policy Baseline Should Be Code, Not a Hub-Scoped Seed Policy

Do not seed a hub-scoped `assign` policy for all hub members. The current resource matcher
has no hub-scope resource arm, and a naive hub-scoped policy can over-match.

**Ruled by ptone:** add a narrow code baseline for current hub members assigning hub-scoped
SAs, gated by `ActionAssign`, service-account resource type, hub-scoped SA, and the D4
mode coupling.

### D6 -- Mint Permission Checks Are Independent of `gcpIamCheckMode`

**Ruled by ptone:** minting checks run even when SA-assignment IAM enforcement is off.

Reason: mint creates new GCP authority and project IAM bindings. It is not merely
assignment. Letting `gcpIamCheckMode=off` skip mint checks creates a separate
privilege-creation bypass.

### D7 -- Hub-Scoped Creation Is Split Between Mint and BYO Registration

**Ruled by ptone:** Option B applies to assignment, not to minting hub-scoped SAs.
Hub-scoped minting remains admin-gated. Non-admin users may BYO/register a
service-account resource by providing the email of an SA they own/control, subject to the
normal verification, Hub policy, and `actAs` gates before it can be assigned.

The OwnerID lever must not let a former hub member assign a hub-scoped SA solely because
they created or registered it. Current hub membership is required for the Option B assign
baseline.

---

## 4. Threat Model

The asset is the **authority a service account carries in the customer's GCP
organisation** -- not the SA record in the Hub database. An SA binding is a grant of real
cloud privilege.

**Primary threat -- lateral privilege escalation via agent creation.** An agent holding a
low-privilege SA creates a child agent bound to a high-privilege SA, then uses the child
to act with privileges its own principal was never granted. Today this is unchecked: the
only guard is user-only and agents skip it.

**Secondary threat -- assignment surfaces other than create.** There are four distinct
paths that bind an SA to an agent (section 6). Hardening only `POST /api/v1/agents` leaves
three open.

**The gate is admission-time, not continuous.** The check runs at assignment time.
Revoking `actAs` afterwards does not unassign the SA, does not stop the running agent, and
does not invalidate the JWT already issued. Revocation stops the next assignment, not the
current one.

---

## 5. Principal Model -- Whose Permission Is Checked

**RESOLVED (Q11).** The check evaluates exactly **one** principal: the immediate caller.

| Caller kind | Principal evaluated |
|---|---|
| Agent | The calling agent's own SA (`AppliedConfig.GCPIdentity.ServiceAccountEmail`) |
| Human (Google OAuth) | The user's Google account principal (`user:<email>`) |
| Human (GitHub OAuth) | **No GCP principal** -- denied. Hub should leave toggle off |
| Broker / unknown | Denied -- fail closed |

**No ancestry walk.** `Ancestry[0]` / `OriginUserID()` is not consulted. Checking the
originating human would be weaker: an agent started by an admin but holding a low-privilege
SA could pass on the admin's authority.

**Same-SA propagation is auto-allowed.** An agent holding SA X binding SA X to its child
grants no new privilege -- short-circuit before any IAM call.

**A `block`-mode agent cannot assign any SA.** It has no GCP principal, so there is
nothing to evaluate. Fail closed.

**Project-default assignment** checks the agent creator. Human creator for human-created
agents; creating agent's assigned SA for agent-creates-agent. This was ruled by ptone
(see D1-D7; also `design-em-delegation.md` section 4.5).

---

## 6. The Four Assignment Surfaces

All four must reach the same decision function.

| # | Surface | Plan |
|---|---|---|
| a | `POST /api/v1/agents`, explicit `gcp_identity` | Full gate |
| b | Project-default annotation | Gate required by Q4; checks the agent creator |
| c | `PATCH /api/v1/agents/{id}` | Full gate + validation parity |
| d | Lifecycle hook `execution_identity` | Full gate |

Surface (b) is the **dominant** real-world path because `hubclient.CreateAgentRequest` has
no `gcp_identity` field.

The gate belongs at the shared chokepoint (`createAgentInProject`), not on each route.

---

## 7. The Permission Check -- Interface (Frozen)

```go
// pkg/store
type ActAsOutcome int

const (
    ActAsIndeterminate ActAsOutcome = iota  // zero value = fail closed
    ActAsAllowed
    ActAsDenied
)

type ActAsResult struct {
    Outcome   ActAsOutcome
    Mechanism string
    Reason    string
}

type CallerPermissionChecker interface {
    CanActAs(ctx context.Context, caller Principal, targetSA *store.GCPServiceAccount) (ActAsResult, error)
}
```

Three load-bearing properties:

1. **Three-valued, not a bool.** `ActAsIndeterminate` is the zero value. An unpopulated
   result fails closed by construction.
2. **`error` is for programming/transport failures only.** Never for "denied" or "unknown".
3. **The Q1 toggle is evaluated by the caller, not inside the checker.** The checker is
   pure and testable.

### 7.1 Policy Troubleshooter Checker (P2, implemented)

`PolicyTroubleshooterChecker` in `pkg/hub/gcp_iam_pt.go` implements
`CallerPermissionChecker` using PT v3 (`cloud.google.com/go/policytroubleshooter/iam/apiv3`).

PT v3 `AccessState` mapping:

| PT v3 `AccessState` | Checker `ActAsOutcome` |
|---|---|
| `ACCESS_STATE_GRANTED` | `ActAsAllowed` |
| `ACCESS_STATE_NOT_GRANTED` | `ActAsDenied` |
| `ACCESS_STATE_UNKNOWN_INFO_DENIED` | `ActAsDenied` |
| `ACCESS_STATE_UNKNOWN_CONDITIONAL` | `ActAsIndeterminate` |
| Unrecognised / transport error | `ActAsIndeterminate` |

**No fallback mechanism.** PT answers, or the result is indeterminate, which is denied at
the gate. There is no `getIamPolicy` fallback path. The rationale (from P2 design):

- The `getIamPolicy` fallback fails open on IAM Deny/PAB and does so most often in
  exactly the orgs that deploy them.
- Q1's toggle (`gcpIamCheckMode: off`) is already the explicit, auditable escape hatch.
- `getIamPolicy` coverage is narrow: misses project-level grants, group grants,
  conditional grants, IAM Deny policies, and PAB policies.

### 7.2 Decision Cache (P2, implemented)

`CachedCallerPermissionChecker` in `pkg/hub/gcp_iam_cache.go` wraps the PT checker with
asymmetric TTLs:

- Allow TTL: 60s
- Deny TTL: 10s
- **Indeterminate and errors are NOT cached.** Indeterminate means the check could not
  reach an answer; caching "I don't know" turns a transient failure into a fixed-length
  outage.

Cache key: `(callerPrincipalID, targetSAEmail, permission)`.

`InvalidateForSA(saEmail)` removes entries for a specific SA (called on SA delete and
Hub-initiated IAM mutations). `InvalidateAll()` clears the full cache.

### 7.3 Group-Binding Limitation

`roles/iam.serviceAccountUser` is commonly granted to a Google Workspace **group**.
PT resolves group bindings only when the Hub SA has Workspace Admin `groups.read`
privilege (domain-wide delegation). Without it, PT returns a membership-unknown state,
which under fail-closed is a denial -- even for legitimately authorised users.

This is not a bug; it is a property of the ruling. Mitigation:

| Approach | Cost | Resolves groups? |
|---|---|---|
| Grant Hub SA domain-wide delegation + `groups.read` | High | Yes |
| Grant actAs directly to users, not via groups | Low | Avoided |
| Leave `gcpIamCheckMode: off` | Zero | N/A |

### 7.4 Configuration

```yaml
hub:
  # "off"     -- no check; any member who can see the SA can assign it (default)
  # "enforce" -- Policy Troubleshooter checks actAs; denials are enforced
  gcpIamCheckMode: "off"
```

Default is `off` per Q1. When set to `enforce`, the Hub SA must have
`roles/iam.securityReviewer` on the org or project containing any SA that will be assigned.

### 7.5 Required IAM Permissions for the Hub SA

| Role / Permission | Scope | Purpose | Required? |
|---|---|---|---|
| `roles/iam.securityReviewer` | Org or project containing target SA | Read allow policies and role definitions | **Yes** |
| `roles/iam.denyReviewer` | Org or folder | Read IAM Deny policies | Recommended |
| `roles/browser` | Org | Read project/folder hierarchy | Recommended |
| Workspace Admin `groups.read` | Google Workspace domain | Resolve group memberships | Only if group-granted actAs must be resolved |
| PT API enabled | Hub's GCP project | `policytroubleshooter.googleapis.com` | **Yes** |

---

## 8. Hub Policy Layer -- the `assign` action

Add `ActionAssign` to the `gcp_service_account` resource actions. Two layers, both
required:

1. **Hub policy layer** -- `CheckAccess(identity, gcpServiceAccountResource(sa), ActionAssign)`.
   Answers "may this principal use this SA record within Scion."
2. **GCP IAM layer** -- `CanActAs`. Answers "does this principal hold the delegation grant
   in GCP."

### 8.1 Prerequisite: `gcpServiceAccountResource()`

The resource function must branch on scope. Hub-scoped SAs produce a **parentless**
resource (no project parent), which is the input class #595 addresses. The conditional
pattern from `harnessConfigResource()` is the correct reference.

This is implemented on the working branch. `matchesResource` now rejects parentless
resources under project-scoped policies, providing structural confinement.

### 8.2 `CreatedBy` Is an Authorization Input

`gcpServiceAccountResource` sets `OwnerID: sa.CreatedBy` unconditionally, and the owner
bypass runs before resource-type logic. This means:

- `CreatedBy` is immutable only because `UpdateGCPServiceAccount` omits it (not by design).
- The owner bypass grants unconditional access regardless of hub membership.

**Required before Step 5:** assert immutability of `CreatedBy`, `Scope`, `ScopeID` at the
store boundary.

---

## 9. Goal 2 -- Hub-Scoped SAs (Q5: Option A)

**Ruled by ptone:** real hub-scoped SAs, pickable in any project, subject to the permission
check.

### 9.1 Hub-Scope Mode Coupling (D4)

Hub-scoped SA assignment is denied when `gcpIamCheckMode != enforce`. This is
assignment-time coupling. The precondition:

```go
if targetSA.Scope == store.ScopeHub && s.saAssignCheckMode != SAAssignCheckEnforce {
    deny("hub-scoped service-account assignment requires gcpIamCheckMode=enforce")
}
```

### 9.2 Hub-Policy Baseline (D5)

Current hub members may assign hub-scoped SAs when they pass `actAs`. Implemented in code
(not seed policy) with the following conditions:

- User is current hub member
- Action is `ActionAssign`
- Resource is a hub-scoped SA
- `gcpIamCheckMode == enforce`

### 9.3 Hub-Scoped Creation (D7)

- Hub-scoped **minting** remains admin-gated.
- Non-admin BYO registration allowed (SA email, subject to verification + assign gates).
- Former member creator denied -- current hub membership required.

---

## 10. Passthrough Identity

Passthrough exposes the broker host's GCP identity. Two independent checks:

1. **Broker-owner/admin restriction:** "may this caller use this broker in passthrough mode?"
2. **`actAs` check:** "may this caller act as the broker host service account?"

The second check requires broker host SA identity fields on the broker
registration/update surface:

- `gcp_host_service_account_email`
- `gcp_host_project_id`

If passthrough is requested and the broker host SA identity is absent, deny with a
configuration error.

The checker uses a transient `store.GCPServiceAccount` for the broker host SA rather than
widening `CallerPermissionChecker`.

---

## 11. Minting

The mint path must (per D6, checks run even when `gcpIamCheckMode=off`):

1. PT-check requester for `iam.serviceAccounts.create` on the Hub GCP project.
2. PT-check requester for `aiplatform.endpoints.predict` (representative Agent Platform
   User permission from `roles/aiplatform.user`).
3. Create the service account.
4. Grant the Hub SA `roles/iam.serviceAccountTokenCreator` on the minted SA.
5. Grant the requester `roles/iam.serviceAccountUser` on the minted SA.
6. Grant the minted SA project-level `roles/aiplatform.user`.
7. Store as verified only if all required IAM mutations succeeded.
8. Invalidate actAs cache for the minted SA after IAM changes.

**Mint failure semantics (P7C):** stop swallowing tokenCreator grant failure. Either fail
the HTTP request (preferred) or store as `VerificationStatus=failed`. Do not write
`Verified=true` unless required IAM mutations succeeded.

---

## 12. Audit

There is no audit event for SA assignment today. Add one, emitted on both allow and deny,
carrying:

- The principal evaluated and its kind
- The target SA (id + email)
- The permission checked and the mechanism used
- The decision, and whether it was served from cache
- The surface (create / patch / project-default / lifecycle-hook)

---

## 13. Security Track Relationship

Issue #591 documents a hub-wide authorization bypass. Goal 1's Hub-policy check must be
built on the shared fail-closed `authorize` helper from that track.

- **#595** (`matchesResource` defect) -- hard prerequisite for Goal 2.
- **#596** (passthrough + SA-assign gate alignment) -- collision with P4.

Track S lands first; Goal 1 rebases onto it and upgrades `ActionRead` to `ActionAssign`.

**Security constraint:** details of ptone/scion#51 remain pointer-only. See ptone/scion#51,
tracked separately on the security track.

---

## 14. Implementation Phases

See `design-em-delegation.md` for the full phase breakdown and dependency graph.
Summary:

| Phase | Description | Status |
|---|---|---|
| P7A | PATCH passthrough parity | Can start now |
| P7B | Cache invalidation wiring | Can start now |
| P7C | Mint failure semantics | Can start now |
| P8 | Passthrough `actAs` | After P7A |
| P9 | Hub-scoped assignment semantics | After ptone/scion#51 gate |
| P10 | Project-default assignment gate | Can start now |
| P11 | Mint permission and project role grants | After P7B/P7C |
| P12 | Documentation sweep | This phase |

---

## 15. Resolved Decisions

All design questions are now resolved. This table supersedes the original design's section
10.

| # | Question | Ruling |
|---|---|---|
| Q1 | Enforcement | Hub-level toggle, **OFF by default** |
| Q2 | Check mechanism (human path) | Policy Troubleshooter. No `getIamPolicy` fallback |
| Q3 | Permission checked | `iam.serviceAccounts.actAs` (`roles/iam.serviceAccountUser`) |
| Q4 | Surfaces covered | All four surfaces of section 6 |
| Q5 | Goal 2 scope | Option A -- real hub-scoped SAs, pickable in any project |
| Q6 | Caching | Asymmetric TTLs: allow 60s, deny 10s. Indeterminate not cached |
| Q7 | Failure mode | Fail closed on error |
| Q8 | Migration | Forward-looking only |
| Q9 | `verification_status` | Already shipped in P0.1 |
| Q10 | PATCH hole | Moved to security track (agent-id-fix) |
| Q11 | Principal checked | Immediate creator only. No ancestry walk |
| Q12 | Broker else-branch | Moved to security track |
| Q13 | Dev-auth bypass | None; toggle off by default makes dev unaffected |
| **Q14** | **Agent-caller mechanism** | **Policy Troubleshooter for ALL caller types.** Overrides option (e) / impersonated `testIamPermissions` recommendation |
| **Q15** | **UNKNOWN state handling** | **Fail closed.** `ActAsIndeterminate` = denial. Q1 toggle is the escape hatch |
| **Q16** | **Fallback behaviour** | **No `getIamPolicy` fallback.** PT only. Fallback fails open on IAM Deny; Q1 is already the explicit escape hatch |
| D1 | GCP permission | `iam.serviceAccounts.actAs` |
| D2 | Checker mechanism | PT v3, single mechanism for all callers |
| D3 | Layer independence | Hub policy and GCP IAM both required |
| D4 | Hub-scope mode coupling | Hub-scoped SA assignment denied when mode != enforce |
| D5 | Hub-policy baseline | Narrow code baseline, not seed policy |
| D6 | Mint independence | Mint checks run regardless of `gcpIamCheckMode` |
| D7 | Hub-scope creation | Mint admin-gated; BYO registration allowed; current membership required |
