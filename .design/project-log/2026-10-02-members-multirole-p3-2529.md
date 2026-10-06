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
    - a radio group for the single built-in role, plus "None";
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

## Review round 1 fixes

Round 1 raised four Low and three Nit findings. All seven were fixed in
new commits on top of the branch. History was not rewritten.

- **Late picker events (R1-1).** `onPrincipalChange` now checks for Add mode
  before it assigns anything. A debounced picker event that arrives after
  the dialog has switched to Edit can no longer retarget the PUT.
- **Automatic switch to Edit follows the row rules (R1-2).** Add mode can
  land on an existing member: through the picker, or through 409
  `principal_roles_changed` after save. The dialog then applies the same
  tier rule as the row pencil.
  - When the actor may not edit that member, the dialog shows "Only project
    owners can change this member's roles." Every role control, Remove
    member and Save are disabled.
  - For a principal not in the loaded rows (typed by email), the tier comes
    from the built-in role in `currentRoleDefinitionIds`. When a role ID is
    not in the catalog, the tier fails closed to owner.
  - Last-owner status on that path is computed from the reloaded rows, not
    forced to false.
- **Labels (R1-3).** Test names, comments and this log now describe the
  behaviour instead of citing design-document labels.
- **Catalog failure and group URLs (R1-4).** New tests cover:
  - an assignable-roles failure: the error is shown, the catalog is
    cleared rather than left stale, and held roles stay visible and are
    kept in the save;
  - group principals: Add, Edit and row delete all use
    `/members/principals/group/<encoded id>`.

  The first test found a gap: when the catalog failed to load, the held
  built-in role disappeared from the dialog, although the PUT still kept
  it. The dialog now shows it, as it already did for held custom roles.
- **Row-delete confirmation (R1-5)** now ends with "This removes all of
  their project roles.", the same as the dialog's Remove member.
- **Trash condition (R1-6)** simplified to `editable`. The removed clause
  could never add a case.
- **Deviation 4 wording (R1-7)** corrected above: Remove member is shown
  disabled with the reason.

Each new test was checked against the unfixed code, or against a
targeted mutant where the fix was test-only, and failed there. The editor
suite now has 60 cases (60/60). `npm run typecheck` and `npm run build`
pass. eslint reports 0 errors in the editor source.

## Review round 2 fixes

Round 2 raised two Low and three Nit findings. All five were fixed in new
commits on top of the branch. History was not rewritten.

- **One helper for the dialog lock.** `deriveDialogLock` now computes the
  Edit dialog's lock and last-owner state. Opening Edit from a row, opening
  Edit from role IDs, and the authority-changed path all use it. A loaded
  row follows the row pencil rule. A principal known only by role IDs takes
  its tier from the built-in role among them.
- **The actor's own authority changes mid-save.** After a 409
  `actor_authority_changed`, the editor reloads and then re-derives the
  dialog:
  - If the editor is now read-only, the dialog closes and the message is
    shown as feedback.
  - In Edit mode, the lock and last-owner state are recomputed. A dialog
    that is now locked goes back to the principal's current roles and says
    nothing was saved.
  - Otherwise, newly chosen custom roles the actor can no longer grant are
    dropped, and held ones are kept. A built-in choice whose option is now
    disabled goes back to the current role in Edit mode, or to the Add
    default in Add mode.
- **Role IDs that can't be classified.** This applies when Add reaches an
  existing member by email through the 409 path, and either the role
  catalog failed to load, or a role ID is unknown and no built-in role is
  identified. The dialog no longer sorts the IDs into built-in and custom
  roles, and it no longer infers last-owner status. It is locked for every
  actor with "Couldn't load the list of roles; close and try again.". The
  received IDs stay the expected set, and no checkbox is labelled with a
  raw role ID. IDs that previously fell back to the owner tier now lock
  the dialog instead.
- **Locked info text.** While the dialog is locked, its info text only
  says "<name> is already a member." (or "This member changed while you
  were editing."). It no longer asks the user to edit or save.
- **Lock guards under forced state.** A new test locks the dialog for an
  actor with custom-role authority. It checks that the checkboxes, Save and
  Remove member are disabled, and that removing from the dialog sends no
  request. The `invalid_role_set` reset of a built-in role that left the
  catalog has no test: built-in role IDs never leave the catalog, so that
  branch can't be reached today.
- **This log** now names the radio "None", its label in the dialog.

Each new behaviour test failed against the unfixed code. The forced-state
test covers guards that already existed, so it was checked against four
mutants that each remove one guard; it failed on all four. The editor
suite now has 67 cases (67/67). `npm run typecheck` and `npm run build`
pass. eslint reports 0 errors in the editor source.

## Review round 3 fixes

- **Owner-tier fallback wording.** The round 2 entry above no longer says
  the unknown-ID rule fails closed to the owner tier: such IDs now lock the
  dialog instead. The `tierFromRoleIds` doc comment now says callers check
  `roleIdsUnclassifiable` first and the owner fallback is only a backstop.
  The fallback itself stays.
- **Authority change with a failed catalog reload.** Two mounted tests
  cover the re-derive after `actor_authority_changed` when the
  assignable-roles reload returns 500. In Add mode, an owner's Admin choice
  is reset to None once the actor is an admin, because Admin is no longer
  listed and has no radio; Save is disabled. In a row-based Edit dialog
  (Erin), the loaded row still decides the lock, so the dialog stays
  unlocked with the held custom role kept, rather than locking because the
  role IDs can no longer be classified. No production code changed.

Each new test was checked against a mutant of the re-derive. Keeping a
built-in that the refreshed catalog no longer lists fails both tests;
locking on role IDs instead of the loaded row fails the Edit test. The
editor suite now has 69 cases (69/69). `npm run typecheck` and
`npm run build` pass. eslint reports 0 errors in the editor source (the
test file hits the existing `parserOptions.project` parse error shared by
every `*.test.ts` file).
