# GCP SA status view reads the Workload Identity binding (ptone/scion#3329 phase 5, P4)

**Date:** 2026-11-17
**Branch:** scion/k8s-runtime-3329-p4
**Issue:** ptone/scion#7968 (part of ptone/scion#3329)

## What landed

- `GCPServiceAccountAdmin.RoleMembers` (`pkg/hub/gcp_iam_admin.go`): reads unconditional
  members of a role on `projects/-/serviceAccounts/<email>` (`getIamPolicy`, resource-level
  only; conditional bindings excluded to match `AddRoleMembers`).
- `pkg/hub/gcp_sa_status_wi.go` & `pkg/hub/gcp_sa_status.go`: behind
  `hub.k8s_workload_identity_setup`, `buildGCPServiceAccountStatus` reads
  `roles/iam.workloadIdentityUser` on Hub-minted GSAs (`sa.Managed`), cached for 60 s per
  email (invalidated on `addWorkloadIdentityMembers` writes; failed reads are not cached,
  and list views never read IAM policy). Reuses `workloadIdentityMintPlanFor` across
  legacy and flat provider brokers to report `bound` or `not_bound` per target (with a
  single-quoted `manualStep` when not bound) and per profile row in `mappings` (checking
  the profile's mapped/discovered KSA when mapped, else the conventional `<account ID>`).
  Bring-your-own GSAs stay `unknown` with reason `"not minted by this hub"`. With the
  experiment off, output and behaviour match `main`.
- Stored-target staleness (P1 review 3 FYI-4): targets come from stored broker heartbeat
  reports; when a contributing broker does not advertise `wiTargetsV1` (for example after
  a downgrade) or its `LastHeartbeat` is stale, the target and profile binding set
  `stale: true` with `staleReason` (`broker_no_longer_reports` or
  `broker_heartbeat_stale`), omitting post-downgrade `LastHeartbeat` timestamps.
- CLI (`cmd/project_service_accounts.go`) and `pkg/hubclient`: decode and print per-target
  binding states, heartbeat age / staleness notes, and `manualStep` (`wiOneLine`-sanitized).
- Web (`web/src/components/shared/gcp-service-account-status.ts`,
  `web/src/utils/feature-flags.ts`): `LayerWeb` presentation gate
  (`K8S_WORKLOAD_IDENTITY_SETUP_FLAG`) adds the **Binding** column to the profile mappings
  table and renders per-target binding rows, staleness notes, and manual `gcloud` commands.
- Docs (`reference/api.md`, `reference/cli.md`, `reference/experiments.md`,
  `hosted/ha/kubernetes.md`).
