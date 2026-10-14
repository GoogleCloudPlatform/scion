# Federated project access item 6B: merge of upstream main (5d15cbd8f)

Date: 2026-10-14. Branch: scion/mmr-fed-p1-6b (ptone/scion#4868, tracked in ptone/scion#4603).

## Merge
- Merged upstream main at 5d15cbd8f forward (no rebase). The branch was stacked on an
  earlier item 6A head (1bea0b023); item 6A has since landed upstream as
  GoogleCloudPlatform/scion#3598, together with federated accounts
  (GoogleCloudPlatform/scion#3489) and the project-visitor role (GoogleCloudPlatform/scion#3537).
- 14 files conflicted. Ten are item 1 and item 6A files that item 6B does not change
  (catalog report, federation-token command and handler, OIDC handler, route manifest and
  metadata, live-inventory fixture, hub client, role store, federated user store): upstream's
  text is taken as is.
- `pkg/hub/auth.go`, `pkg/hub/federation_visitor.go`, `pkg/hub/server.go`: upstream's item 6A
  text plus item 6B's own hunks, unchanged.
- `pkg/experiments/registry.go`: upstream's "user access credentials" wording for the
  federation.project_visitors comment and description, with item 6B's added clause about
  peer hub issuers kept; the comment is rewrapped to the usual width.
- The catalog slices are unchanged by item 6B, so upstream's catalog report stands.
- Check: the diff from upstream main to the merged head has the same files and the same
  added and removed lines as item 6B's own diff (1bea0b023..61fba18f0), apart from the
  registry wording above. No removed line came back in an auto-merged file.

## To confirm in the gate run
- pkg/config tests, the item 6B pkg/hub tests, the catalog staleness test and the masked
  `./cmd/...` build at the merged head.
