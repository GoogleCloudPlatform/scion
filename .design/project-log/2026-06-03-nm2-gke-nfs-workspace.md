# NM2 — GKE NFS Workspace Live Test — PASSED

**Date:** 2026-06-03  
**Agent:** qa-agent  
**Branch:** `postgres/wave-b-integration` @ `4a6ccf50`  
**Gate:** NM2 (Model B — GKE Autopilot + Filestore)  
**Full report:** `/scion-volumes/scratchpad/NM2-REPORT.md`

## Result: PASS (4 full + 1 partial-expected)

| Scenario | Result |
|----------|--------|
| (a) NFS-backed workspace PVC+subPath, not EmptyDir; init-container pre-populates | **PASS** |
| (b) Init-container provisioning race-safety via sentinel | **PARTIAL** — sequential sentinel works; concurrent race needs advisory lock (N2-2b), which requires hub-mediated provisioning with CloudSQL. Expected limitation in direct-pod test. |
| (c) kubectl cp skip for NFS backend (verified at `k8s_runtime.go:394`) | **PASS** |
| (d) Stable FSGroup/UID 1000 on all NFS-backed pods | **PASS** |
| (e) Shared dirs on NFS via same PVC + distinct subPaths | **PASS** |

## Infrastructure Provisioned in `scion-demo-cluster`

- Namespace `scion-agents` created
- Static PV `scion-workspaces` + PVC `scion-workspaces` bound to Filestore `10.45.255.170:/scion_share` (RWX, NFSv3, 1Ti) — left in place for future tests
- Binary `4a6ccf50` confirmed working on GKE Autopilot

## Key Observations

- GKE Autopilot auto-injects `seccompProfile: RuntimeDefault` — no node-level tuning possible, all pod-level
- NFSv3 + nconnect=4 works on Autopilot nodes (confirmed in mount output)
- Filestore BASIC_HDD ownership issue: share root owned by 1002:1003 from NM1 setup; new project dirs need UID 1000 for pod writes. In production, the hub's provisioner handles this under the advisory lock.
- Advisory lock end-to-end (N2-2b) requires GKE pods to reach CloudSQL (35.202.106.255:5432) — network path not fully verified; this is the one remaining unconfirmed integration point for GKE Model B.
