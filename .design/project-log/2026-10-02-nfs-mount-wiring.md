# Broker NFS mount wiring (2026-10-02)

## Finding

`runtimebroker.ServerConfig.NFSConfig` was never set from settings, so the
NFS mount reconciler, the `ensureNFSMountsReady` dispatch guard and the
`nfs_mounts` health check never ran on a real broker. The original N1-7 work
wired it from `cmd/server_foreground.go`, but that wiring did not survive the
squash merge (upstream #306), and nothing set the field afterwards. The
doctor D5 step always reported skip.

## Decision

Brokers on the `nfs` backend already mount exports by hand (or rely on the
kubelet), and some do not run as root. Mounting automatically and adding a
new 503 on upgrade would change their behaviour, so mounting is opt-in:

- `server.workspace_storage.nfs.auto_mount`, default `false` (Layer-0).
- The broker sources its NFS block from **global** settings only
  (`config.LoadGlobalSettings`), like `shared_dir_storage`, and validates it
  (absolute `mount_root`, single-segment unique share ids, server, absolute
  export) before using it.
- Off: read-only checks at startup and every minute feed `healthz`
  `nfs_mounts`. No mounts, no dispatch gate.
- On: a background reconcile loop with 90 s command timeouts. The dispatch
  gate applies only when the project's effective workspace backend is `nfs`,
  and checks only `Shares[0]` (the share the nfs backend uses): 503
  `nfs_unavailable` on local-container runtimes, a warning on Kubernetes.
  Dispatch-time mounts and the wait for the reconcile lock are bounded by
  the request context as well as the exec timeout.
- Non-root brokers with auto_mount on do not shell out to `mount`; shares
  report "mounting NFS requires root" and a startup warning is logged.
- Health: `nfs_mounts` always reports per share. The overall `healthz`
  status is degraded only with auto_mount on, after the first pass, on a
  non-Kubernetes default runtime. The hub does not read broker health for
  dispatch (heartbeats always send `online`), but the web `/healthz`
  composite and `scion server start` readiness do, so check-only and
  pending states must not degrade it.
- NFS state never affects `readyz` and never stops the broker.
- `scion doctor` checks locally: configured, mounted from the expected
  `server:export`, server reachable on TCP 2049.

## Follow-ups noticed

- `settings-v1.schema.json` has no `shared_dir_storage` definition yet.
- `NFSMountReconciler.CleanupNFSProject` has no production caller.
