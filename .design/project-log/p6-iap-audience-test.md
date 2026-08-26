# P6: IAP Instance-audience test and region-scope documentation

**Date:** 2026-08-26
**Author:** dev-iap-test
**Branch:** scion/dev-iap-test (based on scion/dev-rebase-1294)

## What was done

Added a test case to `TestIsSupportedIAPAudience` in `cmd/server_foreground_test.go`
that pins the Cloud Run Instance-form audience:

```
/projects/123456789/locations/us-east4/services/my-instance
```

The test asserts this audience is accepted by `isSupportedIAPAudience` and includes
a thorough doc comment explaining the rationale.

## Why the audience says "services" for an Instance

IAP uses a fixed resource vocabulary (`/services/`) for every backend type, including
Cloud Run Instances. The path segment is **not** a description of the resource type —
it is IAP's canonical audience format. The Instance audience looks identical to a
Service audience because IAP does not distinguish between them.

This is NOT a bug and NOT a mismatch to correct. A future change from `services` to
`instances` would produce an audience mismatch on every request: the IAP edge would
continue to stamp JWTs with `/services/`, but the hub would expect `/instances/`,
and every comparison would fail with a 401 that does not obviously point back to the
audience configuration.

Reference: design doc §11.3, OQ-17.

## Region-scope IAP policy binding

IAP policy binding for Cloud Run Instances is at the **region level**, not per-instance:

```
projects/{PROJECT_NUMBER}/iap_web/cloud_run-{REGION}
```

Per-instance `setIamPolicy` returns 404 — there is no per-Instance IAP resource path.
This was confirmed during the OQ-17 investigation (design doc §10b.1, §11.2).

This is acceptable for the single-node Cloud Run Instance tier because:
- The project hosts exactly one tenant (one Scion Instance).
- A region-level grant therefore admits the holder to exactly one resource.
- The breadth of the grant is operationally identical to a per-resource grant.

### Revisit trigger

**If this tier ever hosts more than one tenant in one project, region scope is
immediately wrong.** A region-level `roles/iap.httpsResourceAccessor` binding would
admit any authorized user to every Cloud Run resource in the region, breaking tenant
isolation. Per-resource auth (e.g., the §4.9a auth-proxy Service pattern) must come
back before multi-tenancy is introduced.

Reference: design doc §11.2, §11.1.

## Files changed

- `cmd/server_foreground_test.go` — added Instance-audience test case with doc comments
