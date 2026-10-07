# Administrative User Provisioning API

**Status:** H.1 design approved ([ptone/scion#2133](https://github.com/ptone/scion/issues/2133), merged as GoogleCloudPlatform/scion#2064). H.2 ([ptone/scion#2134](https://github.com/ptone/scion/issues/2134)) is in implementation; its start gates are satisfied (§16.1): the D.2 shared admission mechanics it uses have merged (Phase 2, hub-token admission, is further gated on D.2 G5), and the sign-in item [GoogleCloudPlatform/scion#2071](https://github.com/GoogleCloudPlatform/scion/pull/2071) ("require provider-verified email and unify sign-in policy across auth paths") has landed. OD-1 (stored initial role) was **decided by ptone on 2026-09-28**: no stored role (§6, §19). OD-2 to OD-10 were **decided by ptone on 2026-10-04: option (a) for each** (§19, §20.2). H.2 PR-1 binds the A/D concepts to the merged code (§16.2, "Phase 0 binding").
**Tracker:** H, [ptone/scion#2116](https://github.com/ptone/scion/issues/2116) (ptone approved continuing it on 2026-09-28 as a separate followup; it is not a core prerequisite and does not gate core UAT delivery)
**Date:** 2026-09-28
**Anchored at:** `origin/main` @ `acc5a4b`
**Related contracts:** A.1 [ptone/scion#2117](https://github.com/ptone/scion/issues/2117), A.2 [ptone/scion#2118](https://github.com/ptone/scion/issues/2118), B.3 [ptone/scion#2121](https://github.com/ptone/scion/issues/2121), D.2 [ptone/scion#2124](https://github.com/ptone/scion/issues/2124), E.1 [ptone/scion#2126](https://github.com/ptone/scion/issues/2126), E.2 [ptone/scion#2127](https://github.com/ptone/scion/issues/2127)
**Vocabulary:** `authorization-operation-contract.md`, `authorization-operation-catalog.md`

All line numbers refer to `acc5a4b`. A.1, A.2, D.2 and E land in the same files first, so recheck
the anchors before H.2 starts.

The A.1 and A.2 contracts are still under design review. This document therefore describes admission
requirements by **concept**: credential boundary, target scope, active project access, frozen
permission ceiling, exact selector-to-permission mapping, and mint eligibility. Each concept carries
its issue reference. Binding these concepts to the final A type and function names is an explicit H.2
step (§16.2, Phase 0).

---

## 1. Problem and goals

The Hub creates user records through interactive sign-in, the admin invite and allow-list endpoints,
and development seeding. `POST /api/v1/users` is registered but always returns 403. Operators and
automation need one well-specified administrative operation that pre-registers a person with chosen
profile fields. It must be safe to repeat, and usable with an ordinary reduced-permission hub UAT once
D.2 admits bearer credentials for user administration.

Goals:

1. Define exactly what `POST /api/v1/users` does after H.2. It becomes a **provisioning** operation
   whose base effect is exactly invitation-equivalent. It creates the same pre-registration record that
   `POST /api/v1/admin/users/invite` creates, through the **same creation core and validation**. It is
   not a general create endpoint.
2. **Provisioning never lets anyone sign in outside configured authentication providers and access
   policies.** The record carries no credential and no provider binding. A person can use it only by
   completing a configured sign-in flow. **Provisioned records are equivalent to invite-created
   records on every sign-in path**: each path treats them exactly as it treats an invite-created
   record today, no better and no worse. Provisioning inherits exactly the email-ownership guarantees
   of invite, and no stronger.
3. Field-level authority:
   - Profile fields need `user.invite`.
   - `admin` is never provisionable.
   - No role is chosen or stored at provisioning. The role at activation follows the current
     configured policy. This is OD-1, **decided by ptone on 2026-09-28**; the stored-role option
     ("Option R") is not adopted (§6).
4. Deterministic outcomes, each with an HTTP status and error code, for:
   - repeats;
   - collisions;
   - suspended principals;
   - invalid input;
   - insufficient authority;
   - failures.
5. Transactional audit with credential attribution (E decoration).
6. Bounded client surfaces: API, `pkg/hubclient`, one CLI command, and a small web UI extension.
7. Existing sign-in, invitation, allow-list and invite-code behaviour does not change.

Success means H.2 can be implemented from this document without guessing, and every H.1 acceptance
criterion is met (§18).

## 2. Non-goals

- No change to existing sign-in or invitation behaviour: OAuth/OIDC/GitHub/Google/proxy sign-in,
  invite-code redemption, allow-list, activation and default-role rules, and `admin_emails`. Hub-token
  support alone does not imply any such change.
- No stored initial role and no member/viewer selection before sign-in. OD-1 is decided (ptone,
  2026-09-28): the role at activation follows the current configured policy, and there is no
  pending-role column (§6).
- No provider-subject pre-binding. The caller cannot supply provider, issuer or subject.
- No passwords, local credentials, or UAT minting for the provisioned user. UAT management stays
  session-only for the token owner, per the canonical design.
- No provisioning of the `admin` role, and no bindings of any kind at provisioning time.
- No group or project membership assignment in the request (OD-4).
- No bulk provisioning endpoint. Bulk invite already exists and is unchanged.
- No expiry of pending records (OD-5).
- No change to authorization of existing user operations (`PATCH`/`DELETE /api/v1/users/{id}`,
  invites). D.2 owns their bearer admission.
- No new `status` value.

## 3. Current state (factual, at `acc5a4b`)

### 3.1 How user records come into existence

| Path | Entry point | Record created | Authorization |
| --- | --- | --- | --- |
| Hub API sign-in (CLI/device/token exchange) | `provisionUser`, `pkg/hub/handlers_auth.go:1352`; called from `:229`, `:345`, `:1069`, `:1297`; proxy provisioner `pkg/hub/auth.go:880` | New `active` user with role from `determineUserRole`; invited rows transition to `active` (`:1408`-`:1452`) | `checkUserAuthorized` (`:1500`): `admin_emails`, then `authorized_domains`, then `user_access_mode` |
| Web OAuth callback | `pkg/hub/web.go:2475`-`:2596` | Same rules, duplicated (`TODO(NG4)` at `:2525`) | Same `checkUserAuthorized` (`:2475`) |
| Web proxy auth | `pkg/hub/web.go:2073`-`:2190` | Same rules, duplicated (suspension `:2115`, grants `:2190`) | Same `checkUserAuthorized` (`:2073`) |
| Google external bearer / GE exchange | `GoogleIdentityResolver.Resolve` (`pkg/hub/google_identity_resolver.go:141`), used by GE exchange (`ge_exchange.go`) and external-bearer auth (`auth_external_bearer.go`) | Links an `ExternalIdentity` (provider, issuer, subject) to an existing user by email, for authoritative domains only (`google_identity_resolver.go:237`-`:278`), or creates an `active` user through `provisionNewUser` (`:383`). Unchanged by H. | External-bearer trust `AllowedDomains` pre-check, then `Resolve` under the supplied `ResolvePolicy`. Unchanged by H. |
| Admin single invite | `POST /api/v1/admin/users/invite`, `pkg/hub/admin_user_invite.go:62` | Trim, lowercase, `mail.ParseAddress` (`:76`-`:80`); existing email gives 409. Creates `status=invited`, placeholder `role=member`, `invitedBy`, `inviteNote` (`:100`-`:117`). Post-commit: `LogInviteAudit(user_invited)` and `PublishAllowListChanged` (`:124`-`:125`). | Route guard `user.invite` (`pkg/hub/route_metadata.go:648`) |
| Admin bulk invite | `POST /api/v1/admin/users/invite/bulk`, `admin_user_invite.go:138` | Same, up to 1000; existing emails skipped | `user.invite` (`route_metadata.go:643`) |
| Allow-list add/import (deprecated) | `admin_allow_list.go:208`, `:285` | Same `invited` record | `hub.allow_list.update` (`route_metadata.go:633`) |
| Dev auth | `seedDevUser`, `pkg/hub/seed.go:502` | Fixed dev user plus super-admin binding | Dev mode only |
| Break-glass | `scion admin promote`, `cmd/admin.go:47` | None (promotes an existing user; direct DB) | Operator DB access |
| `POST /api/v1/users` | `createUser`, `pkg/hub/handlers_users_core.go:99`-`:104` | None. Always `403 forbidden`: "user creation is managed through sign-in flows and cannot be performed via the API" | n/a |

**Sign-in policy since [GoogleCloudPlatform/scion#2071](https://github.com/GoogleCloudPlatform/scion/pull/2071) ("require provider-verified email and unify sign-in policy across auth paths").** Activation of an `invited` record, whether created by invite or by
provisioning, follows that sign-in policy. Provisioning does not change it.

Invite codes (`/api/v1/admin/invites`, `invite_service.go`, redemption at `handlers_auth.go:1681`) are
a separate flow. Redemption requires an already-authenticated user. Invite codes do not create user
records.

### 3.2 Status, role and grants

- **User record.** `store.User` (`pkg/store/models.go:851`) has these fields:
  - `Email` (unique, `pkg/ent/schema/user.go:48`)
  - `DisplayName`, `AvatarURL`
  - `Role` (`admin|member|viewer`)
  - `Status` (`active|suspended|invited`, `models.go:893`)
  - `InvitedBy`, `InviteNote`, `Preferences`, `SessionGeneration`, timestamps

  Emails are trimmed and lowercased on write and matched case-insensitively
  (`pkg/store/entadapter/user_store.go:54`, `:116`, `:177`).
- **`invited` means pre-registered.** In `invite_only` mode the sign-in gate is
  `IsUserInvitedOrActive` (`user_store.go:350`), so an `invited` record admits that email to sign in.
  Domain restrictions are evaluated first (`handlers_auth.go:1512`), and `admin_emails`
  short-circuits.
- **The role on an `invited` row is a placeholder.** At first sign-in the role is computed by
  `determineUserRole` (`handlers_auth.go:1590`):
  - an email in `admin_emails` gets `admin`;
  - otherwise the user gets the configured default (`member|viewer`);
  - the one exception is an existing `AdminAPICreatedBy` super-admin binding, which keeps `admin`.
- **PATCH guards on invited rows.** PATCH rejects role changes on invited users
  (`errRoleOnInvitedUser`, `handlers_users_core.go:422`) and rejects `invited`→`active` (`:430`).
- **Role-to-grant mapping.** `syncHubRoleGrants` (`pkg/hub/seed.go:1514`) maps:
  - `member` → hub-members group membership;
  - `viewer` → an unconditional hub-viewer system binding;
  - `admin` → a super-admin system binding, managed separately (`executeRoleTransition`,
    `handlers_users_core.go:743`).

  At activation this call is best-effort (`handlers_auth.go:1480`). Startup reconciliation skips
  invited users (`seed.go:867`, `:932`).
- **JWTs.** JWT authentication rejects `suspended` users (`pkg/hub/auth.go:495`). Provisioning and
  invite issue no Hub JWT for the record. Later sign-in follows §5.6.

### 3.3 Existing user mutation admission and governance

- **Credential admission.** `PATCH` and `DELETE /api/v1/users/{id}` call `requireSessionCredential`
  (`handlers_users_core.go:190`). It admits only `CredentialKindInteractive` and `CredentialKindDev`
  (`:182`-`:185`).
- **UAT owner suspension.** UAT validation returns `403 user_suspended` for a suspended owner
  (`pkg/hub/useraccesstoken.go:366`, `pkg/hub/auth.go:411`-`:415`).
- **PATCH field authority** (`:220`-`:432`):
  - `role` needs `user.promote`, plus `CanDelegate(super-admin binding)` when a super-admin binding is
    involved (`checkUserPromotePermission`, `:629`);
  - `status` needs `user.suspend`;
  - cross-user profile fields need `user.update`.

  Mutations and their synchronous `MutationAuditRecord`s share one transaction (`:437`-`:543`).
- **Last-admin guard.** `checkLastSuperAdminTx` (`:880`) takes a row lock and counts only **active**
  users with an active system super-admin binding. It is enforced on demotion and on `DELETE`
  (`:1011`). Self-demotion and self-deletion are refused.
- **Stock roles** (`hubAdminPermissionIDs`, `seed.go:694`). Hub-admin holds `user.read`, `user.list`,
  `user.update` and `user.invite`. It lacks `user.suspend`, `user.promote` and `user.delete`.
  Super-admin holds all permissions.
- **Registry** (`pkg/hub/permissions/registry.go`):
  - `user.invite` (`:253`) has UAT selector `user:invite` and `NonRouteUse` only.
  - `user.promote`, `user.suspend` and `user.delete` (`:254`-`:256`) have no UAT selector.
- **Catalog** (`pkg/hub/authzop/catalog.go`):
  - `user.admin.invite` (`:1019`, ID at `:1020`): session JWT only, `Effects` =
    `EffectIssueCredential` only (`:1034`), `GovernanceIssuerCredential`, `AuthorityEvalNone`
    (`:1040`).
  - `user.admin.promote` (`:1050`), `user.admin.suspend` (`:940`) and `user.admin.delete` (`:1076`)
    also exist.
  - `POST /api/v1/users` has no entry.
  - `MutationClassifications` maps every `CreateUser` call site. The rows span `:2711`-`:2916`, for
    example `provisionUser` `:2711`, the resolver `:2726`, web `:2731`/`:2733` and seed `:2802`. The
    invite rows are keyed by File + Function + Symbol: `handleAdminUserInvite` and
    `handleAdminUserInviteBulk`, each with `CreateUser` (`:2759`-`:2760`).
    `findStaleMutationClassifications` (`authzop/catalog_test.go:815`) fails on rows whose function
    no longer contains the symbol.
  - `AuthorityEvalKind` (`pkg/hub/authzop/operation.go:308`-`:320`) has exactly three values:
    `AuthorityEvalNone`, `AuthorityEvalProposedPost` and `AuthorityEvalBeforeAndAfter`.
- **Route metadata.** The method-agnostic `/api/v1/users` entry (`route_metadata.go:437`-`:440`)
  declares `RouteID: "users.list"`, `Permission: "user.read"`, `Resource: "user"`, `Action: "read"`,
  `Classification: RoutePolicy`. `RoutePolicy` does not enforce the declared permission
  (`route_metadata.go:1072`-`:1078`); the handler enforces authorization.

## 4. Proposed design overview (baseline)

`POST /api/v1/users` stops returning 403 and implements operation **`user.admin.provision`**.
(Before H.2, main catalogued the refusing route as a placeholder operation `user.provision` with an
out-of-scope bearer disposition. H.2 renames it to `user.admin.provision`, which matches the other
`user.admin.*` operations; §13.3 rejects a *permission* named `user.provision`, not this operation.)

```text
caller (session | dev | hub UAT after D.2)
   │  POST /api/v1/users {email, displayName?, note?}
   ▼
admission ── D.2 per-operation admission for user.admin.provision:
   │         credential kind ∈ {interactive, dev, uat}; for UATs: exact user.invite selector in the
   │         frozen permission ceiling (A.1/A.2), credential boundary reaches the hub target (A.1),
   │         owner active, D.2 credential restrictions
   ▼
authorize ── live user.invite at hub scope ∩ credential ceiling (every request, including replays;
   │          before the body is decoded, so unauthorized callers get a uniform 403, §8 ordering note)
   ▼
validate ── strict field set and bounds; email parity with invite (shared validator)
   ▼
tx ── createPendingUserTx (shared with invite):
   │   lookup by normalized email → replay (200) | conflict (409)
   │   insert users(status=invited, role=member placeholder, display_name, invited_by, invite_note)
   │   insert mutation_audit(user_provision, actor, credential decoration)
   ▼
201 {user: ProvisionedUser, created:true}
                      … later …
person completes a configured sign-in flow → each path treats the record exactly as an
invite-created record (unchanged by H)
```

The resulting account state is identical to an invite, with one addition: an optional display name,
which follows today's precedence at activation (OD-6). What provisioning adds over invite:

- a standard REST entry point;
- a strict field set and bounds (email validation stays at parity with invite);
- idempotent outcomes;
- a transactional mutation audit;
- bearer admission.

## 5. Operation contract (baseline)

### 5.1 Route: same route, new meaning

`POST /api/v1/users` is repurposed as the provisioning operation. §13.1 explains why there is no new
route.

- The handler `createUser` (`handlers_users_core.go:99`) is replaced by `handleProvisionUser`. The
  new name avoids a collision with the sign-in `provisionUser` in `handlers_auth.go`. H.2 places it
  in a new file, `pkg/hub/handlers_users_provision.go`, and `handleUsers` dispatches POST to it;
  this keeps it apart from the D.2-owned functions of `handlers_users_core.go`.
- The route stays `RoutePolicy`, like PATCH and DELETE.

`POST /api/v1/users` does **not**:

- create `active` users;
- set status, avatar, preferences or role;
- bind provider identities;
- create any grant.

Attempts to do any of these are rejected (§8).

### 5.2 Shared creation core with invite

The two entry points must not diverge. H.2 therefore extracts the body of `handleAdminUserInvite` into
one core in `admin_user_invite.go` that both handlers call:

```go
// illustrative
type PendingUserSpec struct {
    Email       string  // raw input; normalized inside
    DisplayName string  // provision only; invite passes ""
    Note        *string
    InvitedBy   string  // human principal ID (the UAT owner for bearer calls)
}
type PendingOutcome int // Created | ExistingPendingIdentical | ExistingPendingDifferent | ExistingActive | ExistingSuspended

// NormalizeInviteEmail is today's invite rule moved without change:
// strings.TrimSpace(strings.ToLower(s)); non-empty; mail.ParseAddress(email) succeeds.
// Parity note: mail.ParseAddress accepts RFC 5322 display-name forms such as
// "Bob <bob@x.com>", and invite then stores the whole lowercased string today
// (admin_user_invite.go:76-77). This rule keeps that behaviour for both entry points;
// tightening it (for example requiring addr.Address == email) is part of OD-9(b).
func NormalizeInviteEmail(raw string) (string, error)

// createPendingUserTx looks up by normalized email and creates the invited row in the supplied store/tx.
// It never modifies an existing record.
func createPendingUserTx(ctx context.Context, st store.Store, spec PendingUserSpec) (*store.User, PendingOutcome, error)
```

**Invite handler.** It calls the core without a mutation audit, as today, and maps every outcome other
than `Created` to its current `409 "user already exists"`. It keeps:

- its current response shape;
- its audit event;
- its event publication.

**Bulk invite does not move onto the creation core.** It keeps its own per-email loop, its own
`CreateUser` call and its skip-existing behaviour. Its only change is that it calls
`NormalizeInviteEmail` in place of its inline copy of the same rule (`admin_user_invite.go`, bulk
loop), so all three entry points share one validator. Characterization tests pin invite and bulk
invite responses as unchanged.

**Mutation classification consequence.** `MutationClassifications` rows are keyed by File + Function
+ Symbol. Moving the single-invite `CreateUser` call into `createPendingUserTx` makes the
`handleAdminUserInvite`/`CreateUser` row (`catalog.go:2759`) stale, and
`findStaleMutationClassifications` would fail. H.2 therefore **replaces** that row with a
`createPendingUserTx`/`CreateUser` row, classified under A.1's shared-call-site convention (§7.2).
The `handleAdminUserInviteBulk`/`CreateUser` row (`:2760`) stays, because bulk keeps its own call.

**Provision handler.** It calls the core inside `WithTx` together with the mutation audit, and maps
outcomes per §8.

**One validator.** Email validation is the same function for all entry points: whatever invite
accepts or rejects, provision accepts or rejects. This is **email parity with invite**, not a strict
email check: the rule accepts display-name forms (see the parity note above). Any future tightening
changes both together (OD-9(b)), and is out of scope here.

**Invitation equivalence.** Because OD-1 is decided against a stored role, `display_name` is the only field
provision stores that invite does not. It is a profile value that grants no authority, and it follows
today's sign-in precedence (OD-6). This keeps `user.invite` appropriate as the base permission
(§7.1).

### 5.3 Request

```go
// pkg/hub (server) — illustrative. The pkg/hubclient request type is the same without Role (§14.2).
type ProvisionUserRequest struct {
    Email       string  `json:"email"`                 // required
    DisplayName *string `json:"displayName,omitempty"` // optional profile field
    Note        *string `json:"note,omitempty"`        // optional admin note (stored as InviteNote)
    Role        json.RawMessage `json:"role,omitempty"` // recognized so it can be rejected precisely; any non-null value is refused, see below
}
```

Decoding is strict, following the PATCH pattern:

- The body is decoded to `map[string]json.RawMessage`.
- Any key outside `{email, displayName, note, role}` gives `400 invalid_request` with the message
  `unknown field "<f>"; allowed fields are email, displayName, note`.
- An explicit `null` counts as absent.

| Field | Rule | Failure |
| --- | --- | --- |
| `email` | Required. Must pass `NormalizeInviteEmail` (the shared invite rule). | `400 invalid_request`, "valid email is required", `details.field: "email"` (same code and message as invite) |
| `displayName` | Trimmed (`strings.TrimSpace`). At most 128 Unicode code points after trim. No control characters (`unicode.IsControl`). Empty after trim counts as absent (stored `""`). No Unicode normalization: bytes are stored and compared as sent, after trim. | `400 validation_error`, `details.field: "displayName"` |
| `note` | **Not trimmed**, matching invite. `""` counts as absent and is stored as NULL, matching invite (`admin_user_invite.go:95`-`:98`). At most 500 code points. No control characters except `\n`. No Unicode normalization. | `400 validation_error`, `details.field: "note"` |
| `role` | Any non-null value, of any JSON type, is rejected with 422 (OD-1 decided, §6); the JSON string `"admin"` gets a distinct reason. No type check applies to `role` (§8 row 9 excludes it). | `422 unprocessable` with reason `privileged_role_not_provisionable` (the string `"admin"`) or `role_selection_not_supported` (any other non-null value, including non-strings) |

**Advisory warnings.** These conditions do not fail the request, because invite accepts such emails
today. They describe how the paths that evaluate `checkUserAuthorized` (API login, device and token
exchange, web OAuth and web proxy) will treat the email. Every sign-in path treats the record exactly
as it treats an invite-created record (§5.6). Warnings are informational.

- `reserved_identity`: the email matches `isReservedPlatformIdentity` (`auth.go:673`). Sign-in
  refuses it through the reserved-identity checks (for example `handlers_auth.go:1357`,
  `web.go:2061`, `google_identity_resolver.go:156`).
- `domain_not_authorized`: `authorized_domains` is non-empty, the email is outside it, and it is not
  in `admin_emails`.
- `sign_in_currently_blocked_by_access_mode`: the mode is `domain_restricted` and no domains are
  configured.

Rejecting these instead would diverge from invite. OD-9 records the option of tightening both entry
points together.

### 5.4 Response

The response uses a dedicated DTO, not `store.User`. `store.User` would serialize the placeholder
`role` (wrong when `default_user_role=viewer`), `sessionGeneration`, and any future store field.

```go
// illustrative
type ProvisionedUser struct {
    ID          string    `json:"id,omitempty"`          // detailed view only
    Email       string    `json:"email"`
    Status      string    `json:"status"`                // always "invited"
    DisplayName string    `json:"displayName,omitempty"` // detailed view only
    InvitedBy   string    `json:"invitedBy,omitempty"`   // detailed view only
    InviteNote  *string   `json:"inviteNote,omitempty"`  // detailed view only
    Created     *time.Time `json:"created,omitempty"`    // record creation time; set only in the detailed view (pointer, so omitempty omits it)
}
type ProvisionUserResponse struct {
    User     ProvisionedUser `json:"user"`
    Created  bool            `json:"created"`            // false on idempotent replay
    Warnings []string        `json:"warnings,omitempty"` // advisory only
}
```

There is no `role` field. The role of an invited record is decided at first sign-in (§3.2).

- **Created (row 13):** `201 Created` with `Location: /api/v1/users/{id}` and the **detailed view**
  (all fields). Every value either came from this request or was just generated for it.
- **Identical replay (row 14):** `200 OK` with `created:false`. The body depends on **detail
  authority** (below):
  - with detail authority: the detailed view;
  - without it: the **minimal view** `{email, status}` only. `id`, `invitedBy`, `inviteNote`,
    `displayName` and `created` are omitted, because the record may have been created by someone else
    and the user ID is information that only `user.read` otherwise gives.
- **Detail authority** means the caller also holds `user.read` at hub scope, evaluated exactly like
  `user.invite`: live user authority intersected with the credential ceiling (for a UAT, through the
  exact `user:read` selector mapping). It is evaluated only after the caller has passed the
  `user.invite` check (§8 row 8), and it never admits or denies the request. It only selects the
  response view and the collision detail (§8 rows 14-17). A session hub-admin has it; a hub UAT
  `{user:invite}` does not.
- The response never contains credentials, invite codes, or provider data.

### 5.5 Account state created

| Attribute | Value |
| --- | --- |
| `id` | new UUID |
| `email` | normalized email |
| `status` | `invited` (existing value) |
| `role` | placeholder `member`, exactly as invite; never authoritative while invited; not returned by the provision response (§5.4) |
| `display_name` | request value or `""` |
| `invited_by` | actor's human user ID (including for UATs) |
| `invite_note` | request `note` or NULL |
| role bindings / group memberships | **none** |

### 5.6 Activation and identity linking at first sign-in (unchanged)

The baseline changes nothing at sign-in. The governing rule is **sign-in equivalence**: on every
sign-in path, a provisioned record is treated exactly as an invite-created record with the same
email, status and note. H.2 proves this with characterization tests on each path (§16.4).

1. **Email ownership.** Each sign-in path establishes the email by its own provider rules, exactly
   as today. Provisioning inherits **exactly the email-ownership guarantees of invite, and no
   stronger**. H does not change them.
2. **Admission on the `checkUserAuthorized` paths** (API login, device and token exchange, web OAuth,
   web proxy). `checkUserAuthorized` runs unchanged:
   - `invite_only`: the `invited` record is the admission, exactly as for invite;
   - `domain_restricted`: the email domain must match;
   - `open`: the record adds no admission.
3. **Activation.** The existing `invited` branch runs unchanged (`handlers_auth.go:1408`,
   `web.go:2127`, `web.go:2523`). The role comes from `determineUserRole` (`admin_emails`, then an
   existing UI-promoted admin binding, then the configured default), and grants come from
   `syncHubRoleGrants`.
4. **Google external bearer and GE exchange.** H leaves these paths unchanged, and a provisioned
   record is treated on them exactly as an invite-created record.
5. **Sign-in policy.** Activation of a provisioned record follows the existing sign-in policy
   (GoogleCloudPlatform/scion#2071, title as in §3.1), exactly as for an invite-created record.
   Provisioning cannot tell at creation time how a provider will report an address, so H.2 adds no
   verification warning; OD-9(a) keeps invite and provision at parity.

## 6. Option R — stored initial role (not adopted; OD-1 decided)

**Decision (ptone, 2026-09-28, relayed by pat-refactor):** the role at activation follows the current
configured policy. There is no optional `member`/`viewer` selection before sign-in and no
pending-role column. OD-1 is closed (§19).

Option R would have let a caller choose `member` or `viewer` for a pending record, applied at first
sign-in. It is not adopted, for these reasons:

- It changes today's rule that invited accounts take the configured default at sign-in, and today's
  refusal of role assignment on invited users.
- It is a delayed authority grant. It would need `user.promote` plus `CanDelegate` at request time,
  B.3 durable provenance, re-evaluation before activation, atomic activation on all three activation
  sites, and consume-and-clear semantics. That widens H.2 into every sign-in path and adds a hard B.3
  dependency.
- Without it, the base effect stays exactly invitation-equivalent, so `user.invite` remains the
  right base permission (§7.1).

Consequences for this design: a `role` field in the request is always rejected with 422 (§8 row 12).
There is no role field in the response, CLI or web form, and H.2 has no Option R phase. Any future
proposal to store a role is a new design. If one is made, note that `EffectGrantAuthority` requires
`DelegationKind` ≥ `DelegationNonAmplification` (`operation.go:289`), which
`DelegationConditionalIncrease` satisfies, and that `AuthorityEval` follows
`effectAuthorityEvalRequirements` as it stands then. §13.4 records the alternative.

## 7. Authority and governance

### 7.1 Permissions (least privilege)

| Request content | Required (live user authority ∩ credential ceiling) | Rationale |
| --- | --- | --- |
| `email`, `displayName`, `note` | `user.invite` on the hub scope | The base effect is exactly invitation-equivalent (§5.2): a pre-registration record that admits sign-in under `invite_only`. Hub-admin already holds `user.invite`. |
| `role: member\|viewer` | Rejected (422) for every caller (OD-1 decided, §6) | It would be a delayed authority grant; the role at activation follows the configured policy. |
| `role: admin` | Never provisionable (422) | The super-admin binding stays on PATCH after activation, with its CanDelegate, self and last-admin guards. |

The invitation effect is not inert. An `invited` record can enable later admission under
`invite_only`, and that effect outlives the request and the credential that created it. That is true
even though activation derives the role from configuration. §7.3 and OD-10 treat it as such.

`user.invite` is appropriate only while the base effect stays exactly invitation-equivalent. Any
future change that stores authority-conferring state on a pending record must revisit the permission
model. OD-1 decided against storing a role (§6).

No new permission ID is introduced (§13.3). `user.invite` gains an `Enforcement` entry for
`pkg/hub/handlers_users_provision.go:handleProvisionUser`, the new file that holds the handler (§5.1).
`user.read` records the same site, for the detail-authority check (§5.4).

**`admin_emails` is independent.** Rejecting `role: admin` does not stop an email listed in
`admin_emails` from becoming admin at sign-in. That is independent authority configured by the Hub
operator. It exists whether or not a record was provisioned, and provisioning does not grant it.

### 7.2 Catalog entry (new)

```go
{
    ID:          "user.admin.provision",
    Domain:      "user.admin",
    Description: "Pre-register a user (status invited); invitation-equivalent; no role, no grants",
    EntryPoints: []EntryPoint{{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users", Method: "POST"}},
    Principals:  []PrincipalKind{PrincipalUser},
    Credentials: []CredentialKind{CredentialSessionJWT /*, CredentialScopedUAT when D.2 admission lands */},
    ResourceResolver: "hub-scoped",
    BasePermission:   "user.invite",
    Effects:          []SecurityEffect{EffectCreateResource, EffectIssueCredential},
    DelegationKind:   DelegationNone,
    Governance: &GovernancePolicy{Kind: GovernanceIssuerCredential,
        Description: "Pre-registration admits sign-in under invite_only, identical to user.admin.invite"},
    // AuthorityEvalNone, matching user.admin.invite (catalog.go:1040). Neither EffectCreateResource
    // nor EffectIssueCredential has an entry in effectAuthorityEvalRequirements (operation.go:340).
    AuthorityEval: AuthorityEvalNone,
    AuditObligation: &AuditObligation{
        EventType:     "user.admin.provision",
        ContextFields: []string{"actor_id", "credential_id", "credential_kind"},
        AfterFields:   []string{"target_user_id", "email", "status", "display_name"},
        Atomic:        true,
    },
    // H.2 PR-1 (Phase 0 binding): forbidden covers rows 4, 5 and 8 (row 5 carries the
    // session-only reason); user_suspended comes from the auth middleware (row 3); conflict
    // covers rows 15-17; role_assignment_forbidden is the denial-log classification of row 12
    // (wire code unprocessable). DenialCredentialInsufficient is added in Phase 2, when hub
    // token admission can return it (rows 6-7).
    DenialCodes: []DenialCode{DenialForbidden, DenialUserSuspended, DenialConflict,
        DenialRoleAssignmentForbidden},
    TestRefs:    []TestRef{{Package: "pkg/hub", Function: "TestHandleProvisionUser"}},
    // H.2 PR-1: token admission opens in Phase 2.
    Bearer:      SessionOnly(ReasonGovernancePending),
}
```

The catalog is now split into per-area files; this entry lives in `pkg/hub/authzop/catalog_identity.go`,
and references to `catalog.go` below mean that file for operation entries (the
`MutationClassifications` table stays in `catalog.go`).

The entry follows the full `OperationSpec` shape of `user.admin.invite` (`catalog.go:1019`-`:1049`),
including `TestRefs`, which catalog validation requires (`authzop/validate_test.go:194`, "at least one
test reference is required"; invite's is at `catalog.go:1048`). The test name `TestHandleProvisionUser`
is chosen to avoid the existing `TestProvisionUser*` tests of the sign-in provisioner
(`handlers_auth_test.go:815` and others). `TestCatalogTestRefsExist` (`catalog_test.go:480`) requires
`TestHandleProvisionUser` to exist in `pkg/hub`.

Every constant above exists in today's `authzop` vocabulary (`pkg/hub/authzop/operation.go`):
`EffectCreateResource`, `EffectIssueCredential`, `DelegationNone`, `GovernanceIssuerCredential`,
`AuthorityEvalNone`, `CredentialSessionJWT`, `CredentialScopedUAT`, `DenialForbidden`,
`DenialCredentialInsufficient` (Phase 2), `DenialUserSuspended`, `DenialConflict` and
`DenialRoleAssignmentForbidden` (`operation.go:573`).

**Effects.** The entry **extends invite's effect set with `EffectCreateResource`**. `user.admin.invite`
has only `EffectIssueCredential` (`catalog.go:1034`). In catalog vocabulary, `EffectIssueCredential`
and `GovernanceIssuerCredential` label an **admission-conferring record**: the `invited` row admits
sign-in under `invite_only`. That is consistent with goal 2: the record is not a secret or token, and
nobody can present it. It is a pre-registration that a configured sign-in flow consults.

**Audit fields.** `AfterFields` matches the mutation audit `AfterSummary` in §11 field for field
(`target_user_id` is the record's `TargetID`). §11 has the full name-to-sink map.

**Mutation classification.** The single-invite row `handleAdminUserInvite`/`CreateUser`
(`catalog.go:2759`) is **replaced** by a `createPendingUserTx`/`CreateUser` row in
`pkg/hub/admin_user_invite.go`. The core is reached from both `user.admin.invite` and
`user.admin.provision`, so H.2 classifies the new row under A.1's final convention for shared call
sites. The bulk row `handleAdminUserInviteBulk`/`CreateUser` (`:2760`) stays, because bulk keeps its
own call (§5.2). The invite **operation entry** (`user.admin.invite`, `:1019`) is unchanged. A.1's
route/catalog drift inventory ([ptone/scion#2117](https://github.com/ptone/scion/issues/2117)) must
list the new entry point.

### 7.3 Credential admission

| Credential | Admitted | Why |
| --- | --- | --- |
| Interactive session | Yes | Primary administrative path |
| Dev | Yes | Parity with PATCH/DELETE (`handlers_users_core.go:182`-`:185`) |
| Hub UAT | Yes, **after D.2** | Agreed product decision: a hub UAT carries user identity, reduced by the token |
| Project UAT | No: `403 forbidden` / `credential_insufficient` | The target is the hub scope, which a project boundary does not reach |
| Agent JWT, broker, federation, external bearer without user identity | No: `403 forbidden` | Not user principals |
| Unauthenticated | `401 unauthorized` | |

A hub UAT is admitted only if **all** of the following hold. The boundary check alone is not
sufficient.

1. **Credential validity.** The token is valid (not revoked or expired) and its owner is active (not
   suspended).
2. **Exact selector in the permission ceiling.** The token's frozen permission ceiling (A.2,
   [ptone/scion#2118](https://github.com/ptone/scion/issues/2118)) contains `user.invite`, reached
   through A.1's explicit, fail-closed selector-to-permission mapping
   ([ptone/scion#2117](https://github.com/ptone/scion/issues/2117)) from `user:invite`. No alias,
   implication or manage expansion can produce it. An empty, malformed or unknown-version ceiling
   denies.
3. **Boundary reaches the target.** A.1's target-scope resolution classifies user creation as a
   hub-scope target, through an explicit creation-scope rule like the one used for project creation.
   The credential boundary must admit that target. A hub boundary does; a project boundary does not.
   An unresolved or unknown target never admits.
4. **Live authority.** The owner currently holds `user.invite` at hub scope, evaluated against current
   grants and intersected with the ceiling (see "What counts as authority" below).
5. **Credential restrictions.** The operation-specific restrictions that D.2 declares for user
   administration pass.
6. **Invitation-effect governance.** The whole invitation effect (a record that can enable later
   admission, §7.1) is authorized for the scoped caller under the governance and `CanDelegate`
   contract that D.2 and B.3 define for invitation-type effects. H.2 does not invent that contract. If
   D.2/B.3 require a delegation check or durable provenance for the invitation effect, H.2 applies it
   through their mechanism. Phase 0 (§16.2) binds this, and OD-10 records the lifetime decision that
   depends on it.

**What counts as authority** (all credentials, including sessions). Authority for provisioning
requires the **exact canonical permission** `user.invite`, held through an **actual grant** at hub
scope, and evaluated **after** the existing scope-sensitive grant filters and constraints. Holding the
raw permission ID in some role is not enough. Seeded hub-member catalog grants do not count on their
own: a grant counts only if it applies to the actual target, which is the hub-level user collection.
The same rule applies to the `user.read` detail-authority check (§5.4). This follows the A.1 ruling on
system authority for an exact permission and an actual target; H.2 binds to A.1's final helper in
Phase 0.

**Active project access** (A.1) does not apply, because the target is hub scope, not a project.
**Mint eligibility** (A.1) decides whether a hub UAT carrying `user:invite` can be issued at all. That
is the issuer's current authority at mint time, and it is outside H.

D.2 owns replacing `requireSessionCredential` with per-operation admission. H.2 calls D.2's mechanism
for `user.admin.provision` and adds no parallel path. The concept-to-contract binding is H.2 Phase 0
(§16.2).

### 7.4 Reduced-permission hub UAT examples

- Hub-admin U holds a hub UAT `{user:invite}`. U can provision. After U's hub-admin binding is
  removed, the next request with the same token gets 403. On a collision, this token gets only the
  undifferentiated `409 user_exists` and the minimal replay view, because its ceiling lacks
  `user:read` (§5.4, §8).
- A super-admin's hub UAT holding only `{hub.settings:read}` gets 403, because the exact selector is
  absent.
- A project-bounded UAT holding `user:invite` should not be mintable: A.1's provisional
  `PermissionAllowedBoundaries` lists `user.invite` as hub-only. If one exists anyway, it gets 403 at
  the boundary.
- A caller that holds `user.invite` and sends `role` gets 422. A caller without
  `user.invite` gets 403 first (§8).

### 7.5 Field-level authority summary

| Field | Who may set it at provisioning |
| --- | --- |
| `email`, `displayName`, `note` | Holders of `user.invite` |
| `role ∈ {member, viewer}` | Nobody (OD-1 decided, §6) |
| `role = admin`, `status`, `avatarUrl`, `preferences`, `id`, provider identity, groups, project roles | Nobody |

After provisioning, pending records are edited through existing operations and gates:

- PATCH `displayName` needs `user.update`;
- PATCH `status=suspended` needs `user.suspend`;
- DELETE needs `user.delete`.

### 7.6 Last-admin and privileged-role invariants

- Provisioning never removes or changes existing authority.
- Invited records never count as surviving admins, because `checkLastSuperAdminTx` counts only
  `active` users.
- No binding is created, so a caller cannot assign authority it does not hold.

## 8. Outcomes table

All errors use the existing envelope (`writeError`, `ErrCode*`), with `details.reason` as a
machine-readable reason. Checks run in the order shown, and the first failing row wins.
**Admission and authorization (rows 1-8) run on every request, including replays, before the body is
decoded and before any lookup.** Rows 2-3 follow the existing middleware order: on the UAT path,
`ValidateToken` rejects a revoked or expired token (`useraccesstoken.go:345`-`:351`) before it checks
the owner's status (`:366`), so a revoked token whose owner is suspended gets 401.

| # | Situation | HTTP | `code` / `details.reason` | State | Audit |
| --- | --- | --- | --- | --- | --- |
| 1 | No identity | 401 | `unauthorized` | none | none |
| 2 | UAT revoked/expired/unknown | 401 | `unauthorized` (existing UAT validation, `useraccesstoken.go:345`-`:351`) | none | none |
| 3 | JWT or UAT path, caller or token owner suspended | 403 | `user_suspended` (existing middleware: `auth.go:495` for JWTs; `useraccesstoken.go:366`, `auth.go:411`-`:415` for UATs; D.2 may refine) | none | none |
| 4 | Non-user principal (agent, broker, federation) | 403 | `forbidden` | none | denial log |
| 5 | UAT before D.2 admission is enabled | 403 | `forbidden` / `credential_insufficient` (PR-1: the session-only refusal, `details.reason: "GOV_PENDING"`, `details.credential: "session_required"`; see the Phase 0 binding in §16.2) | none | denial log |
| 6 | UAT whose boundary does not admit the hub target | 403 | `forbidden` / `credential_insufficient` | none | denial log |
| 7 | UAT whose frozen ceiling lacks the exact `user.invite` mapping | 403 | `forbidden` / `credential_insufficient` | none | denial log |
| 8 | Caller lacks live `user.invite` (§7.3 "What counts as authority"), or invitation-effect governance denies (§7.3 item 6) | 403 | `forbidden` (structured: resource `user`, action `invite`) | none | decision log |
| 9 | Malformed JSON / body not a JSON object / unknown field / wrong JSON type for a known field (except `role`, row 12) | 400 | `invalid_request` | none | none |
| 10 | Invalid email (shared invite rule) | 400 | `invalid_request`, `details.field: "email"` | none | none |
| 11 | Invalid displayName or note | 400 | `validation_error` with `details.field` | none | none |
| 12 | `role` present | 422 | `unprocessable` / `privileged_role_not_provisionable` (admin) or `role_selection_not_supported` | none | denial log |
| 13 | New email | 201 | `created:true`, detailed view, optional `warnings` | invited record | `user_provision` (atomic) |
| 14 | Existing `invited` record whose `display_name` and `invite_note` equal the normalized request (**identical replay**) | 200 | `created:false`; detailed view with detail authority, minimal view `{email, status}` (no `id`) without it (§5.4) | none | no mutation audit; info log `user_provision_replay` |
| 15 | Existing `invited` record with any differing field | 409 | with detail authority: `conflict` / `pending_user_exists`, `details.userId`. Without it: `conflict` / `user_exists`, no `userId` | none | none |
| 16 | Existing `active` user (any case variant) | 409 | `conflict` / `user_exists` (no `userId`, for every caller) | none (never modified) | none |
| 17 | Existing `suspended` user | 409 | with detail authority: `conflict` / `user_suspended_exists`. Without it: `conflict` / `user_exists` | none (never reactivated) | none |
| 18 | Unique-index race | classified by re-read as row 14, 15, 16 or 17, including the detail-authority variant | | at most one record | at most one `user_provision` |
| 19 | Store error before commit | 500 | `internal_error` | rolled back | none |
| 20 | Audit write fails | 500 | `internal_error` | **rolled back: no user row** | none |
| 21 | Post-commit side effects fail (event publish, invite audit log) | 201 | | committed | mutation audit present; failure logged |

Notes on the table:

- **Ordering.** Authorization (row 8) runs before decoding and validation (rows 9-12). A caller
  without `user.invite` therefore always gets a uniform 403 and learns nothing about field rules,
  including the `role` rules. The baseline authorization does not depend on the body, so this order is
  possible.
- **Idempotency key.** The key is the normalized email, which the unique index enforces. There is no
  `Idempotency-Key` header (OD-7).
- **Replay comparison.** Replays compare normalized semantic inputs, using the §5.3 rules:
  `displayName` trimmed (`""` when absent or empty), and `note` untrimmed, with absent and `""` both
  meaning NULL. So `note:""` and an absent note are the same input. An invite-created record with the
  same note and an empty display name counts as an identical replay of a provision request without
  `displayName`. That is correct, because the state is the same.
- **Collision disclosure.** Invite returns one undifferentiated `409 "user already exists"`
  (`admin_user_invite.go:83`-`:86`). Provision matches that for callers without **detail authority**
  (`user.read`, live ∩ ceiling, §5.4): rows 15-17 collapse to `409 conflict` / `user_exists` with no
  `userId`, and a replay returns only the minimal view `{email, status}`, with no `id`. For example, a hub UAT `{user:invite}` has no
  `user:read` in its ceiling, and neither does a custom role that holds `user.invite` without
  `user.read`. Only callers who could already read the user record get the distinguishing reason,
  `details.userId`, the record's `id` and the detailed replay body. Row 16 never carries `userId`, for
  any caller. One signal remains for callers without detail authority: a 200 reveals that a pending
  (`invited`) record exists whose stored note and display name equal the request, where a 409 covers
  every other existing record. That is more than invite's single undifferentiated 409, and it is
  accepted as the cost of idempotent replays.
- **Outstanding invite codes** are not user records and do not collide.
- **Case normalization.** `Alice@Example.com ` and `alice@example.com` are the same key.
- **Role-assignment rollback.** Not applicable, because no role is stored and no grants are written.
- **Lifetime of the created record.** Once created, the record's lifetime is independent of the
  request. What happens to a record created through a hub UAT when the token or its owner later
  changes is **OD-10**, decided by ptone on 2026-10-04 as option (a): the record persists (§12).

## 9. Transactionality

```go
// illustrative
if err := s.authorizeProvision(ctx); err != nil { return deny(err) }   // rows 1-8, every time, before decoding
spec, err := decodeAndValidateProvision(r)                              // rows 9-12
if err != nil { return reject(err) }
detail := s.holdsUserRead(ctx)   // live ∩ ceiling; selects the response view only (§5.4)
var out PendingOutcome; var u *store.User
err = s.store.WithTx(ctx, func(tx store.Store) error {
    var err error
    u, out, err = createPendingUserTx(ctx, tx, spec)
    if err != nil || out != Created { return err }
    return tx.CreateMutationAudit(ctx, provisionAudit(u, buildAuditActorFromContext(ctx)))
})
if errors.Is(err, store.ErrAlreadyExists) { u, out, err = reclassify(ctx, spec) } // row 18, read-only
if err != nil { return internalError(err) } // rows 19-20; the tx rolled back
return respond(u, out, detail) // rows 13-18, view chosen by detail
```

- **Before the transaction:** admission and authorization, then decoding and validation, then the
  detail-authority check.
- **Inside the transaction:** the user row and the audit. An audit failure rolls back the user row.
- **No updates:** the core never updates an existing row. A concurrent insert that loses the
  unique-index race is re-read and classified without writing.
- **After commit, best-effort:**
  - `LogInviteAudit(..., InviteAuditUserProvisioned, ...)` with the new event type
    `user_provisioned`;
  - `s.events.PublishAllowListChanged(ctx, "provisioned", email)`.

  A failure here is logged and does not change the `201` (row 21). The mutation audit is already
  committed.

## 10. Interaction with invitations and OAuth

- **Coexistence.** Invite, bulk invite and allow-list are unchanged. Invite and provision share one
  creation core and one email validator, so they cannot diverge.
- **Precedence.** There is one record per email. The first creator wins, and the other entry points
  get 409, or skip the email in bulk invite, as today. Activation follows today's rules: `admin_emails`,
  then an existing UI-promoted admin binding, then the configured default.
- **Existing sign-in and invitation behaviour is unchanged.** No change is inferred from hub-token
  support.
- **Allow-list DELETE**, which removes `invited` records, also removes provisioned pending records.

## 11. Audit

| Event | Where | Fields |
| --- | --- | --- |
| `MutationAuditRecord{MutationType: "user_provision"}` | in the transaction (atomic) | `ActorPrincipalKind`, `ActorPrincipalID` (human user), `ActorCredentialID`, `ActorCredentialType` (from `buildAuditActorFromContext`, `handlers_users_core.go:570`), `TargetType:"user"`, `TargetID`, `AfterSummary` `{"email","status":"invited","displayName"}` |
| Invite audit `user_provisioned` | post-commit, best-effort | email, actor ID and actor email; `details: {"user_id": <id>}` (`inviteID` empty, as for `user_invited`) |
| Decision/denial log | `logAuthzDenial` | operation ID, required permission, credential kind and ID, boundary, denial reason |

Both row 12 reasons (`privileged_role_not_provisionable` and `role_selection_not_supported`) log
under the catalog denial code `DenialRoleAssignmentForbidden`; the response reason stays as in §8.

**Name-to-sink map.** Four names are in play. Each has exactly one sink:

| Name | Kind | Sink | When |
| --- | --- | --- | --- |
| `user.admin.provision` | catalog operation ID and `AuditObligation.EventType` | the authzop catalog (declares the audit obligation; also the operation ID in decision and denial logs) | static declaration |
| `user_provision` | `MutationAuditRecord.MutationType` | `mutation_audit` table, in the transaction | row 13 only |
| `user_provisioned` | `InviteAuditEventType` (`InviteAuditUserProvisioned`, `pkg/hub/audit.go`, next to `user_invited` at `:101`) | invite audit logger, post-commit, best-effort | row 13 only |
| `user_provision_replay` | structured `slog` info message | application log | row 14 only |

**Field alignment.** The catalog `AfterFields` (`target_user_id`, `email`, `status`, `display_name`)
and the mutation audit's `TargetID` plus `AfterSummary` (`email`, `status`, `displayName`) record the
same four values. `AfterFields` uses the catalog's snake_case labels, and `AfterSummary` uses the JSON
keys of the user API. The credential ID and kind are catalog `ContextFields` and appear in
`ActorCredentialID`/`ActorCredentialType`.

**Credential decoration (E).** When E.1/E.2 land, these records carry E's single decoration structure
next to the human principal:

- token ID, kind and name;
- boundary;
- bounded issuer-supplied labels, marked as user-supplied.

H.2 consumes this structure rather than defining its own. Token plaintext and hashes are never logged.
Labels are never treated as actor identity.

## 12. Lifecycle

- **De-provisioning a never-signed-in record** uses existing operations:
  - `DELETE /api/v1/users/{id}` (`user.delete`); the last-admin guard trivially passes for invited
    records;
  - the deprecated allow-list DELETE (`hub.allow_list.update`), which applies to invited records only.

  Hub-admin, which lacks `user.delete`, can withdraw a record only through the allow-list route
  (OD-3).
- **Finding records created by a credential.** The `user_provision` mutation audit records
  `ActorCredentialID` and `ActorCredentialType`. An operator can list every record that a given token
  created, then withdraw any that are still `invited` through the operations above.
- **Records created through a hub UAT, when the token or owner changes later (OD-10, decided (a) by ptone on 2026-10-04).**
  The cases are: the source token is revoked or expires before first sign-in; the owner is suspended;
  the owner loses `user.invite`.
  - Under OD-10(a), the decision, the record persists (invitation parity: session invites
    survive the inviter's demotion today). Token expiry or revocation prevents new operations with the
    token; it does not undo the completed invitation. Creation must still have authorized the whole
    invitation effect for the scoped caller (§7.3 item 6). Durable provenance as defined by the
    D.2/B.3 invitation-effect contract (§7.3 item 6; B.3 defines none, §16.1), with at minimum the mutation audit's credential
    attribution, plus audit and explicit withdrawal (above), are required.
  - Under OD-10(b), the record is tied to B.3 provenance, and activation re-checks the source (owner
    active and still holding `user.invite`, token not revoked or expired) before the record admits
    sign-in. That is additional B.3-linked scope on the activation paths.
  - Under either option, the existing provider and access-policy checks at sign-in stay mandatory.
  - OD-10 is decided (a), so Phase 2 implements the (a) behaviour; the (b) bullet is kept for the record.
- **Suspending a pending record:** PATCH `status=suspended` (`user.suspend`). A suspended record blocks
  sign-in (`handlers_auth.go:1398`) and blocks re-provisioning (row 17).
- **Expiry:** none (OD-5).
- **After activation**, the record is an ordinary `active` user, and all PATCH/DELETE governance
  applies, including the last-admin guard.

## 13. Alternatives considered

### 13.1 Route: new `POST /api/v1/admin/users/provision` — rejected

- `POST /api/v1/users` is the REST-natural collection create and is already registered.
- Its 403 is the answer the H.1 criteria ask us to replace.
- A second route would leave a permanently dead verb and add another pattern to A.1's drift
  inventory.
- Cost of reusing it: `RoutePolicy` means the handler must enforce authorization, as PATCH and DELETE
  already do.
- This decision is load-bearing, because clients bind to the route.
- pat-refactor had no objection (§20).

### 13.2 Account state: new `provisioned` status — rejected

A new status would touch all of the following, and all of them are on sign-in paths:

- `IsUserInvitedOrActive`;
- the startup reconcilers;
- the PATCH invited guards;
- web filters;
- every activation site;
- an enum migration.

Reusing `invited` keeps admission semantics identical to invite.

### 13.3 Permission: new `user.provision` — rejected

The base effect is identical to invite. Two permissions would gate the same state differently, and
stock roles would need migration. A later split is additive, and would only be needed if a future
design stored authority-conferring state on pending records.

### 13.4 Store an initial role on the pending record (Option R) — not adopted (OD-1 decided)

- **For:** a single call fully pre-configures a person.
- **Against:**
  - it changes today's rule that invited accounts take the configured default at sign-in;
  - it is a delayed authority grant, which needs B.3 provenance, pre-activation re-evaluation and
    atomic activation (§6);
  - it widens H.2 into every sign-in path.

ptone decided OD-1 on 2026-09-28: the role at activation follows the current configured policy, with
no selection before sign-in and no pending-role column. Rationale and consequences are in §6.

### 13.5 Create grants on the invited record at provisioning — rejected

- Activation recomputes the role and runs `syncHubRoleGrants`, so it would overwrite those grants.
- Reconciliation skips invited users.
- Grants held by a principal who has never signed in would appear in authority listings.

### 13.6 Extend `POST /api/v1/admin/users/invite` instead — rejected

This would change an existing endpoint's contract and web copy, and would leave `POST /users`
forbidden. The shared core gives the same no-divergence guarantee without changing invite.

### 13.7 Pre-bind provider subject — rejected

The Hub cannot verify a caller-supplied subject, and accepting one would change sign-in linking.

## 14. Surfaces

### 14.1 API

`POST /api/v1/users` as specified above. No other new routes.

### 14.2 `pkg/hubclient`

`pkg/hubclient/users.go`, `UserService` (`:24`):

```go
// Provision pre-registers a user (status invited). Returns created=false on identical replay.
Provision(ctx context.Context, req *ProvisionUserRequest) (*ProvisionUserResponse, error)
```

- The request and response types mirror §5.3 and §5.4. The request has no `role` field (OD-1 decided, §6). The
  response user type is the `ProvisionedUser` DTO, with no `role` field; the detailed-view fields are
  optional, and `id` is optional too, because a caller without detail authority receives the
  minimal view `{email, status}`.
- 409 and 422 responses map to typed errors that expose `details.reason`. The client must treat the
  409 reason set as `{user_exists, pending_user_exists, user_suspended_exists}`, and must expect
  `user_exists` without `userId` whenever the caller lacks detail authority (§8 rows 15-17).
  `details.userId` is optional.

### 14.3 CLI

`scion hub users provision EMAIL [--display-name S] [--note S] [--json]`

- This creates a new `hub users` group; none exists today.
- H.2 adds only `provision` to it.

Mode availability (`AGENTS.md:92`, `.design/cli-modes.md`, `cmd/cli_mode.go`):

- **Agent mode:** unavailable automatically, because `hub` is not in `agentAllowed`.
- **Assistant mode:** proposed available, because `hub.users` is not in `assistantDenied`. The
  command is not interactive and not credential lifecycle. Its closest equivalent is
  `hub allow-list add EMAIL` (`cmd/hub_allow_list.go:55`), which creates the same `invited` record and
  which assistants can already run. `scion hub invite` (`cmd/hub_invite.go:36`) is a different flow:
  it manages redeemable invite **codes**, not user records.
- Under OD-8(a), `cmd/cli_mode.go` needs no map change; H.2 adds a mode test only. Under OD-8(b), H.2
  adds `hub.users.provision` to `assistantDenied`.

AGENTS.md requires developer confirmation of this choice. ptone decided OD-8 (a) on 2026-10-04; H.2
adds the mode test (`TestHubUsersProvisionCmd_ModeAvailability`, `cmd/hub_users_test.go`).

### 14.4 Web UI

`web/src/components/pages/admin-users.ts`: add an optional **Display name** field to the existing
single-invite form (`renderInviteUserDialog`, `:1919`-`:1972`; submit handler `inviteUser`,
`:1243`).

- When a display name is given, the form submits to `POST /api/v1/users`.
- Otherwise it keeps calling the invite endpoint, so existing behaviour and copy are unchanged.
- Response handling on the provision path:
  - `201`: the same success state as invite, and any `warnings` shown as non-blocking notices;
  - `200` with `created:false`: an "already pre-registered with these details" notice, with no error
    styling;
  - `409` `pending_user_exists`: "a pending record for this email exists with different details";
  - `409` `user_suspended_exists`: "this email belongs to a suspended user";
  - `409` `user_exists`: the existing invite copy ("user already exists");
  - `422`: not reachable from this form, which sends no `role`; shown as a generic error if it
    appears.
- The bulk UI is unchanged.
- There is no role selector (OD-1 decided, §6).

## 15. Migration / rollout

- **Baseline: no store migration.** Every stored value (`status=invited`, `display_name`,
  `invited_by`, `invite_note`) already exists. There are no changes to role definitions, stock role
  permissions, invites, UATs or external identities.
- No pending-role column or pending-grant record is added (OD-1 decided, §6).
- **Rollout.** The route change is additive: a 403 becomes a working operation. Bearer admission is
  enabled only through D.2's mechanism.

## 16. H.2 execution plan

H.2 was blocked by H.1, D.2 and the sign-in item. These gates are **satisfied**: this design is
approved (GoogleCloudPlatform/scion#2064); the D.2 shared admission mechanics H.2 uses have merged
(GoogleCloudPlatform/scion#2579, #2639, #2647), and the D.2 owner confirmed that Phases 0-1 and 3 do
not depend on the remaining D.2 admission batches; the sign-in item [GoogleCloudPlatform/scion#2071](https://github.com/GoogleCloudPlatform/scion/pull/2071) ("require provider-verified email and unify sign-in policy across auth paths") has landed. Phase 2 (hub-token admission) has one further gate, D.2 G5 (§16.1).

### 16.1 Dependencies

| Contract | Needed for | Binding |
| --- | --- | --- |
| D.2 [ptone/scion#2124](https://github.com/ptone/scion/issues/2124): per-operation replacement for `requireSessionCredential`; bearer admission for user administration | Admission in every phase | Hard gate for H.2: **satisfied for Phases 0-1 and 3** (shared mechanics merged). Phase 2 is gated on D.2 G5 (row below). |
| A.1 [ptone/scion#2117](https://github.com/ptone/scion/issues/2117): target-scope resolution including creation scopes, credential boundary check, explicit fail-closed selector-to-permission mapping, mint eligibility, route/catalog drift inventory | UAT boundary and exact-selector checks; catalog and registry | Concepts only; contract under review; bound in Phase 0 |
| A.2 [ptone/scion#2118](https://github.com/ptone/scion/issues/2118): normalized frozen permission ceiling with interpretation version, used by runtime decisions and `CanDelegate` | Ceiling on `user.invite` | Concepts only; bound in Phase 0 |
| E.1/E.2 [ptone/scion#2126](https://github.com/ptone/scion/issues/2126)/[ptone/scion#2127](https://github.com/ptone/scion/issues/2127) | Credential decoration in audit | Soft; use `buildAuditActorFromContext` until they land |
| B.3 [ptone/scion#2121](https://github.com/ptone/scion/issues/2121): durable ceilings and provenance | (1) The invitation-effect contract for Phase 2 (§7.3 item 6). (2) Activation re-checks, if ptone chooses OD-10(b). | (1) **Resolved: nothing to bind.** B.3 defines no provenance type, ceiling or effect regression for user or invitation records (B.3 owner, 2026-10-07). The durable provenance of §7.3 item 6 and OD-10(a) is the `user_provision` mutation audit's credential attribution, written in the same transaction as the record (§9, §11). The governance side is D.2 G5 (row below). B.3's effects apply only if a future change writes a role binding or group membership at provisioning. (2) Not applicable: OD-10 is decided (a). |
| Sign-in item [GoogleCloudPlatform/scion#2071](https://github.com/GoogleCloudPlatform/scion/pull/2071) ("require provider-verified email and unify sign-in policy across auth paths") | H.2 start (§16) | **Satisfied**: landed. H does not change sign-in. |
| OD-10: lifetime of records created through a hub UAT | Phase 2 | **Satisfied**: decided (a) by ptone on 2026-10-04. |
| D.2 G5 ([ptone/scion#2124](https://github.com/ptone/scion/issues/2124)): token admission for invitations and the allow-list | Phase 2 | **Hard gate for Phase 2.** `POST /api/v1/users` creates the same invited record as the invite routes, so its token admission follows the same rule as G5: it lands with or after G5, never before, and the operation stays `SessionOnly(ReasonGovernancePending)` until then (D.2 owner, 2026-10-07). Phase 2 reuses G5's selector handling and effect regression tests rather than a parallel version. |

### 16.2 Phases

**Phase 0 — bind to final A and D contracts (documentation step, no behaviour).** Map each concept in
§7.3 to the merged A.1/A.2/D.2 types and functions:

| Concept | Source |
| --- | --- |
| credential boundary | A.1 |
| target-scope resolution for user creation | A.1 |
| exact selector-to-permission mapping for `user:invite` | A.1 |
| frozen ceiling | A.2 |
| system authority for the exact canonical permission and the actual target, after scope-sensitive grant filters (§7.3 "What counts as authority") | A.1 (final helper; the earlier project-access helper signature is withdrawn and is not used) |
| per-operation admission and credential restrictions | D.2 |
| governance and `CanDelegate` contract for the invitation effect (§7.3 item 6) | D.2 with B.3 |
| per-method route metadata convention for `/api/v1/users` | A.1 |

Record the mapping in the H.2 PR description. If a final contract cannot express a requirement in
§7.3, stop and raise it with the lead. Do not weaken the requirement.

**Registry row obligations** (A.1 v3; names provisional until A.1 is pushed). Any new or changed
permission `Registry` row, including a `UATScope` added or changed on a row, needs matching rows in
the two per-permission tables `ProjectTargetApplicability` and `PermissionAllowedBoundaries`
(`pkg/hub/permissions/project_applicability.go`). A drift test enforces this. `SelectorRegistry` is
derived from `Registry` `UATScope`, so H adds no hand-written selector entry. Provisioning targets
the **hub-level user collection, not a project**, so the dispositions are not project-applicable and
hub-only. Concretely:

- Baseline: `user.invite` keeps its existing `UATScope` (`user:invite`). A.1's provisional tables
  already list it (`ProjectTargetApplicability["user.invite"] = false`,
  `PermissionAllowedBoundaries["user.invite"] = {Hub}`). H.2 adds only an `Enforcement` entry. If that
  entry counts as a changed row under A.1's final drift rule, H.2 re-confirms both table rows in the
  same commit.
- `user.read` (the detail-authority check, §5.4) is already listed as not project-applicable and
  hub-only. No change.

**Phase 1 — vertical slice.** One handler, one client method and one CLI command. Admission covers
session and dev credentials only, through D.2's mechanism.

1. `pkg/hub/admin_user_invite.go`:
   - extract `NormalizeInviteEmail` and `createPendingUserTx`;
   - move the single-invite handler onto both; switch bulk invite to `NormalizeInviteEmail` only
     (§5.2);
   - add characterization tests proving invite and bulk invite responses, audits and events are
     unchanged.
2. `pkg/hub/handlers_users_provision.go` (new; `handlers_users_core.go` only dispatches POST to it):
   `handleProvisionUser`, covering strict decoding, validation,
   warnings, authorization, the transaction with audit, and the outcomes table.
3. `pkg/hub/audit.go`: `InviteAuditUserProvisioned`.
4. Registry and catalog:
   - `pkg/hub/authzop/catalog_identity.go`: `user.admin.provision` (renamed from main's placeholder
     `user.provision`); `pkg/hub/authzop/catalog.go`: replace the `handleAdminUserInvite`
     classification row with a `createPendingUserTx` row; keep the bulk row (§7.2);
   - `pkg/hub/permissions/registry.go`: the `user.invite` Enforcement entry, subject to the
     registry row obligations above;
   - `pkg/hub/route_metadata.go`: per §16.3 (A.1-owned).
5. Client and CLI:
   - `pkg/hubclient/users.go`: `Provision` and its types;
   - `cmd/hub_users.go` (new);
   - `cmd/cli_mode.go`: a mode test under OD-8(a); an `assistantDenied` entry under OD-8(b).
6. Integration tests (§16.4, P1) against the real SQLite-backed Hub server.

**Validate the slice end to end before fanning out.** Provision with the CLI, sign in through the test
OAuth/proxy harness under `invite_only`, and confirm the user is `active` with the default role and
grants. Phases 2-3 are conditional on this.

**Phase 2 — hub UAT admission (conditional on the Phase 1 slice, ptone's OD-10 decision and D.2 G5,
§16.1).** Delivery: H.2 PR-3, built on G5. The H.2 PRs are PR-1 (Phases 0-1), PR-2 (Phase 3) and
PR-3 (Phase 2).
Enable UATs through D.2's mechanism with the full §7.3 checks: exact selector, boundary, live
authority, restrictions and invitation-effect governance. Implement the chosen OD-10 lifetime
behaviour. Add the UAT credential kind to the catalog, then the P2 tests. Under UAT admission,
confirm that the `user.read` detail-authority decision (§5.4; PR-1 evaluates it on
`hubScopedResource("user", "hub")` with no target evidence) resolves to the same hub target as the
`user.invite` decision, or switch it to `hubCollectionEvidence("user.read")`. Then re-add
`DenialCredentialInsufficient` to the catalog entry for rows 6-7.

**Phase 3 — web UI.** The display-name field and submit routing in `admin-users.ts`, plus web tests.
Delivery: H.2 PR-2. It does not depend on Phase 2: the web form uses a session.

**Phase 0 binding (H.2 PR-1, against main).** Each §7.3 concept is bound to merged code:

| Concept | Bound to | PR-1 use |
| --- | --- | --- |
| per-operation admission and credential restrictions (D.2) | `requireSessionCredentialFor(w, ctx, authzop.ReasonGovernancePending)` (`session_only_gate.go`, D.2 M3) and the catalog `Bearer: SessionOnly(ReasonGovernancePending)` disposition | Interactive session and dev credentials only. Every other credential kind gets the session-only refusal. |
| §8 row 5 (UAT before token admission) | the session-only refusal: `403 forbidden` with `details.reason: "GOV_PENDING"` and `details.credential: "session_required"` | Replaces the design's provisional `credential_insufficient` detail. Pinned by the bearer disposition matrix and `TestHandleProvisionUser`. |
| target-scope resolution for user creation (A.1) | `ResolveTargetScope` with `hubCollectionEvidence("user.invite")` on `Resource{Type: "user"}` (`authz_hub_target.go`); `permissions.CollectionTargetClasses["user.invite"] = {hub_resource}` | Hub-scope collection target, the same creation-scope rule as project creation. |
| credential boundary (A.1) | `PermissionAllowedBoundaries["user.invite"] = {Hub}`; the bearer gate in `AuthzService.Decide` | Unchanged; used in Phase 2. |
| exact selector-to-permission mapping for `user:invite` (A.1) | `Registry` `UATScope: "user:invite"`, from which `SelectorRegistry` derives | Unchanged; used in Phase 2. |
| frozen ceiling (A.2) | the token ceiling evaluated by `AuthzService.Decide` for UAT credentials | Phase 2. |
| system authority for the exact permission on the actual target (A.1) | `AuthzService.Decide` with `Permission: "user.invite"` and the collection evidence above; detail authority uses `Permission: "user.read"` on `hubScopedResource("user", "hub")` | Seeded hub-member grants do not include `user.invite`, so hub members are refused (tested). |
| governance and `CanDelegate` for the invitation effect (D.2 with B.3) | D.2 G5 (governance and token admission for invitation effects); B.3 defines nothing for this effect, so provenance is the transactional mutation audit (§16.1) | not used in PR-1 (session and dev only); Phase 2 binds to G5. |
| per-method route metadata for `/api/v1/users` (A.1) | none: the method-agnostic `RoutePolicy` entry stays, and POST is handler-enforced (§16.3 default) | No `route_metadata.go` change. |
| shared call-site classification (A.1) | `MutationClassifications` row `createPendingUserTx`/`CreateUser` with `ExemptionInternalOnly`, naming both callers (main's convention for shared helpers such as `replaceBindingTx`) | Replaces the `handleAdminUserInvite`/`CreateUser` row; the bulk row stays. |
| registry row obligations (A.1) | `user.invite` gains `Enforcement: pkg/hub/handlers_users_provision.go:handleProvisionUser`; `UATScope`, `ProjectTargetApplicability` (false) and `PermissionAllowedBoundaries` ({Hub}) unchanged | Drift tests stay green. |

### 16.3 Files and shared ownership

| File | Change | Shared with / coordinate |
| --- | --- | --- |
| `pkg/hub/permissions/registry.go` | `user.invite` Enforcement | **A.1 owns until merge-ready**; land after A.1 or via the A.1 owner |
| `pkg/hub/permissions/project_applicability.go` | none (existing `user.invite`/`user.read` rows re-confirmed) | **A.1 owns** (provisional name) |
| `pkg/hub/authzop/catalog_identity.go`, `pkg/hub/authzop/catalog.go` | `catalog_identity.go`: new op (renamed from the placeholder `user.provision`); `catalog.go`: replace the single-invite classification row with the `createPendingUserTx` row | **A.1 owns until merge-ready**; D.2 edits the user.admin entries |
| `pkg/hub/route_metadata.go` | `/api/v1/users` (`:437`-`:440`) declares `users.list` / `user.read` / `read` for all methods. **Default: no change**, because `RoutePolicy` does not enforce the declared permission (`:1072`-`:1078`), POST authorization is handler-enforced, and the catalog carries `user.invite` for POST. **If A.1's final convention is per-method metadata**, H.2 adds a POST entry (`RouteID: "users.provision"`, `Permission: "user.invite"`, `Action: "invite"`) per that convention. | **A.1-owned**; decided in Phase 0 |
| `pkg/hub/handlers_users_provision.go` (new), `pkg/hub/handlers_users_core.go` | `handlers_users_provision.go`: the handler; `handlers_users_core.go`: POST dispatch only (`createUser` removed) | **D.2 owns bearer admission** in `handlers_users_core.go`; no D.2-owned function changes |
| `pkg/hub/admin_user_invite.go` | shared core extraction | none known |
| `pkg/hub/audit.go` | event type | E.2 |
| `pkg/hubclient/users.go`, `cmd/hub_users.go`, `cmd/cli_mode.go` | client and CLI | C.2/D.3 edit the token client and CLI in different files |
| `web/src/components/pages/admin-users.ts` | form | none known |

### 16.4 Test list (behaviour-level; real handlers and store)

Positive:

- P1: session super-admin provisions a new email → 201, detailed view, no `role` field in the
  response, the record is `invited` with no bindings, and the audit row records credential kind
  `interactive`.
- P1: hub-admin session provisions → 201.
- P1: the provisioned person signs in under `invite_only` via API login, web OAuth and proxy → each
  path yields `active` with the configured default role and grants, identical to an invited record.
- P1: identical replay → 200 `created:false` and no second audit row. A case-variant email replays
  the same record.
- P1: replay with `note:""` versus an absent note → both are the same input (200 replay). A replay
  with `displayName:"  Bob "` against a record with `Bob` → 200 replay. A note differing only by
  surrounding whitespace → 409 (notes are not trimmed).
- P1: replays are reauthorized → a caller that has lost `user.invite` gets 403.
- P2: a hub UAT `{user:invite}` held by a hub-admin → 201. The audit carries the token ID, plus E
  decoration when available.

Negative:

- P1: unauthenticated request → 401 `unauthorized` (row 1).
- P1: a caller holding `user.invite` sends any non-null `role`, of any JSON type (`"admin"`,
  `"member"`, `5`, `true`, `{}`, `[]`) → 422, with the `admin` reason for `"admin"` and the
  not-supported reason otherwise, never 400, for every authorized credential kind (row 12).
- P1: a caller without `user.invite` sends `role`, an unknown field, malformed JSON or an invalid
  email → 403 in every case, never 400/422 (ordering, rows 8-12).
- P1: member or viewer session without `user.invite` → 403.
- P1: a hub-member whose only grants are the seeded hub-member catalog grants → 403 (no actual grant of
  `user.invite` for the hub user collection, §7.3).
- P1: malformed JSON, a non-object body (`[]`, `"x"`), and wrong JSON types (`"email": 5`,
  `"note": {}`) → 400 `invalid_request` (row 9).
- P1: unknown fields (`status`, `avatarUrl`, `preferences`, `id`, `groups`, `provider`, `subject`) →
  400 (row 9).
- P1: invalid emails → 400 with the same message invite gives. This is a table test run against both
  endpoints.
- P1: characterization of the email rule on both endpoints: `"Bob <bob@x.com>"` is accepted by invite
  and by provision today, and both store the same normalized string (email parity; OD-9(b) would
  change both).
- P1: displayName with control characters or over 128 code points → 400. Note over 500 code points →
  400.
- P1: reserved identity or out-of-domain email → 201 with the matching warning. Sign-in through the
  `checkUserAuthorized` paths (API login, device and token exchange, web OAuth, web proxy) is then
  denied.
- P1 collisions, caller **with** detail authority (session hub-admin):
  - active user → 409 `user_exists`, no `userId`, record unchanged;
  - suspended user → 409 `user_suspended_exists`, record stays suspended;
  - invited record with a different displayName or note → 409 `pending_user_exists` with
    `details.userId`, record unchanged;
  - identical replay of an invite-created record → 200 with the detailed view.
- P1 collisions, caller **without** detail authority (a custom role holding `user.invite` but not
  `user.read`): rows 15, 16 and 17 → 409 `user_exists` with no `userId`; identical replay → 200 with
  the minimal view `{email, status}` only, and the test asserts the response `user` object has exactly the keys `email` and `status`.
- P2 collisions with a hub UAT `{user:invite}` (no `user:read` in the ceiling), held by a hub-admin
  who does hold `user.read` live: the same results as the previous test (ceiling intersection).
- P1: concurrent identical requests → exactly one row and one audit record.
- P1: injected audit-write failure → 500 and no user row. Create failure → 500 and no audit row.
- P1: injected post-commit failure (event publish and invite audit log both fail) → 201, the user row
  and the `user_provision` mutation audit are present, and the failure is logged (row 21).
- P1: session caller suspended after the session was issued → 403 `user_suspended` (row 3, JWT
  path).
- P1: agent JWT, broker HMAC and federation credentials → 403.
- P1: before Phase 2, any UAT → 403 `credential_insufficient` (PR-1: the session-only refusal `GOV_PENDING` / `session_required`; see the Phase 0 binding in §16.2).
- P2: project-bounded UAT holding `user:invite` (constructed directly in the store if it cannot be
  minted) → 403.
- P2: hub UAT without the exact selector (`{user:read}`, `{hub.settings:read}`, or a manage alias) →
  403.
- P2: hub UAT whose user lost `user.invite` after the token was minted → 403.
- P2: hub UAT with an empty, malformed or unknown-version ceiling → 403.
- P2: revoked or expired hub UAT → 401 (row 2).
- P2: revoked hub UAT whose owner is also suspended → 401, not 403 (row 2 precedes row 3, following
  the existing UAT validation order).
- P2: hub UAT whose owner is suspended → 403 `user_suspended` (row 3, UAT path; D.2 may refine the
  code, and the test follows D.2 if it does).
- P2: the OD-10 behaviour ptone chooses. Under (a): token revoked, token expired, owner suspended,
  and owner loses `user.invite`, each after provisioning → the record stays `invited`; sign-in under
  `invite_only` through the `checkUserAuthorized` paths is admitted as for any invited record (the
  owner-suspended case follows the record, not the owner); the audit lists the record under the
  token's credential ID; and the record can be withdrawn through DELETE. Under (b): the same four
  cases → activation re-check denies admission, and the reason is audited.

Sign-in equivalence (no sign-in outside providers):

- P1: the provision response schema contains no credential fields.
- P1: the provisioned (invited) user cannot mint a UAT, because it has no session.
- P1: characterization — a provisioned record and an invite-created record for comparable emails
  behave identically on **each** sign-in path: API login, device and token exchange, web OAuth, web
  proxy, **GE exchange** and **external bearer**. Compare the outcome (admitted or denied, and error
  code), the resulting status, role, grants and linked identities. The test asserts equality between
  the two records; it does not assert any new behaviour.
- P1: `authorized_domains` changed after provisioning to exclude the email → sign-in through the
  `checkUserAuthorized` paths is denied.
- P1: `domain_restricted` with no domains → provisioning returns 201 with a warning, and sign-in
  through the `checkUserAuthorized` paths is denied.

No regression:

- P1: existing invite, bulk invite, allow-list add/delete and invite-code redeem tests pass unchanged.
- P1: bulk invite skips an already provisioned email.
- P1: PATCH role on a provisioned record → 409.
- P1: DELETE of a provisioned record works.
- P1: `checkLastSuperAdminTx` never counts an invited record.
- P1: catalog validation, the `MutationClassifications` scanner (including
  `findStaleMutationClassifications`), the A.1 registry/applicability drift test and A.1's route
  drift check all pass. H.2 adds `TestHandleProvisionUser` (the handler integration tests, §16.4),
  which `TestCatalogTestRefsExist` requires.
- P3: the web form uses the invite endpoint when no display name is set, and `POST /api/v1/users`
  otherwise. It renders the `200 created:false` notice and each 409 reason (§14.4).

**Row-to-test index** (§18 requires one or more tests per §8 row):

| §8 row | Test(s) above |
| --- | --- |
| 1 | unauthenticated → 401 |
| 2 | revoked or expired hub UAT → 401; revoked hub UAT with suspended owner → 401 |
| 3 | session caller suspended (JWT); hub UAT owner suspended (UAT) |
| 4 | agent JWT, broker HMAC, federation → 403 |
| 5 | before Phase 2, any UAT → 403 (session-only refusal in PR-1) |
| 6 | project-bounded UAT → 403 |
| 7 | hub UAT without the exact selector; empty/malformed/unknown-version ceiling |
| 8 | member/viewer without `user.invite`; seeded catalog grants only; lost `user.invite` (session and UAT); ordering test |
| 9 | malformed JSON / non-object / wrong types; unknown fields |
| 10 | invalid emails (both endpoints) |
| 11 | displayName and note bounds |
| 12 | any non-null `role` (any JSON type) → 422 for authorized callers |
| 13 | session super-admin and hub-admin provision; hub UAT provision |
| 14 | identical replay; `note:""` vs absent; case variant; detailed vs minimal view (minimal view has exactly `email` and `status`) |
| 15 | invited record with different fields, with and without detail authority |
| 16 | active user, with and without detail authority |
| 17 | suspended user, with and without detail authority |
| 18 | concurrent identical requests |
| 19 | create failure → 500, no audit row |
| 20 | audit-write failure → 500, no user row |
| 21 | post-commit failure → 201, mutation audit present |

Run:

- `make ci`, documenting baseline failures;
- `go test ./pkg/hub ./pkg/store/... ./pkg/hubclient ./cmd`;
- web tests for Phase 3.

## 17. Open questions (inputs outside H)

- The final A.1/A.2 contracts: per-permission project applicability, allowed boundaries, the
  creation-scope rule, and the convention for classifying shared call sites (Phase 0).
- The shape of D.2's per-operation admission mechanism, and the credential restrictions it declares
  for user administration.
- The B.3 provenance record shape and the lapsed-source-token rule: not needed, because ptone decided
  OD-10 (a).
- The governance and `CanDelegate` contract for invitation-type effects (§7.3 item 6): resolved. D.2
  G5 ([ptone/scion#2124](https://github.com/ptone/scion/issues/2124)) provides the governance and token
  admission, and Phase 2 lands with or after it; B.3 defines nothing for this effect (§16.1).
- The sign-in item: resolved. [GoogleCloudPlatform/scion#2071](https://github.com/GoogleCloudPlatform/scion/pull/2071) ("require provider-verified email and unify sign-in policy across auth paths") has landed (§16.1).
- A.1's final route-metadata convention for method-agnostic entries such as `/api/v1/users`
  (§16.3).

## 18. Acceptance criteria (for reviewer/QA)

H.1 (this document):

- [ ] `POST /api/v1/users` changes from an unconditional 403 into `user.admin.provision`, an
  invitation-equivalent pre-registration that shares invite's creation core and validation. It is not
  an unrestricted create (§5, §13.1).
- [ ] Collision, suspended, repeated-request, privileged-role and rollback outcomes are defined with a
  status and an error code (§8, §9).
- [ ] Supporting work is bounded (§2, §14). H.2 phases, files, dependencies and tests are executable
  (§16).
- [ ] Existing sign-in and invitation behaviour is unchanged, no change is inferred from hub-token
  support, and the sign-in equivalence rule is stated (§5.6, §10).
- [ ] OD-1 is recorded as decided by ptone (2026-09-28): no stored role, the role at activation
  follows the configured policy, `role` is rejected with 422, and Option R is marked not adopted with
  its rationale (§6, §13.4, §19). H.2 has no Option R phase, rows or tests (§16). Every other open
  decision is pending ptone, and nothing else reads as approved. (H.1 state; OD-2 to OD-10 have since
  been decided, §19.)
- [ ] The lifetime of UAT-created records is an explicit open decision (OD-10) with a test for each
  option (§12, §16.4).
- [ ] Without detail authority, collision and replay responses carry no `userId`, no record `id` and no
  stored field values. The only signal beyond invite's single 409 is that a 200 replay reveals a pending
  record whose stored note and display name equal the request, accepted as the cost of idempotent
  replays (§5.4, §8).
- [ ] A contracts are referenced by concept and issue; Phase 0 binds them.
- [ ] Each H.2 start gate in §16.1 is stated with the same conditions wherever it appears.

H.2 (implementation):

- [ ] Every §8 row has an integration test (§16.4 row-to-test index), and replays are reauthorized.
- [ ] A reduced UAT is admitted only when the exact selector, live authority (an actual grant after
  scope-sensitive filters), the hub boundary, credential restrictions and invitation-effect governance
  are all satisfied.
- [ ] Provisioned and invite-created records behave identically on every sign-in path, including GE
  exchange and external bearer.
- [ ] Any Registry row change has matching `ProjectTargetApplicability` and
  `PermissionAllowedBoundaries` rows, and the drift test passes.
- [ ] Suspended identities and last-admin protections hold.
- [ ] Provisioning grants no sign-in outside configured providers and access policies, and invite
  characterization tests pass unchanged.
- [ ] Registry, catalog, route and client surfaces agree.
- [ ] ptone has confirmed the CLI mode decision.
- [ ] Go and web checks pass, or baseline failures are documented.

## 19. Open decisions (for ptone)

OD-1 is **decided** (2026-09-28). OD-2 to OD-10 are **DECIDED by ptone on 2026-10-04: option (a)
for each** ("go with recommendation, a"). The options are kept below for the record.

**Sign-in dependency (not an H decision).** Satisfied: [GoogleCloudPlatform/scion#2071](https://github.com/GoogleCloudPlatform/scion/pull/2071) ("require provider-verified email and unify sign-in policy across auth paths") has landed (§16.1). H does not change
sign-in.

**OD-1 — stored initial role (Option R). DECIDED by ptone, 2026-09-28 (relayed by pat-refactor).**
Decision: the role at activation follows the current configured policy. There is no optional
`member`/`viewer` selection before sign-in and no pending-role column. Option R is not adopted (§6,
§13.4).

**OD-2 — admin at provisioning. DECIDED by ptone (a), 2026-10-04.**
- (a) Not provisionable (422). **Recommended**; pat-refactor concurs.
- (b) Allow it with `user.promote` and CanDelegate(super-admin). This would select a role before
  sign-in, which the OD-1 decision excludes. Choosing (b) requires ptone to revisit OD-1 and a new
  design (§6).

Under either option, `admin_emails` remains independent configured authority.

**OD-3 — editing and withdrawing pending records. DECIDED by ptone (a), 2026-10-04.**
- (a) No new capability. **Recommended.**
- (b) Let `user.invite` holders delete invited records. This changes DELETE authorization and needs
  coordination with D.2.

**OD-4 — initial group and project memberships. DECIDED by ptone (a), 2026-10-04.**
- (a) Out of scope. **Recommended.**
- (b) Accept `groups[]` with per-group CanDelegate and B.3 provenance.

**OD-5 — pending expiry. DECIDED by ptone (a), 2026-10-04.**
- (a) None, matching invites. **Recommended.**
- (b) Optional `expiresAt` with a sweeper.

**OD-6 — display-name precedence at activation. DECIDED by ptone (a), 2026-10-04.**
- (a) Keep today's rule: the provider name applies on activation. **Recommended**; no sign-in change.
- (b) Keep the admin-provisioned name when one is set.

**OD-7 — idempotency. DECIDED by ptone (a), 2026-10-04.**
- (a) Natural key only. **Recommended.**
- (b) Add an `Idempotency-Key` header with stored responses.

**OD-8 — CLI mode. DECIDED by ptone (a), 2026-10-04.**
- (a) Available in human and assistant modes, absent in agent mode. **Recommended.**
- (b) Also deny it in assistant mode.

**OD-9 — stricter email policy at creation. DECIDED by ptone (a), 2026-10-04.**
- (a) Warn only, keeping invite parity. **Recommended.**
- (b) Tighten creation-time email policy for **both** invite and provision together. This changes
  invite. It covers:
  - rejecting reserved and out-of-domain emails at creation;
  - requiring a bare address: the parsed `addr.Address` must equal the normalized input, so RFC 5322
    display-name forms such as `"Bob <bob@x.com>"` are rejected instead of stored.

**OD-10 — lifetime of a pending record created through a hub UAT (Phase 2). DECIDED by ptone (a),
2026-10-04.** This covers what happens when, before first sign-in, the source token is revoked or
expires, the token owner is suspended, or the owner loses `user.invite` (§12).
- (a) **Recommended.** The record persists, which is invitation parity (session invites survive the
  inviter's demotion today). This does not rest on the invitation being inert: the record can enable
  later admission under `invite_only`, and that is a durable effect. Under (a), creation must still
  authorize the whole invitation effect for the scoped caller under the applicable governance and
  `CanDelegate` contract (§7.3 item 6). Durable provenance as defined by the D.2/B.3
  invitation-effect contract (§7.3 item 6; B.3 defines none, §16.1), with at minimum the mutation audit's credential
  attribution, plus audit and explicit withdrawal (§12), are required. Token expiry or revocation prevents new
  operations; it does not undo a completed invitation.
- (b) Tie the record to B.3 provenance, and have activation re-check the source (owner active and
  still holding `user.invite`, token not revoked or expired) before the record admits sign-in. This is
  additional B.3-linked scope on the activation paths.

Under either option, the existing provider and access-policy checks at sign-in stay mandatory, and
B.3 is coordinated for the invitation effect contract (§16.1).

## 20. Consultation and decision record

This section records the questions asked of `agent:pat-refactor` with their answers, and the
decisions ptone has made. All dates are 2026-09-28 (UTC).

### 20.1 Questions to pat-refactor

**Question 1:** any architectural objection to (a) reusing `POST /users`, or (b) excluding admin at
provisioning?

**Answer (20:58Z):** no objection to either, and both are recommended. Their further points:

- Reuse the invitation creation service and validation so the entry points cannot diverge.
- `user.invite` fits only while the effect stays invitation-equivalent.
- Replays must reauthorize, compare normalized inputs, handle races transactionally, and never
  mutate active or suspended users.
- A stored role is a separate product decision and a delayed grant; they recommend deferring it and
  are raising it with ptone.
- If a stored role is retained, it needs B.3 provenance and ceilings, pre-activation changes, atomic
  activation, consume/clear, and all sign-in paths.
- State that `admin_emails` is independent.
- Bearer admission needs exact selectors, live authority and credential restrictions, not the
  boundary check alone.
- No H.2 work until the design and its dependencies are approved.

**Question 2 (21:20Z):** confirm the recommended lifetime for UAT-created pending records (record
persists, invitation parity)?

**Answer (21:22Z):** do not record it as approved. The recommendation is reasonable, but it must not
rest on the premise that an invitation has no durable effect. An invited record can enable later
admission under `invite_only`, which is an effect. pat-refactor is raising the choice with ptone;
it stays open, with no approval by timeout. Under (a), creation still authorizes the whole invitation
effect under the applicable governance/`CanDelegate` contract; durable provenance, audit and explicit
withdrawal remain required; token expiry or revocation prevents new operations rather than undoing
the invitation. Under (b), B.3-linked source checks at activation are additional scope. B.3 is not
irrelevant just because no pending role is stored; coordinate the invitation-effect contract with
D.2/B.3. Existing provider and access-policy checks at activation remain mandatory either way.
This choice is OD-10 (§19), decided (a) by ptone on 2026-10-04.

### 20.2 Decisions by ptone

| Date | Decision | Conveyed by |
| --- | --- | --- |
| 2026-09-28 | **OD-1 decided:** the role at activation follows the current configured policy. There is no member/viewer selection before sign-in and no pending-role column (§6). | `agent:pat-refactor` |
| 2026-09-28 | Continuing [ptone/scion#2116](https://github.com/ptone/scion/issues/2116) is approved as a separate followup. It is not a core prerequisite and does not gate core UAT delivery. | `agent:pat-refactor` |
| 2026-10-04 | **OD-2 to OD-10 decided: option (a) for each** ("go with recommendation, a"). Admin is not provisionable (422 for any non-null role); no new edit/withdraw capability for pending records; no initial groups; no expiry; the provider name applies at activation; natural-key idempotency only; the CLI is available in human and assistant modes and absent in agent mode; warn-only email policy at invite parity; records created through a hub UAT persist (creation authorizes the whole invitation effect; mutation-audit credential attribution; audit and explicit withdrawal). | `agent:pat-h-lead` |

No other decision has been made.
