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
- **The decision is principal-agnostic, for op=add.** The PUT governs a
  built-in change on an existing member as op=update of both the old and
  new role, and runs CanDelegate only on an increase. That decision depends
  on the target principal, so this view does not model it. The decision is
  the same, but the wording differs ("cannot update" rather than "cannot
  add"). Principal-type eligibility (owner is user-only, and so on) is also
  left to the client.
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
