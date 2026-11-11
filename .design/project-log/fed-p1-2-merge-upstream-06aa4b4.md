# Federated project access item 2: merge upstream main (06aa4b40c)

Date: 2026-10-13. Branch: scion/mmr-fed-p1-2.

## What happened
- Item 1 landed upstream as GoogleCloudPlatform/scion#3489 (one squashed commit). This branch was
  built on item 1's fork branch, so merging upstream main at 06aa4b40c brought item 1's content in
  a second time, as the squashed commit. Merge forward, no rebase.
- Two textual conflicts, both in regions that item 1 introduced and this branch changed further:
  - `pkg/store/entadapter/role_store.go` (`createRoleBinding`, the federated account check): this
    branch's version, which also refuses a visitor-path binding to a non-federated account.
  - `pkg/store/federated_user.go` (add/add): this branch's version, which adds
    `ErrNonMembershipRoleRefused` and `ErrVisitorBindingRefused`.
  - Upstream changed neither file after item 1, and both files are byte-identical between item 1's
    fork branch and the squashed upstream commit, so taking this branch's side drops nothing.
- No generated files conflicted; `pkg/ent` and the authorization operation catalog merged cleanly.

## Checks
- `git diff upstream-main HEAD` equals the branch's own change before the merge (diff against
  upstream aec039c95 plus item 1), except for line offsets in `pkg/hub/permission_helpers_test.go`
  from an upstream edit to the same file: 43 files, 1537 insertions, 98 deletions on both sides.
- Upstream since aec039c95 (other than item 1) adds no permission, role, membership read, member
  list or role binding path, so nothing new needs the confers-membership filter. Its removal of a
  SQLite test driver does not touch any helper this branch's tests use.
- Gates (ent-check, the catalog staleness test, this branch's tests) run after the merge review.
