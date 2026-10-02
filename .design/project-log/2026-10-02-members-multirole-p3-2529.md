# members-multirole P3: multi-role Members editor (ptone/scion#2529)

**Branch**: `scion/members-multirole-p3`, stacked on P1+P2 (base `b456df49`).
**Scope**: frontend and docs only. No Go or backend changes. The editor uses
the P1 atomic PUT/DELETE and the P2 read endpoints as they are.

## Attribution

The custom-role parts of this editor are ported from miller79/scion PR #127
by Anthony Lofton:
- the custom-role checkbox UI;
- the `isCustomProjectRole` classification;
- the `describeCustomRoleError` message mapping.

Commits that carry ported code have the trailer
`Co-authored-by: Anthony Lofton <6901313+miller79@users.noreply.github.com>`.
These are the editor+tests commit and the docs commit. The port was adapted
to the grouped data model and the single-PUT save path. It does not use
PR #127's per-binding POST/DELETE sequence.

## What was built

- **Types** (`web/src/shared/types.ts`):
  - `ProjectMemberGroup`, `ProjectMemberBinding` and `AssignableProjectRole`;
  - `canManageCustomRoles` on the membership capabilities.
- **Editor** (`web/src/components/shared/project-members-editor.ts`):
  - **Loading.** The editor pages through
    `GET /members?groupBy=principal&limit=500`. It then loads
    `GET /members/assignable-roles` only when the editor is editable.
  - **Table.** One row per principal. The Roles cell shows the built-in role
    badge (or "No project role"), then a badge for each custom role.
  - **One dialog for Add and Edit**:
    - a radio group for the single built-in role, plus "No project role";
    - checkboxes for custom roles.
    - Each disabled option shows its inline reason, taken from the tier rules
      and the server's `grantable`/`reason`.
  - **Existing member in Add mode.** Picking a principal who is already a member switches the dialog
    to Edit and prefills it. Nothing is sent.
  - **Save.** Exactly one `PUT /members/principals/{type}/{id}`, with
    `{roleDefinitionIds, expectedRoleDefinitionIds}`.
    - An empty selection is not saved. The dialog shows a warning and offers
      "Remove member", which sends a DELETE after confirmation.
  - **Error mapping**:
    - 409 `membership_changed` with cause `principal_roles_changed`: reload,
      reopen in Edit, and show "changed while you were editing".
    - 409 `membership_changed` with cause `actor_authority_changed`: reload
      and show the authority-changed message.
    - 400 `invalid_role_set`: reload the role list, because it is stale.
    - 403 ceiling or forbidden: shown in the dialog through
      `describeCustomRoleError`.
  - **Admin tier rules** (admins manage the member tier only):
    - Owner and Admin options are disabled, and the owner and admin rows have
      no actions.
    - Custom roles are read-only. A member who holds custom roles cannot be
      row-removed by an admin.
    - When an admin changes a member's built-in role, the member's custom
      roles are kept in the PUT.
  - **Last direct owner.** The only direct owner cannot be demoted or
    removed. The row shows a single tooltip that explains why.
  - **Agents** are offered built-in roles only. Any custom roles they already
    hold show as badges.
  - **Transfer Ownership** is unchanged.
- **Tests** (`project-members-editor.test.ts`): 49 vitest cases.
  - They cover the pure helpers and component behaviour: paging, read-only
    and hub-override modes, defaults, eligibility, tiers, save payloads, the
    switch from Add to Edit for an existing member,
    both 409 causes, `invalid_role_set`, delete and transfer.
  - A deliberate mutation confirmed that the tests catch regressions.
- **Docs** (`docs-site/.../hosted/ha/permissions.md`): Role & Binding
  Management now describes the custom roles in the Members editor.

## Deviations from the spec, with reasons

- **assignable-roles is fetched after the list, not in parallel.** The editor
  only knows from the list's `_capabilities` whether the viewer can edit.
  A read-only viewer would otherwise get a 403 on every page load.
- **"Edit instead" link replaced by an automatic switch to Edit.** Picking an existing
  member opens Edit directly. Typed emails that do not resolve client-side
  reach the same state through the 409 `principal_roles_changed` path.
- **Errors are parsed with the existing `parseApiError`**, not a new
  `extractApiError`. It already exposes `code` and `details`.
- **"Remove member" from an empty selection** is enabled only when the
  viewer may remove that row. When the viewer may not remove that row, the
  button is shown disabled with the reason.

## Validation

- `npx vitest run` on the editor test file: 49/49.
- `npm run typecheck`: clean.
- `npm run build`: OK.
- `npm run lint`: 0 errors in the touched sources. Repo-wide lint was
  already red on the base.
- **Manual validation**:
  - A local hub with test-login and five real users, all with hub role
    member.
  - Two custom roles: one within an owner's delegation ceiling and one
    beyond it.
  - The real UI, driven in Chromium, as the owner and as an admin.
  - Covered: add, edit, custom grant, ceiling refusal, the switch from Add
    to Edit for an existing member, last-owner lock,
    admin tier rules, and remove-all.
  - Validated manually; the transcript is kept outside the repo.
  - The ceiling and admin-custom refusals can only be reached by
    force-enabling a disabled control, because the UI pre-disables them.

## Adjacent cleanup (not done)

- The `project-settings.ts` Members section description still says
  "Adding a member creates a project-scoped role binding".
- **Repo-wide `npm run lint` is red.**
  - Every `*.test.ts` file fails with a `parserOptions.project` error,
    because tsconfig excludes tests.
  - The fix is a `tsconfig.eslint.json` that includes tests.
- **The Source column is now always "Direct".** The grouped endpoint returns
  only direct project bindings, so the column could be dropped or repurposed.
- **No per-role reasons for disabled custom roles in an admin's dialog.**
  The single caption covers them.
