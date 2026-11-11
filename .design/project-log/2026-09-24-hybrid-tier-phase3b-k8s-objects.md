# Hybrid Deployment Tier — Phase 3b, slice 2: Kubernetes objects (PV, namespace, PVC) and their teardown

Branch `scion/hybrid-tier-p3`, same fork PR as the earlier Phase 3a and 3b slices (`ptone/scion`,
based on `main`).

## Overview

Builds on the previous slice's NFS server and export by adding the Kubernetes side: the
PersistentVolume, namespace, and PersistentVolumeClaim that let GKE pods actually mount the
shared tree, plus their marker-based ownership rules and teardown. Also improves test coverage
for the earlier slice: the remote NFS/squash script and the Cloud Run first-create label decision
are now rendered by pure, directly unit-tested functions instead of living only as inline strings
past the point the wiring test suite can reach.

## Making the NFS/squash script and Cloud Run label decision directly testable

The squash-identity creation and NFS export/server setup are now rendered by two pure functions
in `hybrid-tier.sh` (no gcloud, kubectl, or SSH calls of their own): one renders the identity
script (the `useradd` flags, its exists-guard, the unconditional uid-mismatch assertion, and
reading the ids back), the other renders the export script (the export root's ownership and
mode, the exports file, `exportfs -ra`, and enabling the service). `deploy.sh` sends each
function's output over SSH as-is, and each script is now directly unit-tested. The Cloud Run
first-create label decision is similarly extracted into its own function, tested against the
stub: a not-yet-existing service gets the marker, an existing one doesn't, and a describe error
that isn't NOT_FOUND -- which can't be told apart from "exists" by exit code alone -- fails safe
toward no label, since a missing marker costs nothing to add later while a wrong one on an
unrelated service would not be easily undone.

## Kubernetes objects

A cluster-scoped PersistentVolume named `scion-hub-<hub>-shared` points at the hub VM's NFS
export (server = the VM's internal IP, path = the export root), with `Retain` reclaim policy, an
empty `storageClassName` (a static volume, never dynamically provisioned), and mount options
following the design's own rationale (`nfsvers=4.1`, `hard`, and the attribute/lookup-cache
tuning that bounds cross-runtime staleness). Its `claimRef` is pinned to a specific
namespace/PVC pair so nothing else in the cluster can bind it first. The namespace (config
`gke_target.namespace`, default `scion-hub-<hub>`) and the PVC in it (config
`gke_target.pvc_name`, default `scion-hub-<hub>-shared`, bound to the PV) round out the set. All
three are created once the VM's internal IP is known -- unlike the firewall rules or the NFS
export, the PV's identity depends on it, so this runs later than those, in the phase where the
IP is first read for the Cloud Run proxy.

Every create is marked `scion-deployment=<hub>`. An existing PV or PVC with the target name but
no marker refuses the run instead of adopting it. A marked PV or PVC whose identity has drifted
from what's expected -- the NFS server IP after a VM recreate is the case this actually guards
against -- also refuses, printing the differing fields and a delete-then-recreate remediation,
never auto-corrected, matching the firewall rules' own policy. An existing, unmarked namespace
is used as-is: never labeled, never adopted, never refused, since reusing an existing namespace
without adopting it is allowed.

## Teardown

`--delete` reads `gke_target.name` directly via config lookup rather than the interactive
config-reading path used on create, since a delete run must never prompt to enable the tier:
absent `gke_target.name`, none of this is touched, exactly as inert as the tier-off create path.
When it is set, the cluster is described before anything else is touched: a positive NOT_FOUND
means its Kubernetes objects went with it, so this is reported and teardown continues; any other
failure to reach the cluster aborts the whole teardown before any delete, the same
"uncertainty counts as not gone" rule used throughout this tier's other checks. Otherwise, the
PVC, PV, and namespace are classified found (marked, queued for deletion) or SKIPPED (unmarked);
an unmarked PVC or PV aborts the whole teardown before any delete, the same rule as the firewall
rules, while an unmarked namespace is only skipped, matching the create-side rule that reusing
one is allowed.

Deletion happens first, ahead of every base resource, in the order PVC, then PV, then the
namespace (only if it was marked), stopping at the first failure and recording every object
queued behind it as not attempted. A failure here fails the run's exit code but doesn't block
the unrelated base-resource deletions that follow, matching how a hybrid firewall rule failure
already doesn't block resources that ran before it in the existing sequence.

## Kubeconfig handling

Every kubectl call requires a task-private `KUBECONFIG`, obtained via `gcloud container clusters
get-credentials` into a fresh temporary file -- never the operator's default. kubectl's own
presence is preflighted with an actionable message before that call. One `EXIT` trap, registered
once near the top of the script before either file it covers exists yet, cleans up both this
temp file and the pre-existing SSH stderr scratch file, since `trap` replaces rather than stacks
and a second, later registration would have silently dropped the first.

## Config and docs

Adds `gke_target.namespace` and `gke_target.pvc_name` to the example config and the script's own
field-list comment, both with computed defaults and no dedicated interactive wizard prompt: they
have good defaults, and prompting for them would need threading the hub name into the existing
config-reading function's signature, used by many call sites, for two fields most deployments
won't need to override. The runbook's hybrid-tier section is extended to cover the node-subnet
discovery, the dedicated squash identity, the NFS export, the Kubernetes objects and their
marker/refusal/drift rules, the base resource markers, the API-enablement check, and the
matching teardown ordering. The full documentation pass remains a later slice.

## Tests

Extends the existing two-layer harness with a stub `kubectl` (hermetic, records every invocation
including the `KUBECONFIG` value it ran with, and serves object fixtures from plain JSON files)
and a small helper that turns one of this module's own rendered manifests into the minimal JSON
`kubectl get -o json` would return, so the object-management code's JSON parsing runs unmodified
against the stub. Coverage includes: every manifest's fields (names, markers, server/path,
claimRef pinning, reclaim policy, mount options, access mode); the marker refusal on an unmarked
PV or PVC and the unmarked-namespace-used-not-labeled case; PV and PVC drift detection with
remediation; that matching, marked objects are reused rather than recreated; teardown's
found/SKIPPED classification, its abort rules (unmarked PVC/PV aborts, unmarked namespace
doesn't, an unknown check result aborts), its deletion order and stop-at-first-failure behavior;
and, at the wiring level, the delete-mode tier-off inertness, the cluster-not-found and
cluster-error distinction, and a fully-marked teardown actually deleting all three objects.
Create-mode wiring for the k8s objects (as opposed to the objects' own logic, which is fully
unit-tested) remains covered only at the function level, for the same reason as the NFS/squash
step: it runs well past the point the fast create-mode wiring tests intentionally stop.

Verified against a real bash 3.2.57 build, including the cluster-not-found-continues and
unmarked-PV-aborts teardown scenarios, alongside the full scenario set carried over from the
previous slices.
