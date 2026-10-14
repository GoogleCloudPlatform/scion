# Federated project access item 6A: merge of upstream main (88549d3e0)

Date: 2026-10-13. Branch: scion/mmr-fed-p1-6a (ptone/scion#4797, tracked in ptone/scion#4603).

## Merge
- Merged upstream main at 88549d3e0 forward (no rebase); 15 upstream commits since 9fcae545b,
  including catalog entries for the artifact manage and version routes
  (GoogleCloudPlatform/scion#3531).
- One textual conflict: the operation count line of the generated
  `.design/authorization-operation-catalog.md` (upstream 218, item 6A 206 on its older base).
  Resolved by hand to 219; the rest of the report merged cleanly. The table of contents and the
  section order match the order of the `Catalog` slices at the merged head, with
  `federation.token.mint` at position 123 inside `identityOperations`.
- Check: the diff from upstream main to the merged head equals item 6A's own diff
  (9fcae545b..54a4a6b0a), except the operation count line.

## To confirm in the gate run
- `TestCatalogReportStaleness` and `TestPermissionRegistryRowsEnforcedOrReserved` at the merged head.
