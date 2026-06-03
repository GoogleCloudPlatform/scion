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

The broker process requires mount privilege to auto-mount NFS shares at
startup (see `NFSMountReconciler`). Options, in order of preference:

- Configure `/etc/sudoers` to allow the broker user to run `mount`/`umount`
  without a password, and have the reconciler invoke `sudo mount` (recommended
  for a non-root service).
- Run the broker as root.

### Important (NM1b finding): `CAP_SYS_ADMIN` alone is NOT sufficient

The userspace `mount.nfs`/`mount.nfs4` helper **checks `uid == 0` explicitly**
(not Linux capabilities), so granting `CAP_SYS_ADMIN` via `setcap` or a K8s
`securityContext.capabilities` add does **not** let a non-root broker run
`mount -t nfs`. During NM1b the service had to run as `User=root` for the
helper to succeed. To run unprivileged, use the `sudo mount` wrapper above, or
have the reconciler call the `mount(2)` syscall directly (which does honor
`CAP_SYS_ADMIN`) rather than shelling out to the `mount.nfs` helper.

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
