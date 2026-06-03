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
startup (see `NFSMountReconciler`). Options:

- Run the broker as root (not recommended for production).
- Grant `CAP_SYS_ADMIN` via `setcap` or K8s `securityContext.capabilities`.
- Configure `/etc/sudoers` to allow the broker user to run `mount`/`umount`
  without a password.

## NFSv3 Default

The default `mount_options` is `vers=3,hard,nconnect=4,_netdev`. This
targets Google Cloud Filestore **basic** (BASIC_HDD) tier, which supports
NFSv3 only. NFSv4.1 requires Filestore Enterprise/zonal or a self-hosted
NFS server. Override `mount_options` in `settings.yaml` if using a v4.1-capable
server.
