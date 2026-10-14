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

## Follow-up: the remaining two census tests and the conduit re-check
- Revoke now goes through a new `ProjectMembershipService.RemoveVisitor`
  (project_membership_service.go), so handlers_project_visitors.go has no direct
  `DeleteRoleBinding` call and `TestRS1_AST_BypassPathsDocumented` needs no exemption.
  `RemoveVisitor` keeps who may revoke unchanged (the route still admits holders of
  `project.manage_visitors`), writes `project_visitor_revoke` with the same fields in the same
  transaction as the delete, refuses any binding that is not a project-visitor binding of the
  project (`store.ErrVisitorBindingRefused`), and takes no project membership lock and enqueues
  no membership loss check, since a visitor binding confers no membership. The authzop mutation
  classification row moved to `RemoveVisitor`.
- The two `ListVisitorRoleBindings` sites are classified in `testAdminAuthorityReadCensus`
  (display, and mutation input).
- Revoke, and a window change that narrows a grant (a later or added start, or an earlier or
  added expiry), publish `publishConduitAuthzChanged` for the visitor on the project after the
  change commits, as the generic role-binding delete does. A widened window, a refused request
  and a rolled-back transaction publish nothing. Visitors are still refused on every route, so
  no visitor stream exists yet; the event keeps the path correct once per-route visitor
  admission lands.

## Learnings (follow-up)
- The membership service holds its own store reference. A test that injects a store fault on
  `srv.store` also has to install it on `srv.membershipService.store` to cover a service path.
