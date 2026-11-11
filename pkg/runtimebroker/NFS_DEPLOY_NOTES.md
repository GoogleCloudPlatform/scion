# NFS Workspace Storage — Deploy Notes

## Broker Service UID/GID Alignment

The broker service user's UID and GID **must** match the `NFS.UID` and
`NFS.GID` values in `settings.yaml` (default: `1000:1000`).

When the broker provisions NFS-backed workspaces (clone, chown under the
Postgres advisory lock), it writes files using its own on-wire UID/GID.
Agent containers also run as `NFS.UID:GID`. If these differ, the broker
creates files that the container user cannot write to (or vice versa),
causing permission errors on NFS.

### How to verify

```bash
# On the broker host / container:
id    # should show uid=1000(scion) gid=1000(scion)

# In settings.yaml:
# server:
#   workspace_storage:
#     backend: nfs
#     nfs:
#       uid: 1000
#       gid: 1000
```

### Common issue (NM1 finding)

During the NM1 live gate the broker container ran as `uid=1002` while
`NFS.UID` was `1000`. This caused a UID mismatch requiring a manual
`groupadd`/`usermod` workaround. To prevent this in production:

1. Set the broker container's user to `1000:1000` in the Dockerfile or
   K8s `securityContext.runAsUser/runAsGroup`.
2. Or adjust `NFS.UID/GID` to match the broker service user's identity.

## Mount Privilege

The broker only mounts shares when `server.workspace_storage.nfs.auto_mount`
is `true` (default `false`). With it off, the broker checks the mount table
read-only and an operator (or `/etc/fstab`) mounts the exports, so the broker
needs no mount privilege.

With `auto_mount: true`, the broker mounts shares in a background loop (see
`NFSMountReconciler.Run`) by running `mount -t nfs` directly, so it must run
as root. The reconciler has no `sudo` wrapper. A broker whose default runtime
is Kubernetes or Cloud Run never mounts, even with `auto_mount: true`: the
platform mounts the export into the agent, so the loop only verifies.

Whether a share is mounted is decided from `/proc/mounts`, which cannot block
on a hung mount. `mountpoint(1)` runs only immediately before a mount, as a
guard against mounting over a mount the table did not show. Each command runs
in its own process group, and the whole group (including the `mount.nfs`
helper) is killed when the 90-second timeout or the dispatch request ends.

### Important (NM1b finding): `CAP_SYS_ADMIN` alone is NOT sufficient

The userspace `mount.nfs`/`mount.nfs4` helper **checks `uid == 0` explicitly**
(not Linux capabilities), so granting `CAP_SYS_ADMIN` via `setcap` or a K8s
`securityContext.capabilities` add does **not** let a non-root broker run
`mount -t nfs`. During NM1b the service had to run as `User=root` for the
helper to succeed. To run unprivileged, keep `auto_mount` off and mount the
exports outside the broker. Alternatively, change the reconciler to call the
`mount(2)` syscall directly (which does honor `CAP_SYS_ADMIN`) rather than
shelling out to the `mount.nfs` helper.

## `nosuid,nodev` on every broker mount

Every NFS mount the broker makes carries `nosuid,nodev` as its last options
(`HardenNFSMountOptions`), whatever `mount_options` says and whatever its
origin; it is not configurable. A share
counts as mounted correctly only when `/proc/mounts` shows both flags. The
share's mount point is `<mount_root>/<share id>` with `mount_root` resolved
through symlinks (`NFSShareTarget`), because the mount table lists resolved
paths; the share's own mount point is never stat'ed for this.

- `auto_mount: true` (and not verify-only): a share mounted from the right
  source without them is remounted in place (`mount -o remount,nosuid,nodev`).
  Containers already running keep their existing bind until they restart.
- `auto_mount: false`: the share is reported unhealthy with reason
  `mount_flags`. Mount it yourself with `nosuid,nodev`, for example in
  `/etc/fstab`:
  `10.0.0.2:/scion-workspaces /mnt/nfs/ws1 nfs vers=3,hard,nconnect=4,_netdev,nosuid,nodev 0 0`
- Verify-only (default runtime Kubernetes or Cloud Run): the platform mounts
  the export and takes no options; the broker logs it once and does not
  mark the share unhealthy for it. A dispatch from such a broker to a
  local-container runtime is still refused (see below); that refusal shows
  only in the 503 response and the broker log.

Agent create, start and restart on a local-container runtime re-read the
mount table and are refused (`503 nfs_unavailable`) when the workspace share,
or any mount below it, lacks either flag (from any source), or the table
cannot be read, in every mode. The agent workspace is a Docker bind of a
path under the share; this relies on the kernel copying the source mount's
per-mount flags (`nosuid`, `nodev`) to a new bind mount.

## Config: `schema_version` required (NM1b finding)

`settings.yaml` **must** include `schema_version: "1"` when it contains a
`server.workspace_storage` block. A config without `schema_version` is treated
as legacy and auto-migrated to v1; the legacy→v1 migration does **not** carry
the `workspace_storage` block, so it is silently stripped. Always set:

```yaml
schema_version: "1"
server:
  workspace_storage:
    backend: nfs
    nfs: { ... }
```

## NFSv3 Default

The default `mount_options` is `vers=3,hard,nconnect=4,_netdev`. This
targets Google Cloud Filestore **basic** (BASIC_HDD) tier, which supports
NFSv3 only. NFSv4.1 requires Filestore Enterprise/zonal or a self-hosted
NFS server. Override `mount_options` in `settings.yaml` if using a v4.1-capable
server.
