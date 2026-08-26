# P6: deploy-instance command

**Date:** 2026-08-26
**Author:** dev-deploy-cmd agent
**Phase:** P6 — Authentication and one-command deploy (§11)

## What was built

A new `scion deploy-instance` CLI command (`cmd/deploy_instance.go`) that
implements the §11.5 deploy flow: from nothing to a printed URL, no manual
console steps.

### The deploy flow

1. **Resolve identity** — `gcloud config get account` → deploying operator email
2. **Resolve project number** — `gcloud projects describe` → numeric project ID
3a. **Create/update Instance via gcloud** — `gcloud beta run instances deploy`
   (v1 surface). Sets `--sandbox-launcher`, `--set-env-vars`, and optional
   resource flags. gcloud handles operation polling internally and is idempotent
   (create-or-update).
3b. **Enable IAP via REST v2 PATCH** — single PATCH with
   `updateMask=iapEnabled,invokerIamDisabled` sends both booleans atomically.
   The Instance is born with invoker check ON (default), so it is closed from
   birth; the IAP PATCH follows immediately.
4. **Gate 1 — IAP reconcile wait** — polls instance URL with unauthenticated
   HTTP client, waits for 302 to `accounts.google.com`
5. **Bind IAP access policy** — region-level `roles/iap.httpsResourceAccessor`
   binding (per-instance IAP returns 404, §11.2)
6. **Print effective access** — reads both project-level and region-level IAP
   bindings (project-level grants inherit invisibly, §11.2 audit hazard)
7. **Gate 2 — perimeter assertion** — fetches URL with NO credential, requires
   302 to `accounts.google.com` with `x-goog-iap-generated-response: true`.
   **Fails the deploy loudly if the app answers** (§11.1 single point of failure)
8. **Print URL** — `https://{name}-{number}.{region}.run.app`

### Key design decisions

- **Hybrid gcloud (v1) + REST (v2) approach** — `sandboxLauncher` is a v1-only
  field: REST v2 POST with it returns 400 "Unknown name", and REST v2 GET does
  not return it even when set. gcloud speaks v1, making it the only surface that
  can set `sandboxLauncher`. Conversely, `iapEnabled` and `invokerIamDisabled`
  are v2-only fields that gcloud has no flags for. The hybrid approach uses each
  API surface for the fields it supports.
- **v1/v2 hazard (§11.5c)** — v2 silently omits `sandboxLauncher` rather than
  erroring on read. Anything that round-trips an Instance through v2 will drop
  `sandboxLauncher` without complaint. The REST v2 PATCH is safe because it uses
  `updateMask` to touch ONLY the IAP booleans, leaving all v1-only fields
  untouched. Never do a full-body v2 PUT/PATCH populated from a v2 GET.
- **`services` in IAP audience, not `instances`** — IAP's fixed vocabulary uses
  `services` for all backend types. Test pinned with explanatory comment (§11.3).
- **Admin seeded at deploy time** — deploying operator email → `SCION_SERVER_HUB_ADMINEMAILS`
  env var. No bootstrap token for this tier. `--admin-email` override for CI
  service account deploys (§11.6).
- **Region-scope IAP caveat** — documented in command help. Multi-tenancy revisit
  trigger noted (§11.2).

### Idempotency

- `gcloud beta run instances deploy` is idempotent (create-or-update)
- REST v2 PATCH with `updateMask` is idempotent (sets both IAP booleans every time)
- IAP policy binding is additive (safe to repeat)
- Both gates are stateless checks (safe to repeat)

## Files changed

- `cmd/deploy_instance.go` — the deploy command implementation
- `cmd/deploy_instance_test.go` — unit tests (audience format, URL format,
  JSON body fields, Gate 2 perimeter assertion, helper functions)
- `cmd/cli_mode.go` — added `deploy-instance` to agent-mode allowlist
- `cmd/root.go` — exempt `deploy-instance` from project-required check

## Test results

- Unit tests pass, covering:
  - IAP audience format (pinning test for `services` vs `instances`)
  - Audience accepted by existing `isSupportedIAPAudience` validator
  - Instance URL matches `iapAudienceToCloudRunURL` output
  - IAP PATCH body contains both `iapEnabled: true` and `invokerIamDisabled: true`
  - IAP PATCH URL includes `updateMask=iapEnabled,invokerIamDisabled`
  - Gate 2: IAP enforcing (302 → pass), app answers (200 → FAIL),
    wrong redirect (→ FAIL), no IAP header (→ pass with warning)
- `make fmt-check` passes
- `go build ./...` succeeds
- Full `go test ./cmd/` suite passes

## Region-scope caveat

The IAP access policy binding is at the region level, not per-instance.
This is operationally equivalent to per-instance when the project holds a
single Scion Instance (§11.2). **If this tier ever hosts more than one
tenant in one project, region scope is immediately wrong** — revisit §11.2
and the §4.9a auth-proxy Service.
