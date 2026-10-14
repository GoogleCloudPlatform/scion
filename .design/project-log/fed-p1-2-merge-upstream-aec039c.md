# Federated project access item 2: merge upstream main (aec039c95)

Date: 2026-10-12. Branch: scion/mmr-fed-p1-2.

## What happened
- Merged upstream main at aec039c9590b into the item 2 branch (merge forward, no rebase).
- Textual conflicts in `pkg/hub/seed.go` and `pkg/hub/seed_roles_test.go`. Upstream added agent
  delegation grant management to project-owner and project-admin and bumped them to revisions 8
  and 7; this branch had bumped the same roles to the same numbers for `project.manage_visitors`.
- Resolution: both permission sets are kept (they merged cleanly), and the roles move to
  project-owner revision 9 and project-admin revision 8. A hub that already applied upstream's
  revision 8/7 (a different permission set at the same number) records the new number when it
  reconciles, so the stored marker always names one permission set.
  The reconciler would also update on the permission-hash mismatch alone, but the convention in
  `seed.go` is one revision per permission change.
- Revision assertions updated to 9/8: `TestProjectRoleRevisions`,
  `TestProjectRoleReconciliationConverges` (`seed_roles_test.go`) and
  `TestProjectRoles_AttachAndPortAccessLockIn` (`uat_project_relationship_test.go`, which the
  textual merge did not flag). The permissions reference page states the new revisions.

## Checked for meaning-level conflicts
- Upstream's new permissions (`agent.delegation.*`) are not given to project-visitor, and upstream
  adds no code that decides project membership from role bindings, so nothing new needs the
  confers-membership filter.
- New store interface methods upstream are implemented by embedded stores; the composite store
  still satisfies `store.Store` by embedding.
