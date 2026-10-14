# Federated project access item 6A: merge of upstream main (bf090c67e)

Date: 2026-10-13. Branch: scion/mmr-fed-p1-6a (ptone/scion#4797, tracked in ptone/scion#4603).

## Merge
- Merged upstream main at bf090c67e forward (no rebase); 40 upstream commits since 06aa4b40c,
  including chat thread groups (GoogleCloudPlatform/scion#3513), `--test-mode`
  (GoogleCloudPlatform/scion#3467) and the project-visitor role (GoogleCloudPlatform/scion#3537).
- `.design/authorization-operation-catalog.md`: only the operation count conflicted. Upstream's
  report (205 operations, five new thread group operations) plus item 6A's
  `federation.token.mint` entry gives 206; the table of contents and the sections list the
  same operations in the same order.
- `pkg/hub/authzop_sqlite_helpers_test.go`: the live-inventory fixture now turns on four
  experiments (hub.conduit, hub.agent_delegation, hub.chat_thread_groups from upstream,
  federation.project_visitors from item 6A) and keeps item 6A's test OIDC key manager and
  allowed audience for the federation-token route.
- Check: the diff from upstream main to the merged head equals item 6A's own diff, apart from
  the operation count and the merged experiment list.

## To confirm in the gate run
- `TestCatalogReportStaleness`, the live method check and bearer matrix, the project-visitor
  and role permission-set tests, and the masked `./cmd/...` build at the merged head.
