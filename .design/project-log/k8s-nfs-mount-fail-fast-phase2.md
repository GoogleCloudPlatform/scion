# Kubernetes NFS mount failure fails fast: implementation (ptone/scion#7397)

Date: 2026-11-05. Base: c54bc5ec787ff4106fb775f3aedb6980cedc07cc.

## What changed
- New pkg/runtime/k8s_mount_check.go: `podMountWatch` (the pod's NFS volumes), and `checkMounts`, which runs on each `waitForPodReady` poll.
  - It fails a start with `volume_mount_failed` once an NFS mount failure has lasted and recurred.
  - `waitForPodReady` takes the watch as a new parameter; `Run` builds it from the pod spec and the start's configuration.
- The detail text names the volume, the pod, the configured share (server:path or claim), the event reason and, only when an event named that same volume itself (`for volume "X"`), a fixed hint phrase. Kubelet's "unmounted volumes" message only reports a timed-out wait, so it gives no hint.
- One new launch code, `volume_mount_failed`. One entry each in the broker, CLI and web label tables, with the same text.
- Docs: hosted/ha/kubernetes.md, new "NFS volume mount failures" subsection, plus the RBAC and launch-timeout lines.

## Event origin (accepted)
Kubernetes Events carry no verified origin: whoever creates an event sets its source fields. So an actor allowed to create events in the agent namespace could end a start there early with `volume_mount_failed`. This is accepted:
- agent pods do not get that permission;
- the outcome is a start that fails early, with nothing disclosed;
- a source-field filter would not change it.
Two properties limit what an event can do:
- Events count only for the current pod. `readMountEvidence` reads nothing for a pod with no UID, selects events by the pod's UID, and re-checks the UID of each event in code. An event of an earlier pod of the same name never counts.
- No event text is echoed:
  - only the reasons FailedMount and FailedAttachVolume count;
  - `podMountWatch.match` returns only one of the pod's own watched volume names, which `newPodMountWatch` takes from the pod spec;
  - `mountFailureHint` returns only a fixed phrase, taken from the reported volume's own named-form event;
  - `mountFailureError` builds the text from those alone;
  - the hub sees only the fixed table text plus the reason.

## Test evidence (static; compiled and run in the verification window)
- Fail-fast:
  - inline and claim-backed failures fail before the pod-ready budget;
  - the step stays `container_create` on pods with init containers.
- Not failed:
  - non-NFS volumes, other pods' events and stale events keep the existing wait;
  - transient failures below the threshold do not fail;
  - read errors fail open, and the fallback is logged once (also for the read's own timeout) but not when the wait itself ended.
- The gate's sub-conditions and the evidence resets have their own tests.
- Golden texts carry planted mount options, paths and markers, and assert that none appear.
- Every decision has a single-change mutant with a named killing test; the web label's mutant is killed by reading, and the web test runs in CI.

## Notes
- The shared-dir claim is watched by an allow-list of backends (nfs, cloudrun-volume). gke-shared-volume and local-backend claims keep the existing wait.
- Follow-ups 8.1-8.3 are recorded in the plan log and not implemented.
