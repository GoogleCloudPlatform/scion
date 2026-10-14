# Federated project access item 6A: merge of upstream main (9fcae545b)

Date: 2026-10-13. Branch: scion/mmr-fed-p1-6a (ptone/scion#4797, tracked in ptone/scion#4603).

## Merge
- Merged upstream main at 9fcae545b forward (no rebase); 6 upstream commits since bf090c67e,
  including target-hub profile agent commands (GoogleCloudPlatform/scion#3532), broker work
  joined on shutdown (GoogleCloudPlatform/scion#3522) and the telemetry annotation comment
  update (GoogleCloudPlatform/scion#3544).
- No textual conflicts. `.design/authorization-operation-catalog.md` merged cleanly: upstream
  changed only the `testidentity.webcode.issue` section (205 operations), item 6A adds
  `federation.token.mint` (206). `pkg/hub/authzop_sqlite_helpers_test.go` is unchanged upstream
  since bf090c67e.
- Check: the diff from upstream main to the merged head equals item 6A's own diff
  (bf090c67e..54d309bf4), ignoring hunk headers.

## To confirm in the gate run
- `make check-annotation-prefix` (passes at the merged head), `TestCatalogReportStaleness`,
  the live method check and bearer matrix, and the masked `./cmd/...` build at the merged head.
