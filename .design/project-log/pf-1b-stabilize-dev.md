# Phase 1B authorized list/count stabilization

## Changes

- Added `ListOptions.SkipTotalCount` and `CursorBinding`; restricted template,
  harness-config, and group scans fetch only bounded pages and never issue the
  ordinary full-filter count query.
- Reworked keyset predicates to use the decoded `(created,id)` tuple directly,
  so a cursor remains useful when its source row is deleted.
- Bound endpoint cursors to the endpoint and normalized filter, rejecting
  malformed, cross-endpoint, and cross-filter cursors at the handler boundary.
- Added regressions for fail-closed later-page/auth failures, in-flight
  cancellation, cursor deletion/binding, and malformed handler cursors.

## Verification

- Focused Hub authorized-list/cursor tests: passed.
- Focused Ent template, harness-config, and group cursor tests: passed.
- Full Hub and repository CI were run as required before handoff.

## Gate finding disposition

- QA: added fail-closed error and in-flight cancellation regressions plus
  handler malformed-cursor coverage.
- Review: cursor tuple no longer depends on a cursor-row lookup; deletion
  regression added.
- Security audit: restricted scans set `SkipTotalCount`; cursor bindings reject
  endpoint/filter reuse.
