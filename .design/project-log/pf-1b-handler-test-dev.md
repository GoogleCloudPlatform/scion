# Phase 1B handler failure-response regression coverage

## Changes

- Added table-driven HTTP handler coverage for templates, harness configs, and
  groups when the restricted authorized-list scan encounters a later-page store
  error or an authorization-policy read error.
- Each case asserts a non-success response that contains only the error payload:
  no endpoint item array and no `totalCount` are serialized.
- The later-page cases also confirm restricted scans retain their bounded,
  skip-count list options.

## Verification

- Focused handler, authorized-list, cursor, and scoped-UAT tests passed.
- `env -u SCION_GROVE -u SCION_PROJECT go test ./pkg/hub -timeout=600s -count=1`
  passed in 199.390s.
- `env -u SCION_GROVE -u SCION_PROJECT make ci` passed.

## Gate finding disposition

- The `pf-1b-stabilize-rev1` Required handler-serialization coverage finding is
  addressed by this test-only patch. No production bug was exposed.
