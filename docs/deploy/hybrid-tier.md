# Hybrid Deployment Tier (VM Hub + Docker Broker + GKE Target + NFS)

## Overview

The hybrid tier extends the [Single-Node VM deployment](single-node-vm.md) with a
second place to run agents: a GKE cluster (`gke` runtime profile), alongside the
VM's existing co-located Docker broker. Both runtimes share project scratchpads
through `server.shared_dir_storage: nfs` — a new, decoupled-from-workspaces
setting that points both the Docker bind mount and the Kubernetes PVC `subPath`
at the same NFS-served directory tree on the VM.

This tier keeps the single-node hub's SQLite store and its Docker broker
unchanged. It adds:

- an NFS export (`knfsd`) served from a plain directory on the VM's boot disk;
- a second broker runtime profile, `type: kubernetes`, pointed at the GKE
  cluster;
- one static `PersistentVolume`/`PersistentVolumeClaim` pair in the agent
  namespace, applied once by the operator (or, in Phase 3, by `deploy.sh`).

**This tier requires a co-located broker**: the VM's Docker broker process
must have direct local filesystem access to the NFS export (i.e. it can
`stat` the resolved host base itself), because the upper-directory hardening
(mode + ACL, below) only runs when the broker can see and manage that
directory tree directly. A broker that has no local copy of the export —
for example a hypothetical future in-cluster GKE broker — is **not
supported** by `shared_dir_storage: nfs`: `resolveSharedDirs` refuses,
naming the missing path, rather than letting Kubernetes create the `subPath`
itself. (Kubelet would create a missing `subPath` as the export's squashed
anonymous identity on an `all_squash` export — exactly the identity the
upper-directory hardening exists to keep out — so silently tolerating a
missing host base there would quietly defeat it.) This applies to both
runtimes this tier wires up: the co-located Docker broker always has local
access by construction, and the `gke` runtime profile's pods only ever reach
the export via the PVC/`subPath` that same co-located broker's host base
resolves — there is no in-cluster broker process in this tier.

This page summarizes what an operator running or maintaining this tier needs
to know.

## Settings

```yaml
# ~/.scion/settings.yaml (global) on the broker
schema_version: "1"
server:
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: /srv              # where THIS broker sees the share root
      shares:
        - id: scion-shared          # host base becomes <mount_root>/<id>
          server: <nfs-server-ip>   # informational; used verbatim in the PV manifest
          export: /srv/scion-shared
          pv_name: scion-shared     # k8s PVC claim name in the agent namespace
      subpath_root: projects        # default "projects"
runtimes:
  gke:
    type: kubernetes
    context: gke_<project>_<region>_<cluster>
    namespace: scion-agents
    gke: true
profiles:
  gke:
    runtime: gke
```

`server.shared_dir_storage` is Layer-0: global-only, never written to the DB,
and requires a broker restart to take effect. `uid`, `gid`, `mount_options`,
and `storage_class` on the `nfs` block are meaningful for
`server.workspace_storage` but are **not used** by `shared_dir_storage` — the
broker logs a one-time warning at startup if any of them are set, and logs the
resolved layout (`backend`, `host_base`, `subpath_root`, `pv_name`) exactly
once per start.

The hub's own shared-dir file browser (project pages in the web UI) uses this
same setting and the same confined resolver, so `scratchpad` listings work
whether the project is browsed via a co-located Docker broker or lives only on
GKE. When the setting is unset or `local`, both the broker and the hub file
browser behave exactly as they do today — this is an additive, opt-in feature.

## Security posture

NFS authentication is `sec=sys`: none. The server trusts the numeric uid the
client sends, but this doesn't matter in practice because every client is
squashed to a fixed identity (see below). Authorization is by source network
only, enforced at two layers: the export's client list and a VPC firewall
rule.

> **Network shape:** a static `spec.nfs` `PersistentVolume` is mounted by
> **kubelet on the node**, from the node's own network namespace — not by the
> pod. The NFS server therefore always sees the **node's primary IP**, never a
> pod IP, regardless of the cluster's egress-NAT policy. An export or firewall
> rule scoped to the pod CIDR fails outright with `access denied by server`.
>
> This means:
> - **Export client list:** the GKE **node subnet** (e.g. `10.128.0.0/20`), not
>   the pod CIDR.
> - **Firewall:** allow `tcp:2049` with `--source-tags <gke-node-network-tag>`
>   to a target tag (e.g. `scion-nfs`) at **priority 900**, plus an explicit
>   **DENY** for `tcp:2049` from `0.0.0.0/0` to that same target tag at
>   **priority 950**. The deny rule matters: without it, a network's broad
>   `default-allow-internal` rule (typically priority 65534) re-opens port 2049
>   to every VM in the VPC, silently widening the intended node-subnet-only
>   access.
> - **Net effect on the security posture:** *better* in one respect — pods can
>   no longer reach `nfsd` directly at all; only kubelet mounts, and only for a
>   pod spec that names the PV/PVC. *Weaker* in another — at the nfsd layer,
>   any host in the node subnet is trusted, not just the specific nodes running
>   scion agent pods. The firewall narrows this to nodes carrying the GKE node
>   network tag; on a network without a broad allow-internal rule, the deny
>   rule at 950 is harmless defense in depth.
> - For a future `deploy.sh` tier option (Phase 3), the node subnet comes from
>   the cluster's own subnetwork, and the node tag from the cluster's node
>   pool / Autopilot node tag.

Project isolation between agents is a **path convention** (each project's
`subPath` under one shared PVC), not a storage boundary: any pod spec in the
agent namespace that names the shared PVC can mount the **entire** export, not
just its own project's subtree. This holds only because agents do not author
pod specs or hold Kubernetes API credentials — the broker's kubeconfig lives on
the VM, never in a pod. **Treat the GKE cluster used for this tier as
trusted**: any workload with Kubernetes API rights to create a pod in the agent
namespace, or any operator with `kubectl` access to that namespace, can read or
write every project's shared directory on the export.

Traffic is cleartext inside the VPC. There is no encryption in transit beyond
GCP's VPC-level protections, and no Kerberos / NFS-over-TLS on the target
kernel/Ubuntu release.

### E2 hardening: dedicated squash identity + default ACL

Phase 1 squashes every NFS client to the **broker's own uid** (`all_squash,
anonuid=<broker-uid>,anongid=<broker-gid>`). That has an accepted residual,
tracked as `ptone/scion#1794`: an NFS client that is indistinguishable, at the
filesystem layer, from the broker itself can write anywhere the broker can —
including planting a symlink above a project's own leaf directory, which could
redirect a later Docker bind mount (dockerd re-resolves the bind path at
container create, after the broker's own checks — this specific race is a
**separate, still-open residual**).

Phase 2 ships the **code** side of closing the upper-directory part of this:

- Every intermediate directory the broker creates (`<subpath_root>/`,
  `<pid>/`, `shared-dirs/`) is `2755` — setgid, but **not** group-writable.
- Each shared dir's own leaf directory stays `2775` (setgid, group-writable),
  **plus** a minimal default POSIX ACL (`u::rwx,g::rwx,o::r-x`, inherited by
  every new child) is set on it when the broker creates it. This closes a
  concrete finding from UAT: a file created by a *root*-owned process inside a
  Docker container (a common container-entrypoint pattern) comes out
  `0:<group> 0644` — group has read only, no write — so a GKE pod squashed to
  a *different* uid in the same group couldn't write it. The default ACL fixes
  this **for files created with a permissive requested mode** (the common
  case, e.g. a plain `0666`/`0777` create, which is what the umask mechanism
  assumes). It does **not** override a creator that explicitly requests a
  restrictive mode (an explicit `0644` stays `0644` — POSIX ACL inheritance
  only grants what the requested mode also allows).
  - The ACL is set with `Fsetxattr` on the export's local ext4 filesystem —
    that's what the broker's own process has open, since it's the one doing
    the write. `ENOTSUP`/`EOPNOTSUPP` (falls back to plain `2775`, no ACL,
    with one warning logged per process — not a startup failure) happens
    when the host base itself is a mount the broker reaches only as an NFS
    *client* (an NFSv4 client can't set `system.posix_acl_*` at all), or
    when the underlying filesystem otherwise lacks ACL support. This tier's
    own target — the broker writing its own export's ext4 directly, with
    knfsd applying inheritance to NFS clients server-side — always
    supports it; the ACL layer isn't a property of the NFS protocol version
    NFS clients happen to use.

**This code is not sufficient on its own, and `ptone/scion#1794` is not
closed by it.** The upper-directory hardening only actually blocks a
malicious NFS client when the export's `anonuid`/`anongid` is a **dedicated
identity distinct from the broker's own uid** — otherwise a squashed client is
still owner-equivalent on those directories regardless of their mode.
If the export squashes to the broker's own uid, this hardening is inert.
Closing the residual end to end requires:

1. provisioning a dedicated NFS squash uid, in the `scion` group but distinct
   from the broker's own uid, and pointing the export's `anonuid=`/`anongid=`
   at it — infrastructure work, not broker code, tracked for the Phase 3
   `deploy.sh` tier option (new deployments) and as a separate ops action for
   existing deployments;
2. verifying end to end in a scratch-project validation with that dedicated
   identity in place.

Until both are done, treat `ptone/scion#1794` as **open**.

**Existing directories created under Phase 1** (mode `2775`, no ACL, before
this hardening shipped) are not retroactively fixed by an automatic migration
— consistent with this feature's own "no automatic migration" precedent (see
Migration below). If you need to harden an existing tree by hand, the recipe
below fixes it in place.

**Use an fd-based, top-down approach, matching the broker's own `EnsureLeaf`
code.** The recipe below opens every component with `O_NOFOLLOW|O_DIRECTORY`
and `fchmod`s/`setxattr`s the resulting file descriptor, so hardening a
directory never depends on a path still pointing at the same place it did
when it was found -- there is no separate resolve-then-act step for a
concurrent change to land in.

**First, if any directory in the tree might be owned by the squashed NFS
identity rather than the broker uid** (possible wherever an upper directory
was still group-writable, which is exactly what this recipe is fixing): as
root, `chown` it to the broker uid first. A mode change alone does not help
against a mismatched *owner* — the broker's own writes to these directories
rely on being the owner, not just a group member.

Run the script itself as the **broker uid**, not root, against the resolved
host base and the configured `subpath_root` (default `projects`; the walk
handles a multi-component value like `tier/projects` too, refusing at
whichever component is a symlink):

```bash
sudo -u scion python3 fix_shared_dir_modes.py /srv/scion-shared projects
```

```python
#!/usr/bin/env python3
"""fd-based, top-down, symlink-safe fix-up for shared_dir_storage=nfs trees
created before Phase 2's leaf modes/ACL hardening shipped. Run as the broker uid (the
export's owner), never as root, and never against a directory that hasn't
first been chowned to the broker uid if it might be owned by the squashed
NFS identity (a mode change alone does not help against a mismatched owner).

Best-effort: a PermissionError fchmod'ing a directory, or any OSError
setxattr'ing a leaf (ENOTSUP/EOPNOTSUPP included, but not only those), is
reported to stderr and does not abort the rest of the walk -- siblings and
the remaining tree still get fixed. The process exit code reflects this: 0
if nothing was skipped, 3 if one or more directories were (see the summary
line printed either way), 1 if subpath_root itself couldn't be walked at
all, 2 for a usage error.

Usage: fix_shared_dir_modes.py <mount_root>/<share_id> <subpath_root>
"""
import errno
import os
import sys

ACL_DEFAULT_MINIMAL = (
    b"\x02\x00\x00\x00"                  # acl_ea_header, version 2
    b"\x01\x00\x07\x00\xff\xff\xff\xff"  # ACL_USER_OBJ, rwx, undefined id
    b"\x04\x00\x07\x00\xff\xff\xff\xff"  # ACL_GROUP_OBJ, rwx, undefined id
    b"\x20\x00\x05\x00\xff\xff\xff\xff"  # ACL_OTHER, r-x, undefined id
)

F = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW

warning_count = 0


def warn(label, msg):
    global warning_count
    warning_count += 1
    print(f"warning: {label}: {msg}", file=sys.stderr)


def chmod_reporting(dfd, mode, label):
    # A permission failure fixing ONE directory's mode is reported and
    # skipped rather than aborting the whole recipe -- the rest of the tree
    # (siblings, and anything below a directory that DID get fixed) is still
    # worth fixing even if, say, an ownership mismatch the operator hasn't
    # chowned yet blocks one subtree.
    try:
        os.fchmod(dfd, mode)
    except PermissionError as e:
        warn(label, f"could not fchmod to {oct(mode)}: {e}")


def fix(dfd, depth, label):
    # depth 0 = subpath_root's own final component, 1 = <pid>, 2 =
    # shared-dirs, 3 = leaf. Every subpath_root component BEFORE the final
    # one is fixed in main()'s own walk below, since it's outside this
    # recursion (which only ever starts at the final component).
    if depth < 3:
        chmod_reporting(dfd, 0o2755, label)  # upper dirs: never group- or other-writable
    else:
        chmod_reporting(dfd, 0o2775, label)
        # setxattr accepts an open fd directly (no /proc/self/fd, no
        # shelling out to setfacl): both ACL types are set on the fd that
        # was itself opened with O_NOFOLLOW, so this cannot be raced onto a
        # different inode via a symlink swap.
        try:
            os.setxattr(dfd, "system.posix_acl_access", ACL_DEFAULT_MINIMAL)
            os.setxattr(dfd, "system.posix_acl_default", ACL_DEFAULT_MINIMAL)
        except OSError as e:
            # Same policy as chmod_reporting above: report this ONE
            # directory's ACL failure and move on, rather than aborting the
            # rest of the tree over it.
            if e.errno in (errno.ENOTSUP, errno.EOPNOTSUPP):
                warn(label, "export does not support POSIX ACLs; left as plain 2775, no ACL")
            else:
                warn(label, f"could not set default ACL: {e}")
    if depth == 3:
        return
    for name in os.listdir(dfd):
        if depth == 1 and name != "shared-dirs":
            continue
        try:
            # O_NOFOLLOW: a symlink here raises ELOOP; a non-directory
            # raises ENOTDIR. Either way it's skipped, never touched.
            c = os.open(name, F, dir_fd=dfd)
        except OSError:
            continue
        try:
            fix(c, depth + 1, f"{label}/{name}")
        finally:
            os.close(c)


def main():
    if len(sys.argv) != 3:
        print(f"usage: {sys.argv[0]} <mount_root>/<share_id> <subpath_root>", file=sys.stderr)
        return 2
    base, sub = sys.argv[1], sys.argv[2]
    b = os.open(base, os.O_RDONLY | os.O_DIRECTORY)
    try:
        cur = b
        opened = []
        components = sub.split("/")
        for i, comp in enumerate(components):
            try:
                nxt = os.open(comp, F, dir_fd=cur)
            except OSError as e:
                print(f"refusing: subpath_root component {comp!r} is a symlink or not a directory: {e}",
                      file=sys.stderr)
                for fd in reversed(opened):
                    os.close(fd)
                return 1
            opened.append(nxt)
            cur = nxt
            # Every subpath_root component gets fixed the same way as the
            # pid/shared-dirs levels below it, including a non-final one
            # (e.g. "tier" in "tier/projects") -- fix()'s own depth-0 case
            # only ever sees the LAST component, the one handed to it below.
            chmod_reporting(nxt, 0o2755, "/".join(components[: i + 1]))
        try:
            fix(cur, 0, sub)
        finally:
            for fd in reversed(opened):
                os.close(fd)
    finally:
        os.close(b)
    if warning_count:
        print(f"done with {warning_count} warning(s), see above", file=sys.stderr)
        return 3
    print("done, no warnings", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

### Shared-dir removal

With `shared_dir_storage: nfs`, both project deletion and single-shared-dir
removal act on the export, not just the database:

- **Deleting a project** removes that project's entire
  `<subpath_root>/<project-id>/shared-dirs` tree from the export (and the
  now-empty `<project-id>` directory alongside it), via the same fd-based,
  symlink-safe walk used everywhere else in this tier.
- **Removing a shared directory from a project**
  (`DELETE /projects/{id}/shared-dirs/{name}`) also deletes that shared
  dir's directory and all of its contents from the export, via the same
  fd-based walk, leaving the project's other shared dirs and structural
  directories untouched.

Both are best-effort: a failure removing the export-side tree is logged, not
surfaced to the caller, and never blocks or rolls back the corresponding
database change.

## Boot-disk trade-offs

The NFS export (`/srv/scion-shared`) is a plain directory on the VM's **boot
disk**, not a separate mounted volume. This is by design; the trade-offs are
real and should inform how you operate this tier:

| Concern | Consequence | Mitigation |
|---|---|---|
| **Fill risk** | The OS, container images/layers, agent workspaces, SQLite (`hub.db`), and the NFS export all share one filesystem. A runaway agent writing to the scratchpad, from either runtime, can fill `/` — which breaks SQLite writes, dockerd, and journald together, taking down the whole hub, not just the scratchpad. | Alert on `/` usage above 80%. Periodically check `du -sh /srv/scion-shared/projects/*` for outliers. There is no reserved-capacity protection for the export specifically. |
| **Snapshots** | A boot-disk snapshot captures the OS, the hub DB, and the scratchpad together. You cannot roll back the scratchpad independently, and restores are all-or-nothing. | Accept this for scratch data. To recover a single file, mount a snapshot-derived disk on another VM and copy it out. |
| **Lifecycle** | Tearing down the VM (and its boot disk) deletes all shared-dir data with it. Recreating the VM starts with an empty scratchpad. | Know this before tearing down a VM that has scratchpad data you care about. A dedicated persistent disk for the export is the upgrade path if this becomes unacceptable, but is not built in this phase. |
| **I/O contention** | NFS clients, container overlay I/O, and SQLite all compete for the same disk. A low-baseline-IOPS boot disk (e.g. `pd-standard`) makes this worse. | Acceptable for scratch data; prefer `pd-balanced` or better for the boot disk if this tier sees heavy shared-dir traffic. The boot disk can be grown online (grow the PD, then `growpart` + `resize2fs`). |
| **No mount boundary** | There's no separate mountpoint to fail to mount (a small upside), but also no size isolation between the export and everything else on `/`. | — |

## Reboot, stale-handle, and cross-runtime visibility caveats

- **Reboots block, they don't lose data.** During a VM reboot, GKE agents'
  I/O against their shared dirs **blocks** (the PV uses `hard` mounts) and
  resumes after the VM comes back up plus the NFSv4 grace period (~90s). The
  hub itself is down for the same window, so this isn't a new failure domain
  — but unattended kernel-upgrade reboots on the VM will trigger it. Schedule
  a maintenance window if you enable unattended upgrades.
- **`kubectl delete` can hang.** Deleting a pod while the NFS server is
  unreachable can leave it `Terminating` until the server returns.
- **ESTALE is rare, not a reboot symptom.** It only happens if the exported
  directory itself is deleted and recreated, or the filesystem's identity
  changes underneath the export — not on a plain reboot. If it happens,
  restart the affected pods.
- **Visibility is asymmetric across runtimes, by design (no loopback NFS on
  the Docker side).** Docker agents write directly to local
  disk and see their own writes immediately; other Docker agents on the same
  VM see them immediately too. GKE pods see VM-side writes after
  close-to-open revalidation or attribute-cache expiry — bounded to about 3
  seconds by this tier's `actimeo=3` mount option, rather than the NFS
  default's up-to-60-second staleness window.
- **`inotify`/file-watching is not a cross-runtime signal.** A watcher
  running inside a GKE pod never sees changes made by the Docker side (or by
  any other NFS client) — this is standard NFS client behavior, not specific
  to this tier. Do not build any cross-runtime coordination that assumes a
  file-system watch fires for a write made by the other runtime; poll, or use
  an explicit signal instead.
- **`all_squash` affects only NFS clients**, i.e. only the GKE side. It has
  no effect on the Docker broker's own uid, which is why the broker's own uid
  is what the export squashes *to*, not the other way round.

## Migration (enabling this on an existing deployment)

`shared_dir_storage` is opt-in and additive. When it's unset or `local`,
behavior is byte-identical to today — no existing single-node, HA, or
`workspace_storage: nfs` deployment is affected by this feature existing.

When you enable it on a deployment that already has projects with scratchpad
data:

- **Existing scratchpads stay in their legacy location.** New agents see an
  **empty** directory under the new NFS layout. There is no automatic
  migration — copying on first use would race with any agent that's already
  running, and can involve a lot of data.
- To migrate existing data by hand, **stop the hub first**, then for each
  project:

  ```bash
  rsync -a ~/.scion/project-configs/<slug>__<uuid>/shared-dirs/<name>/ \
           /srv/scion-shared/projects/<project-id>/shared-dirs/<name>/
  ```

  The project ID for a given slug is available from `scion project list
  --json` or the hub API (it's the same hub-assigned ID injected into every
  agent dispatch as `SCION_PROJECT_ID`, not the broker-local `<slug>__<uuid>`
  directory name).
- **Rollback is symmetric:** remove the `shared_dir_storage` block and
  restart. Agents go back to reading/writing the legacy local directories.
  Anything written under the NFS export while the setting was enabled stays
  there and can be copied back the same way.

## Known limits (tracked, pre-existing, out of scope for this tier's own code)

These were all found during hybrid-tier UAT but are pre-existing gaps, not
regressions introduced by this feature. They matter operationally for anyone
running this tier:

- **`ptone/scion#1799`** — a profile's `harness_overrides.<harness>.image`
  loses to the template/agent's own image default, so **pinning an image
  through a settings profile does nothing today**. If you need a specific,
  reproducible image for this tier (recommended, since GKE and Docker agents
  should run the same build to keep behavior comparable across runtimes), pin
  it via `--image <digest>` at agent start, or bake the pin into the template
  itself — not via a profile.
- **`ptone/scion#1800`** — registering or linking a project rewrites the
  entire global `settings.yaml` (key order changes, zero-value fields get
  serialized, and any key the settings struct doesn't know about is dropped).
  If you hand-maintain comments or extra keys in your global settings file,
  expect them to disappear the next time a project is registered.
- **`ptone/scion#1801`** — GKE agent pods run as the namespace's **default**
  Kubernetes service account, with its token automounted, because the k8s
  runtime doesn't set `automountServiceAccountToken: false` or require a
  dedicated service account. Exposure depends entirely on what RBAC exists in
  your agent namespace; review it for this tier's cluster specifically.
- **With `shared_dir_storage: nfs`, chat attachments aren't staged into
  NFS-backed shared dirs in this release.** Plan around this for projects on
  the NFS tier until a confined NFS attachment path is built.

**Open question, not built:** the auxiliary Kubernetes runtime's behavior when
GKE credentials are broken or unreachable at hub startup (whether it degrades
gracefully to Docker-only, or blocks/delays hub startup) was flagged as an
open question in the design and was not independently verified during
Phase 1 UAT (the GKE dispatch path was exercised with working credentials
only). Until this is verified, assume broken GKE credentials on a hybrid-tier
broker could affect hub startup, and test credential rotation/expiry in a
non-production environment first.

## References

- `ptone/scion#1777` — the tracking issue for the hybrid tier work.
- `ptone/scion#1794` — E2 hardening (dedicated squash uid), required before
  the Phase 3 `deploy.sh` tier option ships.
- `ptone/scion#1802` — shared-dir cleanup on project deletion (the NFS side
  is handled starting Phase 2; the local backend and workspace directory
  cleanup remain tracked on that issue).
- `ptone/scion#1799`, `ptone/scion#1800`, `ptone/scion#1801` — known limits,
  above.
