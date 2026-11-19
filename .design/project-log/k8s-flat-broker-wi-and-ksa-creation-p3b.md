# Flat Runtime Broker Workload Identity targets and opt-in KSA creation (ptone/scion#3329 phase 5, P3b)

**Date:** 2026-11-19
**Branch:** scion/k8s-runtime-3329-p3b
**Issue:** ptone/scion#7967 (part of ptone/scion#3329)

## What landed

- `server.broker.instances[].runtime_target.create_service_accounts` (`bool`, default `false`) in
  `V1RuntimeTargetConfig`, `RuntimeTargetConfig` (`createServiceAccounts`),
  `settings-v1.schema.json`, strict `LoadRuntimeBrokerInstances` boolean check, Kubernetes-only
  field validation, `ConvertProfilesToInstances` parity (including `every_profile_field` and
  `every_runtime_field` fixtures), and FC4-a `never` redaction.
- Flat `workloadIdentityTargets` producer (`flatWorkloadIdentityTargets` in
  `pkg/runtimebroker/workload_identity_targets.go`) built from the startup
  `flatKubernetesIdentityPolicy` snapshot: emits at most one entry (`profile` empty, `namespace`
  from `flatInstanceNamespace`, `pool` and `ksaCreation` from `runtime_target`) on every flat
  heartbeat with `Capabilities.WITargetsV1 = true`.
- Hub `workloadIdentityMintPlanFor` removes the P1 interim `wiMissingFlat` placeholder now that
  flat instances advertise `wiTargetsV1`, treating missing flat targets identically to legacy
  ones (`wiMissingNotConfigured` with `WITargetsV1`, `wiMissingTooOld` without).
- `redactBrokerStorage` / `brokerStorageForViewer` (`pkg/hub/storage_layout_view.go`) redacts
  `store.RuntimeBroker.WorkloadIdentityTargets` on Runtime Broker read and list responses for
  viewers without `hub.config.read` (except `pureSelf`), even when `WorkspaceStorage` is `nil`,
  closing P1 review 3 FYI-3 while preserving FC4-a's "never published" posture.
- Flat resolver branch in `resolveFlatKubernetesAssignIdentity` (`lookupOrCreateFlatAssignKSA`):
  when `runtime_target.kubernetes_service_account_mappings` misses and both
  `runtime_target.create_service_accounts` and the Hub's per-dispatch `CreateKSA` flag are true,
  validates conventional name and explicit KSA/namespace overrides before any API call, then
  performs one `Get` by `<account-id>` in `flatInstanceNamespace` (never `List`):
  - `NotFound`: creates via `k8s.EnsureConventionalServiceAccount` directly (not the legacy
    `createOrReuseAssignKSA` wrapper, whose doc comment is also corrected);
  - present with `app.kubernetes.io/managed-by=scion` AND this GSA's annotation: reuses it;
  - anything else (including operator-annotated KSAs lacking the Scion label): refuses with
    `400 identity_ksa_conflict` without modifying the existing ServiceAccount.
- Added Revision 7 note to `.design/flat-runtime-brokers/p2-kubernetes-identity-registration-amendment.md`,
  updated FC4-a in `.design/flat-runtime-brokers/contract-appendix-p3.md`, and updated flat
  documentation in `server-config.md`, `kubernetes.md`, `setup-gcp.md`, `permissions.md`, and
  `experiments.md`.

## Findings worth keeping

- Doing `Get` by conventional name before calling `k8s.EnsureConventionalServiceAccount` keeps
  steady-state restarts on an already-created Scion KSA to a single `Get` (zero `Create` calls and
  zero `AlreadyExists` audit noise), while still delegating `NotFound` creation and concurrent
  `AlreadyExists` race handling to `k8s.EnsureConventionalServiceAccount` with
  `KSACreateReusedAnnotated` mapped to `identity_ksa_conflict`.
- `brokerStorageForViewer` previously short-circuited when `b.WorkspaceStorage == nil`; guarding
  on `b.WorkspaceStorage == nil && len(b.WorkloadIdentityTargets) == 0` ensures brokers without a
  workspace storage descriptor still redact `WorkloadIdentityTargets` for non-`hub.config.read`
  viewers.
