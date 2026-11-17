# Workload Identity binding at hub-scoped mint and web mint dialog (ptone/scion#3329 phase 5, P2)

**Date:** 2026-11-17
**Branch:** scion/k8s-runtime-3329-p2
**Issue:** ptone/scion#7965 (part of ptone/scion#3329)

## What landed

- `mintHubScopedGCPServiceAccount` (`POST /api/v1/gcp-service-accounts/mint?scope=hub`) now runs
  the P1 Workload Identity binding helper (`hubWorkloadIdentityBindings` -> `workloadIdentityMintPlanFor`
  -> `grantWorkloadIdentityBestEffort`) over every Runtime Broker on the Hub when
  `hub.k8s_workload_identity_setup` is enabled, returning `workloadIdentityBindings` and
  emitting `gcp_sa.wi_binding` audit events.
- Web mint dialog (`<scion-gcp-service-account-list>`, both `project` and `hub` scopes): when
  `hub.k8s_workload_identity_setup` (`LayerWeb`) is enabled and the mint response carries
  `workloadIdentityBindings`, the dialog stays open to display one line per binding entry
  (`bound`, `skipped`, `failed`), the manual `gcloud` command for any entry not bound, and the
  propagation note when any entry is bound.
- P1 review-3 follow-ups:
  - `GCPTokenEvent.Member` records and emits `member` on `gcp_sa.wi_binding` audit records,
    including `bound` grants, in both project and hub scopes.
  - `projectWorkloadIdentityBindings` and `hubWorkloadIdentityBindings` return a warning view
    that reuses the broker list already loaded for the mint, avoiding a second broker walk for
    SA mapping warnings.
  - `GCPTokenEventWIBinding` moved into the `GCPTokenEventType` block in `pkg/hub/audit.go`.
- Updated `hosted/ha/kubernetes.md` and `reference/experiments.md` for hub-scoped minting and
  the web mint dialog.

## Remaining phases

Broker KSA creation (P3), flat Runtime Broker producer and resolver (P3b), status view binding
read (P4), and optional ensure-bindings action (P5).
