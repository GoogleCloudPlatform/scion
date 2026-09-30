# Hybrid Deployment Tier — Phase 3b, slice 1: NFS server, squash identity, node subnet, API check, base markers

Branch `scion/hybrid-tier-p3`, same fork PR as Phase 3a (`ptone/scion`, based on `main`).

## Overview

This slice builds on Phase 3a's config parsing, cluster discovery, and NFS firewall rules by
adding the pieces needed for the shared tree itself to actually be servable: the NFS server and
export on the hub VM, the dedicated squash identity that export uses, and discovery of the
GKE cluster's node subnet so the export's client list is scoped correctly. It also lands two
small, deliberately tier-independent fixes: an API-enablement check that only enables what's
actually missing, and ownership markers on every base resource this script creates fresh.
Out of scope for this slice: the PV/PVC and namespace objects, the settings.yaml write blocks,
the hub reachability pieces, and the docs pass -- all follow in later slices on this same branch.

## Node subnet discovery

Discovery (already reading the cluster's own `describe` response for its network and node
tag) now also reads the cluster's `subnetwork` field, resolves the cluster's location to a
region (a zonal location like `us-central1-a` becomes `us-central1`; a regional location is
already one), and describes that subnetwork to read its primary IP range. The range is
validated as a well-formed IPv4 network with no host bits set, and refused outright -- before
anything is created, naming the cluster and subnet -- if it's `0.0.0.0/0` or broader than `/8`.
This CIDR, not the pod CIDR or any secondary range, is what the NFS export's client list uses:
nodes, not pods, originate the NFS mount traffic that reaches the VM.

## NFS server, squash identity, and export

A dedicated system account (no login shell, no home) provides the NFS export's `all_squash`
identity, with its primary group set to the existing `scion` group rather than a new group of
its own: the shared tree's leaf ACL already grants that group directly, so a different group
for the squash identity would make one runtime's writes invisible to the other's. Its uid is
system-allocated and read back numerically once created; the deploy asserts it differs from
the `scion` (broker) user's own uid and refuses to continue otherwise, since squashing every
NFS client to the broker's own identity would let any pod that can reach the export act as the
broker on the shared tree.

The export root is a plain directory on the boot disk, owned `scion:scion`, mode 2755 so the
squash uid can't write it. `nfs-kernel-server` is installed and a per-hub file under
`/etc/exports.d/` (so multiple hubs never collide) lists only the node subnet CIDR, with
`all_squash`, `sync`, `no_subtree_check`, `sec=sys`, and the squash uid/scion gid as
`anonuid`/`anongid`. The export's `fsid` is a UUID derived deterministically from the hub name,
stable across re-runs without recording a separately-generated value anywhere. The line itself
is rendered by a pure function with no gcloud or SSH calls, directly unit-tested; the actual
squash-identity creation, package install, file write, and `exportfs -ra` happen over SSH,
gated on the tier being enabled, once cloud-init has finished creating the `scion` user and
group the squash identity and export ownership depend on. This step is kept out of
`cloud-init.yaml`, which every deployment shares, so the tier-off path stays byte-for-byte
unchanged. It's safe to redo on every re-run: the export file is always rewritten and
re-exported, which is how it picks up a changed node subnet CIDR with no separate drift
detection needed. There's no separate teardown for the export -- it dies with the VM.

## Two deliberate tier-off changes

Everything above is gated on the hybrid tier being enabled. Two changes in this slice apply
regardless:

- **API enablement** now lists what's already enabled first and enables only the difference,
  instead of unconditionally enabling the full list on every run. Some validation runners don't
  hold the permission to enable an API and would fail even on a no-op enable call for one that's
  already on. The hybrid tier adds `container.googleapis.com` to the required set. If the list
  call itself fails, the tier-off path falls back to today's unconditional enable of the base
  list (so it keeps working everywhere it always has), while the tier-on path fails with an
  actionable message instead of guessing whether `container.googleapis.com` needs enabling.
- **Base resource markers**: every base resource this script creates fresh (not adopts) now
  carries a `scion-deployment=<hub>` marker -- a label on the VM and, on first create only, the
  Cloud Run IAP proxy service; a description on the service account and Cloud Router (appended
  to the IAP SSH firewall rule's existing description, not replacing it). This is purely
  additive: it's never checked, never used to decide adoption, and never applied to an
  already-existing resource. NAT gets no marker of its own, since it has no identity
  independent of its router. Base-resource adoption and teardown behavior are exactly what they
  were before this.

## Tests

Extends the existing two-layer harness (function-level tests against `hybrid-tier.sh`, and
wiring tests that run `deploy.sh` itself as a real subprocess against the stub). New coverage:
the node subnet CIDR for both a Standard and an Autopilot cluster fixture, the zonal-to-region
resolution, an unreadable subnet, and the `/0` and broader-than-`/8` refusals plus the `/8`
boundary itself; the export-line and fsid renderers directly, including that different hubs
never collide on the same fsid; the API check's zero-enable-calls, exact-missing-set,
tier-on-adds-container, and both list-failure paths; and the base markers present on every
fresh create and absent on every adopt/existing-resource path (including new stub support for
a pre-existing router or service account, which no prior test exercised).

The squash-identity creation and NFS server/export step itself runs after the point the
existing create-mode wiring tests intentionally stop (a sentinel at the VM-exists check, kept
there to avoid the real SSH-readiness retry loop's sleeps) -- the same constraint that already
applies to cloud-init-wait and the systemd unit install earlier in the script. It's tier-gated
with the same `if HYBRID_ENABLED` idiom already covered elsewhere in the wiring tests, and its
only pure, directly testable logic -- the export line and fsid renderers -- has direct unit
tests. The Cloud Run marker (Phase 4 of `deploy.sh`, well past this point) has the same
constraint.

Also fixed: the interactive no-Python teardown test now asserts the ownership list call
actually ran, and a matching interactive test covers an unmarked same-name rule aborting before
any delete; the stop-at-first-failure teardown test asserts the exact queued-rule message and
which rule is recorded; the unreachable-zone test also asserts the "could not confirm" wording.

Both `deploy.sh` and `hybrid-tier.sh` remain bash-3.2-compatible; verified against a real
3.2.57 build across ten scenarios covering tier-off and tier-on create, redeploy, delete, the
VM-delete-failure and unreachable-zone teardown paths, and the API check's missing-set
behavior.
