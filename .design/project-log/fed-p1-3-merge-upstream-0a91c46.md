# Federated project access item 3A: merge upstream main (0a91c4640)

Date: 2026-10-13. Branch: scion/mmr-fed-p1-3.

## What happened
- Merged upstream main at 0a91c4640189 into the item 3A branch (merge forward, no rebase).
  Item 2 reached upstream as one squashed commit (GoogleCloudPlatform/scion#3537), so the
  merge base (aec039c95) predates item 2 on both sides and git reported 12 conflicted files.
- For every conflicted file, upstream's copy of the item 2 text is byte-identical to the item 2
  head this branch was built on (dbae349ed). Each file was therefore resolved as a three-way
  merge with dbae349ed as the base: upstream's version plus this branch's own changes.
  - Six files upstream did not change after item 2 (`authzop/catalog.go`,
    `federation_visitor.go`, `federation_visitor_test.go`,
    `project_visitor_membership_test.go`, `entadapter/composite_federated.go`,
    `entadapter/role_store.go`) take this branch's version.
  - `auth.go`, `permissions/registry.go`, `store/store.go` and the permissions reference page
    merge without overlap.
  - `authzop_sqlite_helpers_test.go`: upstream turns on `hub.chat_thread_groups` and this
    branch turns on `federation.project_visitors` in the same loop; both are kept.
  - The operation catalog report keeps every section from its side and states 222 operations
    (upstream 218 plus this branch's four `project.visitors.*` operations), in `Catalog` order.
- Two removals this branch made to item 2 lines were reapplied by hand, because the textual
  merge restored them without a conflict: the `project.manage_visitors` entries in
  `TestRegisteredPermissionsConsumed` (`authzop/catalog_test.go`) and in `pendingNonRouteRows`
  (`permission_helpers_test.go`). The visitor routes now check that permission.

## Checked
- `git diff upstream-main HEAD` matches `git diff dbae349ed 0bb0de6eb` file by file, except
  the catalog count line and the shared experiment loop above.
- Upstream did not change `seed.go`; the hub-admin revision 8 from this branch stands.
- New hubclient and store interface methods have no new implementers upstream that lack this
  branch's methods.
- 2026-10-14: merged upstream main again at 5d15cbd8fadd. Only the operation catalog report
  (count 223: upstream 219 plus the four `project.visitors.*` operations) and the shared
  experiment loop in `authzop_sqlite_helpers_test.go` (upstream's text, which already enables
  `federation.project_visitors`) needed a hand resolution.
- 2026-10-14: merged upstream main again at 9151dbb1b004. The operation catalog report (count
  280: upstream 276 plus the four `project.visitors.*` operations) and one `opPatternOverrides`
  row in `authzop_sqlite_helpers_test.go` (the `project.visitors.update` row, kept after
  upstream's realigned block) needed a hand resolution.
- 2026-10-14: merged upstream main again at c33bfa6384e8. The operation catalog report count
  (293: upstream 289 plus the four `project.visitors.*` operations) and one `opPatternOverrides`
  row in `authzop_sqlite_helpers_test.go` (the `project.visitors.update` row, kept after
  upstream's new user-scope rows) needed a hand resolution.
