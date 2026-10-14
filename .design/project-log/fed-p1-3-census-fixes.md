# Federated project access item 3A: upstream census tests

Date: 2026-10-14. Branch: scion/mmr-fed-p1-3 (from 0354075a2).

## What happened
- Fork CI on item 3A failed three upstream census tests in pkg/hub. Each one scans pkg/hub
  source, and each one found a site that item 3A added.
- `TestBypassCensus` flagged the `IsUnscopedLocalPlatformAdmin(actor)` shortcut in
  `actorHoldsVisitorPermissions` (handlers_project_visitors.go). Fixed: the check now asks
  `CanDelegate` whether the actor may bind the project-visitor role at the project's scope.
  `CanDelegate` applies the same administrator rule and the same permission comparison
  (`actorHoldsAllPermissions`), so the outcome is unchanged for every actor the visitor routes
  admit. The descriptor names no grantee, because `CanDelegate` refuses a federated grantee on
  the paths it gates. The CanDelegate wiring list in authz_candelegate.go names the new caller.
- `TestRS1_AST_BypassPathsDocumented` flagged `tx.DeleteRoleBinding` in
  `handleRemoveProjectVisitor`. Not fixed yet; the lead decides. The existing removal path
  (`ProjectMembershipService.RemoveMember`) would change behaviour: its governance matrix lets
  a project admin remove only project-member bindings, and it writes a `project_member_remove`
  audit record. The operation catalog entry `project.visitors.remove` says project admins revoke
  any visitor grant, with a `project_visitor_revoke` record written in the same transaction.
- `TestTestAdminAuthorityReadCensus` flagged two unclassified authority reads: item 3A's new
  store method `ListVisitorRoleBindings` returns role bindings, so the census picks it up by
  reflection. The two call sites are `handleListProjectVisitors` and `projectVisitorBinding`.
  Any store read that returns role bindings is in the census, under any name. The only fix is
  to classify the two sites. That is a table change, so it waits for the lead.

## Learnings
- `CanDelegate` refuses a federated grantee ("the visitor grant path, not this one"). A
  visitor-path caller that only needs the permission comparison should leave
  `GranteeType`/`GranteeID` empty.
- The test-admin census works out its read family from the `store.Store` method set
  (names and return types). A new store read that returns `*RoleBinding` always needs a census
  classification at every pkg/hub call site outside the authorization service.
