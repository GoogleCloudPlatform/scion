# Hybrid Deployment Tier — Phase 3b hardening

Branch `scion/hybrid-tier-p3`, same fork PR as the earlier Phase 3a and 3b slices.

## Overview

A hardening pass over the Phase 3b slices (NFS server, Kubernetes objects,
config/pin/settings/docs), spanning the export filesystem, the Kubernetes ownership checks,
teardown ordering, not-found matching, and several loopback/identity/config hardening items.
Also rewords some earlier comments and this project log's k8s-objects entry in plainer language.

## Export filesystem: a dedicated, loop-mounted, size-capped ext4 volume

The export root (`/srv/scion-shared`) is now the root of its own ext4 filesystem, loop-mounted
from a single image file that lives on the VM's boot disk (`/var/lib/scion-nfs/export.img`,
default 20G, configurable via `gke_target.shared_dir_image_size_gb`), rather than a plain
subdirectory of the boot disk's own root filesystem. With `no_subtree_check` (required so NFSv4
file handles survive a restart) on a plain subdirectory export, a forged file handle could reach
any inode on the whole containing filesystem, not just the exported subtree; giving the export
its own filesystem makes the export root the filesystem root, so there's nothing else on it for a
forged handle to reach. The image is created and formatted only the first time -- an existing
image is never re-created or re-`mkfs`'d -- and the fstab entry carries
`x-systemd.before=nfs-server.service` so the mount is ordered before the NFS server unit starts.
The remote script fails closed: it re-checks `mountpoint -q` after attempting to mount, and
refuses to write or activate the export at all if that check doesn't pass, rather than silently
falling through to serving from the boot disk's root filesystem. Growing the image later is a
manual, documented operation (grow the file, then `resize2fs`); this script never shrinks it, and
teardown leaves the image with the VM's boot disk -- there is no separate cleanup step for it.

Also fixed in the same script: `/etc/exports.d` doesn't exist on a stock jammy install and is now
created before the exports file is written into it; the systemd unit is now enabled under its
canonical name (`nfs-server`), not the Debian package name (`nfs-kernel-server`); NFSv2/v3 and UDP
are disabled via an `/etc/nfs.conf.d` drop-in (this tier is NFSv4/TCP-only) and `rpcbind` is
masked; and a pre-existing `scion-nfs` account (not just a freshly created one) is now validated
for uid ≠ 0, a system-range uid, primary group `scion`, and a `/usr/sbin/nologin` shell, refusing
to proceed if a pre-existing account doesn't meet all four.

## settings.yaml: mount_root/id split, and the PVC name

`mount_root`/`id` now split so `<mount_root>/<id>` resolves back to the export root -- the Docker
broker computes its local mount path as exactly that join, so `mount_root` alone equal to the
export root (the previous behavior) put Docker and GKE on two different trees. The share's
`pv_name` field is consumed as the pod spec's claimName on the Go side, not the PersistentVolume's
own name; the call site now passes the resolved PVC name instead of the PV name, which happened
to coincide for the default names but would have silently diverged for any custom
`gke_target.pvc_name`.

## Kubernetes objects: preflight split, teardown pod check, and stop-at-first-failure

The marker-refusal and non-IP-dependent drift checks for the PV/PVC, plus namespace creation, now
run in a new `hybrid_k8s_preflight`, called right after `hybrid_discover` in Phase 2 -- before the
VM, NFS export, or firewall rules exist -- instead of only in Phase 4, after all of those already
exist. `hybrid_k8s_ensure_objects` (Phase 4) still does the full check plus the IP-dependent
create, so it stays correct even when called on its own. Namespace creation happens only after the
PV/PVC checks pass, so a refusal never leaves a freshly created namespace behind it.

Teardown gained a pod preflight: before queuing a marked PVC for deletion, it now lists the
namespace's pods and refuses if any still mount the claim ("stop agents first"), since
`kubectl delete pvc` blocks indefinitely under its own storage-protection finalizer while a pod
references it -- exactly the GKE agent pods this tier exists to run. Every `kubectl delete` now
also carries an explicit `--timeout`, so a hang from any other cause is treated as a failure
rather than blocking indefinitely.

A hybrid-tier Kubernetes delete failure now stops the *entire* remaining teardown -- Cloud Run,
the VM, NAT, Router, the service account, and the firewall rules are all left untouched, not just
the k8s-object sub-chain -- since the k8s objects are deleted first specifically so a failure
there can still protect everything downstream. The final summary now names every Kubernetes
object (kind/name, not just kind), lists each base resource as Deleted or Kept, and always prints
"Kubernetes cluster: ... never deleted" when `gke_target.name` was checked. An interactive
`--delete` (no `--config`) now prints a note naming the default namespace/PVC/PV and pointing at
`--config`, since it otherwise has no way to know a hybrid tier was ever configured.

`kubectl create -f -` replaces `kubectl apply -f -` for every create-only object (namespace, PV,
PVC), avoiding a silent-adoption race between the absent-check and the write. `kubectl`'s own
preflight now also checks for `gke-gcloud-auth-plugin`, required for GKE authentication but easy
to have missing even when `kubectl` itself is present. The `--delete` path now validates
`gke_target`'s fields (name/location/namespace/pvc regexes, `project == PROJECT_ID`) the same way
the create path already does, via a shared `_hybrid_validate_target_fields`.

## Not-found matching, tightened further

`_hybrid_gcloud_not_found` now requires `code=404`, `HTTP 404`, or a bounded `NOT_FOUND` token --
not a bare `404` substring, which shows up in a cluster literally named with "404" in it, a
project ID containing "404", a proxy's own status line, or a timeout duration. `_hybrid_kubectl_
not_found` now requires the full `Error from server (NotFound): <kind> "<name>" not found` shape,
not just the bare `(NotFound)` reason token, since the API server's own generic "could not find
the requested resource" message carries that same token without confirming the checked object is
actually absent. The Cloud Run first-create label decision does not text-match `describe`'s error
at all: real Cloud Run 404 text -- "Cannot find service [X]" -- carries none of gcloud's usual
NOT_FOUND/404 tokens, so a text-match against it would never actually fire against real gcloud.
It instead uses a positive-absence `run services list --filter=... --format=value(...)` check,
the same pattern already used for the firewall rules' own "confirmed gone" checks.

## Other hardening

- The squash-identity SSH call's stderr is now surfaced on failure instead of discarded, and the
  parsed `uid:gid` output is validated (`^[0-9]+:[0-9]+$`, uid ≠ 0) before it's embedded in the
  export line.
- `container_images.registry` is now refused when it names this VM itself under any loopback
  spelling (`localhost`, `127.0.0.0/8`, `[::1]`, `0.0.0.0`, with or without a port), not just a
  bare `localhost/` prefix.
- The tier-on required-API list failure now includes gcloud's own stderr instead of discarding it.
- `docs/deploy/agent-runbook-single-node-vm.md` documents `kubectl`/`gke-gcloud-auth-plugin` as
  prerequisites, the two-part (preflight/create) Kubernetes object check, the teardown pod
  preflight and bounded deletes, the stop-at-first-failure rule, and the dedicated export
  filesystem.

## Test harness

Extends past the point every previous create-mode wiring test stopped at (the VM-exists
sentinel): the stub `gcloud` can now actually answer `compute ssh`/`compute scp` (opt-in via
`GCLOUD_STUB_SSH_SUCCEEDS=true`, so every pre-existing test keeps its old fast-fail behavior by
default) with canned answers for the handful of commands deploy.sh's own variables depend on, and
a new sentinel at the settings.yaml dev-mode write lets a tier-on and a tier-off create run stop
there and assert on the *real* rendered NFS/export SSH commands and settings.yaml heredoc, not
just the isolated render functions. The rendered NFS export script also gets an execution-based
probe (fake `mkfs.ext4`/`fallocate`/`mountpoint`/`mount`/`tee`/etc. on `PATH`, operating only on a
throwaway temp path -- never a real absolute system path) proving the create-once and fail-closed
behaviors hold at runtime, not just in the rendered text. Realistic not-found fixtures (drawn from
actual gcloud/kubectl error corpora, not invented text) replace or supplement the earlier
substring-based ones. `tests/run.sh` now exports a sentinel, deliberately-bogus `KUBECONFIG`
globally, so every test -- not just ones that explicitly set it -- proves it never falls back to
an ambient value.
