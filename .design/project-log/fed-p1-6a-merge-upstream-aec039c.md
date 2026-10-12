# Federated project access item 6A: merge upstream main (aec039c95)

Date: 2026-10-12. Branch: scion/mmr-fed-p1-6a (ptone/scion#4797).

## What happened
- Merged upstream main at aec039c9590b into the item 6A branch (merge forward, no rebase).
- One textual conflict: `.design/authorization-operation-catalog.md`, the generated operation report.
  Both sides added operations: this branch added `federation.token.mint` and upstream added the
  agent delegation management operations. The only conflicting line was the operation count.
- Resolution: upstream's file plus this branch's `federation.token.mint` entries (table of contents
  line and section), with the count set to 201 (base 196, plus 1 from this branch, plus 4 from upstream).
  Checked that the resolved file differs from upstream's only by this branch's own additions to the
  report, so it should match `RenderMarkdown(Catalog)` at the merged head.

## To confirm in the gate run
- `TestCatalogReportStaleness` in `pkg/hub/authzop` must pass without rewriting the report.
- The rest of `pkg/hub/authzop` (permission coverage now includes upstream's new delegation permissions).
