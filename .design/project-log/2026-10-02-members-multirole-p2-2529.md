# members-multirole P2: backend read surface (ptone/scion#2529)

**Branch**: `scion/members-multirole-p2`, stacked on the P1 branch
(`scion/members-multirole-p1`).
**Scope**: the read endpoints the multi-role members UI needs. Nothing here
writes, and no new authority path is added.

## What was built

- **Grouped members GET**: `GET /api/v1/projects/{id}/members?groupBy=principal`
  (`handlers_project_members.go`, `writeProjectMemberGroups`).
  - Returns one item per principal, in the same `projectMemberGroup` shape
    the P1 PUT returns: principal, highest built-in role, and every project
    binding ordered built-in first, then by role name.
  - Pagination is by principal (default limit 100), so a principal is never
    split across pages. `totalCount` counts principals.
  - Order is deterministic: highest built-in tier (owner, admin, member,
    then custom-only), then case-insensitive display name, then exact
    display name, then principal id and type.
  - With no `groupBy`, the flat list is unchanged. Any other `groupBy` value
    is a 400.
  - `changed` moved off `projectMemberGroup` into a PUT-only wrapper
    (`projectMemberGroupMutationResponse`, embedded), so GET items do not
    carry a meaningless `changed:false`. The PUT JSON is identical.
  - The binding sort is now shared by the P1 PUT/DELETE response and the
    grouped GET (`sortProjectMemberBindings`). It adds a binding-id
    tiebreak: the old sort was non-stable on built-in, then name, so two
    same-named custom roles could swap places between responses.
- **Assignable roles**: `GET /api/v1/projects/{id}/members/assignable-roles`
  (`handleProjectAssignableRoles`, routed in `handlers_projects_core.go`;
  service in `project_membership_assignable.go`).
  - Gated by `project.manage`.
  - Lists every project-scoped role definition (built-ins in tier order,
    then custom roles by name). System roles are never listed.
  - Each item has `{id, name, description, roleKind, grantable, reason}`.
  - `grantable` replays the PUT's pre-transaction checks for creating that
    role, using the same helpers in the same order, so the first refusal
    reported is the one the PUT would give:
    1. credential gate;
    2. `checkNoRoleBindingPermissionInCreatedCustomRoles`;
    3. actor has a project role, or hub override;
    4. `governanceDecisionForChange` with op=add, where custom roles get
       their authority from `customRoleAuthorityFromStore(role_binding.create)`;
    5. `CanDelegate`.
- **`MembershipCapabilities.canManageCustomRoles`**: an additive field on
  the `_capabilities` of both members GETs, computed by
  `customRoleAuthorityFromStore(..., role_binding.create)`. It fails closed
  (and logs) on a store error.

## Deviations, with reasons

- **assignable-roles also applies the PUT's credential gate.** A UAT or
  agent credential sees every role as not grantable, with
  `credential_insufficient`. Without this, `grantable=true` would be a
  promise the PUT then breaks.
- **Additive `denialCode` and `details` on refused items.** These mirror the
  PUT's error code and details (for example `details.requiredPermission` on
  a custom-role authority refusal, and `details.roleDefinitionId` on
  ceiling and structural refusals), so the UI keys off structured fields
  rather than reason text. Both are omitted when the role is grantable.
- **The decision is principal-agnostic, for op=add.** `grantable`
  answers "may newly grant this role". For an existing member's built-in
  change, the PUT can differ in both directions:
  - it also governs the old role, so an admin cannot move an owner-held
    principal down to member even though `member` shows grantable;
  - it skips CanDelegate on a decrease, so the hub-override owner→member
    demotion is accepted even though `member` shows not grantable.

  That decision depends on the target principal, so this view does not
  model it. Principal-type eligibility (owner is user-only, and so on) is
  also left to the client.
- **A plain hub admin gets 403** from assignable-roles. This is the same
  project.manage gate as every other members endpoint, because hub-admin
  does not hold project.manage. The hub-override answer is reachable over
  HTTP only for an actor with no built-in project role whose custom role
  carries project.manage. Both cases are tested, and the service result for
  a plain hub admin is tested directly.

## Tests

- **`project_members_grouped_test.go`**:
  - a principal with three bindings appears once;
  - 150 + 3 principals with limit 100 never split a principal, and
    totalCount counts principals;
  - deterministic order, including display-name ties broken by id;
  - the flat list is byte-compatible with the legacy item, plus `roleKind`;
  - invalid `groupBy` is rejected;
  - a member can read the list, with capabilities.
- **`project_members_assignable_test.go`**:
  - **owner**: built-ins and within-ceiling custom roles are grantable; a
    beyond-ceiling role is refused with the ceiling reason; a
    role_binding.*-bearing role is refused.
  - **admin**: only member is grantable; custom roles are refused on
    `requiredPermission`, including when the admin also holds hub
    role_binding.*.
  - **other actors**: a member gets 403; a plain hub admin gets 403, and
    its service-level result is checked; the hub override works over HTTP;
    the credential gate refuses everything.
  - **endpoint properties**: system roles are never listed, and order is
    asserted; the endpoint is read-only (bindings and audit rows unchanged);
    non-GET is a 405.
  - **capabilities** (service and HTTP): `canManageCustomRoles` is true only
    for a direct owner or the no-project-role hub fallback.
  - **consistency table** across owner, admin and hub-override actors and
    every fixture role: each grantable role is accepted by a P1 PUT by the
    same actor on a fresh principal; each non-grantable role is refused
    with the same code and reason, writing nothing.

## Validation

Exercised against a local hub (SQLite, dev auth) with curl:

- grouped GET pages;
- flat GET with `roleKind`;
- assignable-roles as owner, admin and member, each cross-checked against
  the P1 PUT by the same actor;
- the capabilities field.

The dev identity also holds system super-admin. That widens its delegation
ceiling and lets it past project.manage gates, so the beyond-ceiling and
member-403 cases are covered by the unit tests above rather than live. The
transcript is held with the review artifacts, not in this file.

## Gates run

- `gofmt -l pkg/hub/`: clean.
- `go vet -buildvcs=false ./pkg/hub/`: pass.
- `go test -p 2 ./pkg/hub/ -run '<new>|SetMemberRoles|ProjectMember|RS|D002|PM1|Catalog|Classif|AST'`
  with `SCION_PROJECT` unset: pass.
- `go test ./pkg/hub/authzop/`: pass.
- `golangci-lint run --new-from-rev=<P1 head> ./pkg/hub/...`: 0 issues. The
  clone is shallow, has no `upstream-main` ref, and has no merge base with
  the fork's main, so the P1 head was used as the base. That isolates
  exactly this change.
- `go build -buildvcs=false ./...`: pass.
- Not run, per the task's resource limits: `make ci` and the full
  `make test-hub-sqlite`.

## Adjacent cleanup noticed, not implemented

- **The hub-override entry gate.** Every members endpoint is gated on
  `project.manage`, which hub-admin lacks. As a result, the hub-override
  branches in membership governance are reachable over these endpoints only
  by an unusual actor: one with no built-in role but a custom role that
  carries project.manage. Either hub-admin should get project.manage on the
  members routes, or the override should be documented as reachable only
  through `/admin/role-bindings`.
- **Share Phase P instead of mirroring it.** assignable-roles mirrors
  SetMemberRoles Phase P helper by helper. Factoring Phase P into a
  per-created-role function that both call would remove the risk of the
  two drifting apart. Today the consistency table test is what guards
  against drift.
- **Duplicate role-name list.** `validProjectRoles` in the members handler
  duplicates `store.BuiltInProjectMembershipRoles`.

## Review round 1 fixes

All seven deviations were accepted. These fixes close the Medium finding
and every Low and Nit finding. The deviation bullet on op=add above was
reworded under R1-8.

- **R1-1 (Medium):** `TestAssignableRoles_ConsistentWithPut` has a
  fourth actor. It holds only a custom `{project.read, project.manage}`
  role, with no built-in project role and no hub `role_binding.*`, and the
  PUT refuses it with "actor has no project role".
  - `asgHubOverrideActor` is now a thin wrapper over
    `asgManagerActor(…, hubAdmin bool)`.
  - `TestAssignableRoles_CustomManagerWithNoProjectRole` pins that actor's
    decisions directly.
  - In a throwaway detached worktree, two mutations now fail both tests.
    One drops the no-project-role check (M1). The other moves it above the
    structural `role_binding.*` check (M2). Both previously survived.
- **R1-2:** `noProjectRoleDecision` and `canDelegateRefusal` in
  `project_membership_set.go` are the single constructors for those two
  refusals, in SetMemberRoles Phase P and in AssignableRoles. Phase P was
  not otherwise refactored.
- **R1-3:** `buildProjectMemberGroup` is the one group builder for the
  grouped GET and the PUT principal endpoint.
  - It takes a per-request `projectMemberEnricher` cache of role names and
    display names.
  - It picks the group's built-in role by `projectRoleLevel`, with the
    highest winning.
  - `projectMemberGroupTier` is gone; the grouped sort compares
    `projectRoleLevel` in descending order.
  - The PUT JSON is unchanged: the P1 tests pass unmodified.
  - `TestProjectMembersGrouped_MatchesPutResponseGroup` asserts that both
    endpoints render the same group.
- **R1-4:** the grouped page end is computed without `offset+limit`, so a
  huge `limit` no longer overflows. Covered by
  `TestProjectMembersGrouped_HugeLimitDoesNotOverflow`, which fails against
  the old computation.
- **R1-5:** `TestAssignableRoles_RoutingKeepsMemberAddressing` pins the
  routing:
  - a group whose slug is literally `assignable-roles`, and
    `members/principals/user/assignable-roles`, both still reach the
    principal handler;
  - ID addressing still works;
  - PATCH and DELETE on `members/assignable-roles` return 405
    (`Allow: GET`) and write nothing;
  - `members/assignable-roles/x` and real binding IDs still reach the
    binding-ID handler.
- **R1-6:** the consistency table compares `details` with the PUT's error
  details, after JSON-normalising both.
- **R1-7:** a non-user identity, such as an agent token, gets
  `403 credential_insufficient` with the PUT's message.
  - The check now runs before `authorize(project.manage)`, in the same
    position as on the PUT. Otherwise an agent would never reach it.
  - Covered by `TestAssignableRoles_AgentTokenGetsCredentialInsufficient`.
- **R1-8:** the op=add deviation text above was reworded.

### Gates run (round 1)

All of these were run against the round-1 head:

- `gofmt -l pkg/hub/`: clean.
- `go vet -buildvcs=false ./pkg/hub/`: pass.
- The targeted `go test -p 2 ./pkg/hub/ -run 'Assignable|Grouped|Capabilit|SetMemberRoles|ProjectMember|RS|D002|PM1|Catalog|Classif|AST|Rout'`,
  with `SCION_PROJECT` unset: pass.
- `go test ./pkg/hub/authzop/`: pass.
- `golangci-lint --new-from-rev=<P1 head> ./pkg/hub/...`: 0 issues.
- `go build -buildvcs=false ./...`: pass.

Not run, per the task's resource limits: `make ci` and the full
`make test-hub-sqlite`.

### Adjacent issues noticed in round 1, not fixed

- **Bad user principal ID returns 500.** A members PUT to
  `principals/user/<id>`, where the ID is neither a UUID nor an email,
  gets 500 `internal_error`, because the store rejects the principal_id.
  It should be a 4xx. This is pre-existing P1 behaviour, and the routing
  test does not pin the status.
- **Flat-list pagination overflow.** The flat members list still computes
  `offset+limit` and can overflow on a huge `limit`. This is pre-existing,
  and the same fix applies.
- **Remaining copies of the refusal strings.** `project_membership_service.go`
  still builds its own "actor has no project role" and CanDelegate refusals
  for the legacy POST/PATCH member paths. They could use the new shared
  constructors.
- **Flat-list enrichment.** The flat list still enriches bindings by hand
  rather than through `projectMemberEnricher`.

## Review round 2 fixes

Round 2 approved the round-1 head with one Low and one Nit finding. Both
are fixed, along with the two adjacent issues noted in round 1 (bad user
principal ID, flat-list overflow). The PUT/DELETE success JSON is
unchanged.

- **R2-1 (Low):** `TestProjectMembersGrouped_BindingsCarryFlatListEnrichment`
  checks every binding in every grouped item, and every binding in a PUT
  response, with `JSONEq` against the flat-list item that has the same
  `id`. The flat list is pinned to the legacy shape separately, so this
  covers the shared builder's per-binding `principalDisplayName` and
  `createdByDisplayName` for both endpoints. In a throwaway detached
  worktree, deleting either field from the builder now fails this test.
  Both mutants survived before.
- **R2-2 (Nit):** the builder is now `projectMemberEnricher.group`, so its
  real dependency is explicit. The unused `*Server` receiver is gone, and
  callers no longer pass the server twice.
- **L-500, a correction to P1 behaviour:** a PUT or DELETE to
  `members/principals/user/{id}`, where `{id}` is neither an email nor a
  UUID, used to reach the store. The store's principal_id validation then
  produced the wrong status:
  - PUT returned 500 `internal_error`. It now returns 400 `invalid_request`.
  - DELETE returned 404 `not_found` ("principal has no bindings"). It now
    returns 400 `invalid_request`.

  `invalid_request` is the code P1 already uses for unresolvable principal
  addressing. Well-formed addressing is unchanged: a UUID with no bindings
  still gets DELETE's 404, and an unknown email still gets PUT's 400.
  Covered by `TestSetMemberRoles_MalformedUserPrincipalID400`.
- **L-OVF:** the flat members list now computes its page end without
  `offset+limit`, the same way the grouped list does. Before, a `limit`
  near `math.MaxInt` overflowed and the request failed with 500. Covered by
  `TestProjectMembersFlat_HugeLimitDoesNotOverflow`.

In the throwaway worktree, reverting either the L-500 fix or the L-OVF fix
makes its new test fail.

### Gates run (round 2)

All of these were run against the round-2 code head:

- `gofmt -l pkg/hub/`: clean.
- `go vet -buildvcs=false ./pkg/hub/...`: pass.
- The targeted `go test -p 2 ./pkg/hub/ -run 'Assignable|Grouped|Capabilit|SetMemberRoles|ProjectMember|RS|D002|PM1|Catalog|Classif|AST|Rout|Principal'`,
  with `SCION_PROJECT` unset: pass.
- `go test ./pkg/hub/authzop/`: pass.
- `golangci-lint --new-from-rev=<P1 head> ./pkg/hub/...`: 0 issues.
- `go build -buildvcs=false ./...`: pass.

Not run, per the task's resource limits: `make ci` and the full
`make test-hub-sqlite`.

### Adjacent issues noticed in round 2, not fixed

- **Unknown well-formed user UUID returns 500 on PUT.** A PUT to
  `principals/user/<uuid>` for a user who does not exist gets 500
  `internal_error` ("not found: user …"). The store returns `ErrNotFound`
  from inside SetMemberRoles, after address resolution. It should be the
  same 400 as an unknown email. This is pre-existing P1 behaviour.
- **Malformed agent principal IDs.** The store parses agent principal IDs
  as UUIDs too. `principals/agent/not-a-uuid` gets 500 `internal_error` on
  PUT and 404 `not_found` on DELETE, checked against the round-2 head.
  L-500 was scoped to user principals only. Extending
  `validateMemberPrincipalAddress` to agents would be a one-line change.

## P1 corrections: principal addressing

Four fixes correct how P1's `PUT`/`DELETE
…/members/principals/{type}/{id}` handle a principal address. The first
three cover an address that does not resolve: each used to fall through to
the store and come back with the wrong status, and each fix uses
`invalid_request`, the code P1 already uses for an unknown email. The fourth
covers an address that resolves but is not spelled canonically.

| Input | PUT before → after | DELETE before → after | Fix |
|---|---|---|---|
| `user/not-a-uuid` | 500 `internal_error` → 400 `invalid_request` | 404 `not_found` → 400 `invalid_request` | L-500 (round 2) |
| `agent/not-a-uuid` | 500 `internal_error` → 400 `invalid_request` | 404 `not_found` → 400 `invalid_request` | malformed agent ID |
| `user/<unknown UUID>` | 500 `internal_error` → 400 `invalid_request` | 404 `not_found` (unchanged) | nonexistent principal |
| `agent/<unknown UUID>` | 500 `internal_error` → 400 `invalid_request` | 404 `not_found` (unchanged) | nonexistent principal |
| `user/<UPPER-CASE UUID>`, `urn:uuid:…`, `{…}`, undashed (same for `agent/`) | 201, binding stored under the raw string → binding stored under the canonical ID | 404 for a canonical binding, which the raw string never matched → removes the canonical binding | canonical UUID addressing |

- **L-500** (round 2, above) made `validateMemberPrincipalAddress` reject a
  user principal that is neither an email nor a UUID.
- **Malformed agent ID:** `validateMemberPrincipalAddress` now also rejects
  an agent principal that is not a UUID. Agents have no email form, so an
  `@` address is malformed too. Covered by
  `TestSetMemberRoles_MalformedAgentPrincipalID400`.
- **Nonexistent user or agent:** the binding create inside the transaction
  checks that the principal exists. The store reports a missing principal
  as `ErrNotFound`, and that error became a 500. `SetMemberRoles` now looks
  up the addressed user or agent as the last Phase P step, and only when
  the plan creates a binding. If the record is not found, the PUT returns
  400 `invalid_request` ("user not found: …" or "agent not found: …").
  - **Only the not-found is mapped.** Any other error from that lookup is
    still a 500. This was not done by mapping the transaction's error: the
    entadapter's existence probe turns *every* `Get` failure into
    `ErrNotFound`, so a mapping there could not tell a missing user from a
    database fault.
  - **Why the check is last in Phase P and not in the handler:** the P1
    eligibility tests address agents that do not exist and pin 400
    `principal_ineligible`. A check in the handler would have changed
    their code. Placed last, every earlier refusal keeps its code: precondition,
    eligibility, governance, CanDelegate, and the in-transaction
    deleted-role-definition 400 `invalid_role_set`.
  - **DELETE keeps its 404** "principal has no bindings in this project",
    which is pinned in a test.
  - Covered by `TestSetMemberRoles_NonexistentPrincipalID`,
    `TestSetMemberRoles_ExistingAgentPrincipalStillAddressable` and
    `TestSetMemberRoles_PrincipalLookupStoreErrorIs500`.

- **Canonical UUID addressing:** `uuid.Parse` accepts upper-case,
  `urn:uuid:`, braced and undashed spellings, and the handlers used to pass
  the raw path segment on. The binding was then stored under a non-canonical
  `principal_id`. It granted nothing, slipped past the one-built-in check
  (which compares the stored string), and a `DELETE` by the canonical ID
  left it behind. `canonicalMemberPrincipalID` now returns `uuid.String()`
  for a user or agent ID, and both handlers continue with that value. Emails
  and group addresses are unchanged. These spellings are canonicalised, not
  rejected. Covered by
  `TestSetMemberRoles_NonCanonicalPrincipalIDIsCanonicalised` (user and
  agent: PUT by every spelling returns the canonical `principalId` and
  leaves one binding under the canonical ID; `DELETE` by the upper-case form
  removes it).
- **Existence-check placement is pinned.**
  `TestSetMemberRoles_PrincipalExistenceCheckedAfterAuthorization`: an
  admin granting `project-owner` to a nonexistent user gets 403
  `target_role_protected`, and a beyond-ceiling custom role gets 403
  `role_assignment_forbidden`, the same refusals as for an existing user.
  So the check reveals that a principal is missing only to an actor who may
  make the grant. `TestSetMemberRoles_OrphanedPrincipalRemovalOnlyPUT`:
  deleting an agent record leaves its bindings, and a removal-only PUT
  prunes them (200) instead of refusing with "agent not found".

Revert proof, run in a throwaway detached worktree:
- Reverting the agent-ID validation fails its test on PUT (500) and DELETE
  (404) at its own commit. At the final head it fails on DELETE only.
- Removing the existence check fails `TestSetMemberRoles_NonexistentPrincipalID`
  (PUT 500).
- A mutation that maps every lookup error to 400 fails
  `TestSetMemberRoles_PrincipalLookupStoreErrorIs500`.
- Discarding the canonical ID in both handlers fails
  `TestSetMemberRoles_NonCanonicalPrincipalIDIsCanonicalised`.
- Moving the existence check before the project-role lookup, ahead of
  governance and CanDelegate, fails
  `TestSetMemberRoles_PrincipalExistenceCheckedAfterAuthorization`.
- Dropping the "plan creates a binding" guard on the existence check fails
  `TestSetMemberRoles_OrphanedPrincipalRemovalOnlyPUT`.

Residual: a user or agent that was deleted while still holding bindings
can still be removed with DELETE, and with a PUT that only removes roles
(the latter pinned for an agent by
`TestSetMemberRoles_OrphanedPrincipalRemovalOnlyPUT`).
A PUT that adds a binding for it now gets 400 instead of 500.

### Gates run (principal addressing)

- The mandated `pkg/hub` subset (`Assignable|Grouped|…|Flat`, plus
  `Refetch` after the canonical-addressing round) and
  `pkg/hub/authzop`: pass.
- gofmt and `go vet ./pkg/hub/`: clean. `go build -buildvcs=false ./...`:
  ok.
- `golangci-lint --new-from-rev=<P1 base> ./pkg/hub/...`: 0 issues.

Not run, per the task's resource limits: `make ci` and the full
`make test-hub-sqlite`.
