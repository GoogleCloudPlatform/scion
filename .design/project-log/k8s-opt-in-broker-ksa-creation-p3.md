# Opt-in broker KSA creation for Hub-minted GSAs (ptone/scion#3329 phase 5, P3)

**Date:** 2026-11-17
**Branch:** scion/k8s-runtime-3329-p3
**Issue:** ptone/scion#7966 (part of ptone/scion#3329)

## What landed

- `runtimes.<entry>.create_service_accounts` (`bool`, default `false`) in `V1RuntimeConfig`,
  `settings-v1.schema.json`, `opsettings` (`runtimes` section), and `v1RuntimeIndicatorKeys`,
  plus `ProjectSettingsHasCreateServiceAccounts` so project-level values log an operator
  warning and are never honoured (Amendment A-1: read only from global + Hub database overlay).
- `HubAgentDefaults.CreateKSA` (`RemoteHubAgentDefaults.CreateKSA` on the Hub; included in
  `HubAgentDefaults.IsEmpty()` so `startAgent` and `restartAgent` retain a `CreateKSA`-only
  defaults struct on `ctx`), plus `experiments.K8sWorkloadIdentitySetup` added to
  `dispatchExperimentNames`. The Hub sets `CreateKSA = true` on create, start, and restart
  only when `hub.k8s_workload_identity_setup` is enabled and the assigned GSA row is
  Hub-minted (`Managed == true`).
- `pkg/k8s/serviceaccount_create.go`: `ConventionalKSAName`, `ServiceAccountManagedForGSA`,
  and `EnsureConventionalServiceAccount(ctx, client, namespace, ksaName, gsaEmail)` — creates
  `<account-id>` with `iam.gke.io/gcp-service-account`, `scion.dev/created-for-gsa`,
  `app.kubernetes.io/managed-by=scion`, and `automountServiceAccountToken=false`, and on
  `AlreadyExists` re-reads via `Get` without ever modifying an existing ServiceAccount.
- `pkg/runtimebroker/gcp_identity_assign.go`: `createOrReuseAssignKSA` (no legacy discovery
  dependency, ready for P3b flat reuse), precedence `mapping > discovery > create`,
  side-effect-free validation of explicit `serviceAccountName`/`namespace` before `Create`,
  `400 identity_ksa_conflict` on name collision, and `400 identity_not_mapped` with
  `details.missingPermission` on `403 Forbidden`.
- Broker heartbeat `workloadIdentityTargets` populates `ksaCreation` (even when
  `workload_identity_pool` is unset, kept by `normalizeWorkloadIdentityTargets` while still
  skipped by `workloadIdentityMintPlanFor`).
- Hub `"creatable"` (`SAMappingStateCreatable` / `GCPSAMappingCreatable`) state across
  `gcp_identity_dispatch_precheck.go`, `gcp_sa_assign_status.go`, `gcp_sa_mapping_warnings.go`,
  `project_settings_profile_warnings.go`, `gcp_sa_status.go`, `identity_mapping_error.go`, CLI
  (`KSA created at start`), and web picker/status components.
- `scion doctor` checks `get`, `list`, and `create` on `serviceaccounts` when
  `create_service_accounts` is enabled on the active runtime entry in global settings.
- Documentation updated across `kubernetes.md`, `setup-gcp.md`, `permissions.md`,
  `orchestrator-settings.md`, and `experiments.md`.

## Findings worth keeping

- `withStartHubAgentDefaults` (`startAgent` / `restartAgent`) drops `HubAgentDefaults` when
  `IsEmpty()` is true, whereas `createAgent` runs `buildStartContext` before calling
  `withHubAgentDefaults`. Including `!d.CreateKSA` in `IsEmpty()` and reading both context and
  `in.Config.HubAgentDefaults` in `hubAuthorizedCreateKSA` keeps create, start, and restart
  consistent while preserving file-mode parity when `CreateKSA` is false.
- Checking explicit request `serviceAccountName` and `namespace` mismatches before calling
  `Create` prevents leaving behind a newly created KSA on a refused 400 request.

## Open for later phases

Flat Runtime Broker instances (`runtime_target.create_service_accounts`, flat
`workloadIdentityTargets` producer, and flat get-by-name + `createOrReuseAssignKSA` — P3b),
hub-scoped mint (P2), status view IAM policy read (P4), and optional ensure-binding action (P5).
Real-GKE end-to-end Workload Identity token verification script is included in the P3 handoff.
