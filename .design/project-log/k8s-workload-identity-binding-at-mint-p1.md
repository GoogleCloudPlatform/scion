# Workload Identity binding at project-scoped mint (ptone/scion#3329 phase 5, P1)

**Date:** 2026-11-14
**Branch:** scion/k8s-runtime-3329-p1
**Issue:** ptone/scion#7964 (part of ptone/scion#3329)

## What landed

- Experiment `hub.k8s_workload_identity_setup` (alpha, off, LayerServer + LayerWeb).
- `runtimes.<entry>.workload_identity_pool` (validated `<project-id>.svc.id.goog`; schema
  pattern `config.WorkloadIdentityPoolPattern`, admin registry key) and the flat
  `runtime_target.workload_identity_pool` (strict loader, conversion parity, never presented;
  not read until P3b).
- Broker heartbeat `workloadIdentityTargets` (legacy producer,
  `pkg/runtimebroker/workload_identity_targets.go`) plus capability `wiTargetsV1`
  (the design's `wi_targets_v1`).
- Nullable column `runtime_brokers.workload_identity_targets` (JSON string, the `health`
  pattern). `nil` = never reported, `[]` = reports none. Kept on flat rows; the hub drops
  `profile` from a flat row's targets.
- `GCPServiceAccountAdmin.AddRoleMembers` (one get-modify-set; conditional bindings are not
  extended); `IAMAdminClient.SetIAMPolicy` wraps it.
- `pkg/hub/gcp_sa_workload_identity.go`: kind-neutral plan over provider brokers, best-effort
  grant with etag retry and per-member fallback, `workloadIdentityBindings` in the mint
  response, audit event `gcp_sa.wi_binding`, CLI lines.

## Findings worth keeping

- Wire nil vs empty: `hubclient.BrokerHeartbeat.WorkloadIdentityTargets` has no `omitempty`,
  so an empty list reaches the hub as `[]` and replaces a stale list, while `null`
  (settings unreadable) keeps it. The hub request field is `*[]T` to tell them apart.
- `isEventualConsistencyError` matched IAM's "Identity Pool does not exist" 400, which would
  have added 7s of useless retries per mint; it now excludes it.
- Pool source follows design amendment A-1: global settings plus the hub overlay (the same
  source as the namespace), never project settings (warning at assign dispatch).

## Open for later phases

Hub-scoped mint (P2), KSA creation and `ksaCreation` (P3), flat producer (P3b), status view
(P4), ensure-bindings action (P5). The real-GKE gate for P1 is still to run (hybval).
