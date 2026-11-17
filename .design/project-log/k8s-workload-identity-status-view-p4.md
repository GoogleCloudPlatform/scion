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
  `roles/iam.workloadIdentityUser` on Hub-minted GSAs (`sa.Managed`), bounded by a 10 s
  read timeout and cached for 60 s per email (invalidated after any
  `addWorkloadIdentityMembers` write attempt, on mint entry, and on GSA deletion via
  `invalidateActAsCache`; failed reads are not cached, and list views never read IAM
  policy). Reuses `workloadIdentityMintPlanFor` across legacy and flat provider brokers and
  resolves each mapped profile's actual dispatch identity (mapped/discovered KSA and
  namespace when `mapped`, else the conventional `<account ID>`) as one shared source of
  truth across `workloadIdentityBinding.targets`, `mappings[].workloadIdentityBinding`,
  summary `workloadIdentityBinding.state`, and `nextStep` (with a single-quoted `manualStep`
  when not bound, and distinct missing-pool reasons `no_workload_identity_pool`,
  `broker_too_old`, and `flat_broker_pool_unreported`). When any dispatch target is
  `not_bound`, `nextStep` returns code `not_bound` pointing at the manual `gcloud` step;
  when any Kubernetes target reports no pool while reported targets are bound, summary
  `state` stays `unknown`; and IAM read errors or stale stored targets never report
  `"Nothing missing"` or `"is not checked"` in `nextStep.message`. Bring-your-own GSAs stay
  `unknown` with reason `"not minted by this hub"`. With the experiment off, output and
  behaviour match `main`.
- Stored-target staleness (P1 review 3 FYI-4): targets come from stored broker heartbeat
  reports; when a contributing broker does not advertise `wiTargetsV1` (for example after
  a downgrade) or its `LastHeartbeat` is stale, the target and profile binding set
  `stale: true` with `staleReason` (`broker_no_longer_reports` or
  `broker_heartbeat_stale`), omitting post-downgrade `LastHeartbeat` timestamps and
  qualifying the summary `reason` and `nextStep.message`.
- CLI (`cmd/project_service_accounts.go`) and `pkg/hubclient`: decode and print per-target
  binding states, heartbeat age / staleness notes, and `manualStep` (`wiOneLine`-sanitized).
- Web (`web/src/components/shared/gcp-service-account-status.ts`,
  `web/src/utils/feature-flags.ts`): `LayerWeb` presentation gate
  (`K8S_WORKLOAD_IDENTITY_SETUP_FLAG`) adds the **Binding** column to the profile mappings
  table and renders per-target binding rows, staleness notes, and manual `gcloud` commands.
- Docs (`reference/api.md`, `reference/cli.md`, `reference/experiments.md`,
  `hosted/ha/kubernetes.md`).
