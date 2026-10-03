# Broker NFS mount wiring (2026-10-02)

## Finding

`runtimebroker.ServerConfig.NFSConfig` was never set from settings, so the
NFS mount reconciler, the dispatch guard and the
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
- On: a background reconcile loop with 90 s command timeouts. Each command
  runs in its own process group, killed as a whole on timeout or request
  end, with a `WaitDelay` backstop, so the `mount.nfs` helper cannot keep
  the call waiting on its output pipe.
- Never mounting on Kubernetes or Cloud Run: if the broker's default
  runtime is in the Kubernetes family (`kubernetes`, `k8s`, `remote`) or the
  Cloud Run family (`cloudrun`, `cloudrun-instances`, `cloudrun-sandbox`),
  the loop runs verify-only even with auto_mount on.
- Dispatch gate (create only; start/restart are not gated): applies only
  when the project's effective workspace backend is `nfs`, and checks only
  `Shares[0]` (the share the nfs backend uses). It resolves the dispatch's
  runtime first. Kubernetes or Cloud Run: read the recorded status, warn,
  continue; never mount, never wait on the reconcile lock. Local-container
  runtimes: check (and mount, when the broker mounts) under the request
  context; 503 `nfs_unavailable` if not mounted.
- A check or mount cut short because the request context ended does not
  change the recorded share status; a real mount result or the exec
  timeout does.
- Mounted state comes from `/proc/mounts` (shared helper
  `runtimebroker.ProcMountSource`, also used by doctor: octal-unescaped,
  cleaned paths, last entry wins). `mountpoint(1)` runs only right before a
  mount, as a guard; a timeout there is reported and nothing is mounted.
- Non-root brokers with auto_mount on do not shell out at all; shares
  report "mounting NFS requires root" and a startup warning is logged.
- Share `server` must be a hostname or IPv4 address: whitespace, a leading
  `-` and IPv6 literals (bare or bracketed) are rejected.
- Health: `nfs_mounts` always reports per share. The overall `healthz`
  status is degraded only when the broker mounts the shares (auto_mount on,
  default runtime not Kubernetes or Cloud Run), after the first pass. The
  hub does not read broker health for dispatch (heartbeats always send
  `online`), but the web `/healthz` composite and `scion server start`
  readiness do, so check-only and pending states must not degrade it.
- NFS state never affects `readyz` and never stops the broker.
- `scion doctor` checks locally: configured, mounted from the expected
  `server:export`, server reachable on TCP 2049. Not mounted is a warning
  with auto_mount off, for `pv_name` shares, or when the default runtime is
  Kubernetes or Cloud Run (verify only, matching health); a failure otherwise.

## Follow-ups noticed

- `settings-v1.schema.json` has no `shared_dir_storage` definition yet.
- `NFSMountReconciler.CleanupNFSProject` has no production caller.
- Start/restart of an existing NFS-backed agent is not gated.
