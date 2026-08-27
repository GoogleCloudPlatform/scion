# Phase 1B corrective completion verification

**Date:** 2026-08-24
**Branch:** `scion/permissions-p1b`
**Starting head:** `93df005dee3afbdb3b68d62ddf40e781f94380cb`

## Summary

Completed the lead-authorized verification pass for the post-cap list-row correction.
The handler implementation already correctly counts authorized matches across the full
matching set, so no production-handler change was needed. The endpoint regression was
expanded to cover every required behavior for templates, harness configs, and groups:

- scoped UAT, unfiltered and explicit project-B filters;
- readable project-A rows with the `read` capability;
- denied project-B rows absent from scoped responses;
- unscoped local-admin visibility; and
- `TotalCount` values that remain accurate with `limit=1`, rather than collapsing to
  the current page size.

The matrix seeds 51 authorized project-A rows and one denied project-B row per endpoint.
The groups case also accounts for the test server's seeded hub-members group in the
unscoped-admin total.

## Verification

Passed:

```text
env -u SCION_GROVE -u SCION_PROJECT go test ./pkg/hub \
  -run '^TestScopedAdminListEndpointsFilterCrossProjectRowsAndCountAuthorizedMatches$' \
  -count=1 -timeout=180s -v

env -u SCION_GROVE -u SCION_PROJECT go test ./pkg/hub \
  -count=1 -timeout=240s -run '<Phase 1B scoped/federated admin, capability, registry, and list-filter matrix>' -v

env -u SCION_GROVE -u SCION_PROJECT go test ./pkg/hub -timeout=600s -count=1
# PASS (198.209s)

env -u SCION_GROVE -u SCION_PROJECT make ci
# PASS
```

No remaining implementation blocker was found. Fresh corrective review, security, and QA
gates are still required by the lead authorization before Phase 1B can be accepted.
