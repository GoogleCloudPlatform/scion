# Kubernetes NFS mount failure fails fast: plan (ptone/scion#7397)

Date: 2026-11-05. Base: c54bc5ec787ff4106fb775f3aedb6980cedc07cc.

## Findings
- On Kubernetes, `waitForPodReady` (pkg/runtime/k8s_runtime.go) read no Events. A pod whose NFS volume cannot mount sits in ContainerCreating or PodInitializing until the 10 minute `launch_timeout`.
- The claim check (k8s_claim_check.go) runs only before the pod exists. It checks that claims bind, and it does not cover inline NFS volumes.
- `Run` is the only caller of `waitForPodReady`, so a restart goes through the same wait.
- A new launch code is not self-contained:
  - the broker's `launchFailureText` (pkg/runtimebroker/run_launch.go) is the hub-facing allow-list, and a code missing from it is reported as `runtime_error`;
  - `web/src/shared/agent-launch-labels.test.ts` requires a web label for every `pkg/api` code, equal to the broker text;
  - `cmd/launch_labels_test.go` requires a CLI label for every code.
- `podLaunchStep` names the volume-mount wait `init_containers` for pods with init containers, because Kubernetes mounts every volume before any init container runs.

## Decisions
- A new code, `volume_mount_failed`, always in step `container_create` (the step whose documentation covers volume mounts), with the Kubernetes event reason as its reason.
- The hub text, identical in the broker, CLI and web tables, is "an agent volume could not be mounted". The volume and share detail stay in the broker log and the runtime error.
- Detection: pod Events `FailedMount`/`FailedAttachVolume` on the pod's NFS volumes only, which are:
  - inline NFS volumes;
  - the volumes of the configured NFS share claims (workspace, home, and the shared-dir claim of the nfs or cloudrun-volume backend).
- Threshold: the failure has been seen for 60s AND its occurrence count has grown since first seen. Kubelet's mount retry backoff clears transient failures within about 30s, and the recurrence rule means a stale event never fails a start.
- Bounds: at most one Events list per 10s, only while the pod is scheduled and no container has started; a kind/name/UID field selector; Limit 100; a 2s timeout. Any read error fails open to the existing wait.
- Claim-backed failures are matched by kubelet's "unmounted volumes" message, at about 2-4 minutes, with no extra claim read.
- No experiment gate: the change classifies an existing wait more precisely and adds no setting or hub behaviour.

## Rejected alternatives
- Reusing `container_config` with reason FailedMount: its label ("could not be configured") misdescribes a mount failure.
- Reporting the failure in the last progress step (`init_containers` on pods with init containers): it would label a volume failure as an init container problem.
- Mapping each claim to its PersistentVolume name, for faster claim detection: an extra API read per claim for a 1-3 minute gain.

## Follow-ups
- 8.1: map claims to PersistentVolume names if claim-backed detection proves too slow in practice.
- 8.2: bound the claim check's Events list (k8s_claim_check.go `claimEvent`) with a timeout and Limit.
- 8.3: name the volume-mount wait in `podLaunchStep` for pods with init containers.
