# Kubernetes NFS volume health check on start and restart

Date: 2026-11-07. Base: 92d405a0fb2ea3ac2bd974c6c5c534914d50df50 (contains the NFS volume mount fail-fast, 4828ec70).

## Findings
- The local-runtime NFS check from upstream PR 4377 is a mount-flag check. On local-container starts and restarts, `checkNFSMountFlagsForStart` (pkg/runtimebroker/handlers.go) re-reads the broker's mount table and refuses a share mounted without `nosuid,nodev`. Kubernetes and Cloud Run skip it on purpose, because the kubelet mounts their volumes and the pod security context is the boundary.
- The broker already refuses a Kubernetes start when it cannot see the share itself (`projectfs.ShareDirVisible`, a bounded 10 s stat; `pkg/agent/nfs_workspace_leaf.go`).
- The mount fail-fast (`pkg/runtime/k8s_mount_check.go`) acts only while the agent container waits in ContainerCreating or PodInitializing. A volume that mounts but is unusable in the container passed every check: hung after the mount, stale, mounted read-only where the pod wants read-write, or not readable or writable by the agent's uid and fsGroup. The start then reported success and the agent failed later.
- Restart, resume and wake all reach `KubernetesRuntime.Run`, where `waitForExecReady` runs for every pod; the harness waits for the startup gate at the end of `Run`.
- `pkg/runtimebroker` imports `pkg/runtime`, and not the other way round, so the mount table parser in `pkg/runtimebroker/mount_table.go` could not be used from `pkg/runtime` where it was.

## Decisions
- The parser (`MountEntry`, `MountTable`, `ParseMountTable`, `unescapeMountField`) moved to `pkg/projectfs` in its own behaviour-free commit, with aliases and a wrapper left in `pkg/runtimebroker`. `FSType` was added in the feature commit.
- The check runs from the broker, by exec into the agent container, after `waitForExecReady` and before home preparation, the syncs and the startup gate (`pkg/runtime/k8s_volume_health.go`). It needs no image change, its failure is a `LaunchFailure` of `Run`, and restarts are covered because they run `Run` again.
- Targets: the agent container's mounts of the volumes the mount fail-fast watches (`newPodMountWatch`).
- What it checks: it reads `/proc/self/mounts` with `cat` (never touches an NFS mount) and parses it with the shared parser. A mount that is not NFS is skipped. A missing mount is `ShareNotMounted`, and an `ro` mount where the pod wants rw is `ShareReadOnly`. Then one exec per mount runs `cd`, `test -r` and, for read-write mounts, `test -w`, each exec bounded by `projectfs.ShareVisibleTimeout` (10 s). A probe cut off by its own bound is `ShareUnresponsive`. The check reads metadata only, writes nothing to the share and runs nothing from it.
- A mount is read-only by configuration when its volumeMount, its NFS volume source or its claim source is `readOnly`. Such a mount is checked for being mounted and readable only, never write-tested, and its `ro` mount is expected.
- Threshold: 3 consecutive faults, 5 s apart. A mount that passes a check is accepted and not probed again.
- The failure is the new launch code `volume_unhealthy` in step `container_start`, with the reason as the Kubernetes-style enum. The client text is "an agent volume is mounted but not usable", the same in the broker, CLI and web tables. The error and the broker log name the volume and its configured share only.
- The check fails open: an exec error that is not the probe's own timeout, an empty or garbled mount table, or unexpected probe output is logged once with the volume names and an error class, and the start goes on. The later mandatory execs of `Run` still fail closed on real exec trouble.
- `test -w` only, no probe file: no writes to the user's share. The docs name the three cases it misses (an optimistic ACCESS answer, a refusal only at write time, a stale handle seen only on write).
- No experiment gate, as for the mount fail-fast.

## Rejected alternatives
- An in-container entrypoint check: it needs an image change and a new exit classification.
- Reusing `volume_mount_failed`: the volume did mount, and that code's step and text say otherwise.
- A probe file write: it writes to the user's share.
- Applying the `nosuid,nodev` rule to pods: PR 4377 keeps it off Kubernetes by design.
- A copy of the parser in `pkg/runtime`: two parsers that can drift.

## Follow-ups
- The Cloud Run part of the item.
- A runtime-owned probe file, if field reports show `test -w` passes volumes that then refuse writes.
- A combined healthy-path probe, to cut the execs from 1 + N to 2, if the start latency matters.
