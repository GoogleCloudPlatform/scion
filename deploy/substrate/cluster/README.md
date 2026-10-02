# Cluster prerequisites for the Substrate broker

This directory holds the cluster-level manifests an operator applies
**before** `deploy/substrate/broker.yaml` — the WorkerPool actors run on, and
the NetworkPolicy objects Substrate's threat model depends on. It does not
install Agent Substrate itself; that is a separate step, using Substrate's
own tooling (see "Install order" below).

## What's here

| File | What it is | When to apply |
|---|---|---|
| `workerpool.yaml` | A `WorkerPool` (Substrate CRD `ate.dev/v1alpha1`) dedicated to scion actors, sized for a Claude Code-class workload, tolerating the gVisor node taint. | After Substrate is installed (the CRD must already exist), before the broker's first `scion start`. |
| `networkpolicy.yaml` | Three `NetworkPolicy` objects: the router ingress restriction (a cluster/`ate-system` prerequisite, not broker-specific — see `../broker.yaml`), and a default-deny ingress baseline each for the worker namespace and the broker namespace. See "NetworkPolicy objects" below. | After NetworkPolicy enforcement is enabled on the cluster (below), alongside the broker deploy. |

## Cluster requirements (not manifests — cluster/project setup)

These aren't Kubernetes objects this folder can apply; they're GKE/cluster
properties to get right before Substrate is installed at all.

- **GKE version**: Substrate's control-plane objects (`PodCertificateRequest`,
  `ClusterTrustBundle`) used the `certificates.k8s.io/v1beta1` API on the
  version this was built against. GKE 1.37 serves only `v1` — check which
  API version your target Substrate commit uses before picking a cluster
  version. On GKE 1.36, the `v1beta1` APIs must be enabled **at cluster
  creation**; this is not fixable on an existing cluster. Check what your
  cluster actually serves with:
  ```sh
  kubectl api-resources --api-group=certificates.k8s.io
  ```
- **Two node pools**: a small system pool for Substrate's own control-plane
  workloads (`ate-api-server`, `ate-controller`, its Postgres, `atenet-*`),
  and a separate **gVisor sandbox pool** for actor workers. Don't run actor
  workers on the system pool.
- **gVisor node pool — single CPU platform.** gVisor checkpoint/restore
  (used for actor suspend/resume) requires the restoring node to have a CPU
  feature superset of the node the snapshot was taken on. A multi-zone pool
  on a machine type whose zones map to different CPU platforms (observed:
  an `e2-standard-8` pool split Intel Broadwell / AMD Rome across zones)
  makes roughly half of all warm resumes fail with
  `runsc restore: exit status 128` / `incompatible FeatureSet: missing
  features: ...` — a real, reproduced failure, not a theoretical one. Use a
  single-CPU-platform machine type (e.g. a `c3-*` shape, which is
  Intel Sapphire Rapids across every zone GCE offers it in) instead of an
  `e2-*`/`n2-*` shape whose platform mix varies by zone, and verify every
  node lands on the same platform before trusting cross-node resume:
  ```sh
  gcloud compute instances describe <node> --zone=<zone> --format='value(cpuPlatform)'
  ```
  `--min-cpu-platform` is not a substitute: "minimum" still allows a newer
  platform on some nodes, which reproduces the same mismatch.
- **gVisor node pool — no auto-upgrade, no auto-repair, no spot/preemptible.**
  Auto-upgrade evicts running pods on GKE's schedule; an actor gets a SIGTERM
  and a 30-minute grace window to suspend before it's force-terminated into a
  terminal `CRASHED` state with no snapshot taken. Spot/preemptible nodes can
  be reclaimed on the same short notice. Create the pool with
  `--no-enable-autoupgrade --no-enable-autorepair`, no `--spot`/`--preemptible`.
- **gVisor node pool — the GKE Sandbox taint.** GKE puts
  `sandbox.gke.io/runtime=gvisor:NoSchedule` on every gVisor-enabled node.
  Substrate's own `atelet` DaemonSet needs a matching toleration patched in
  as part of the Substrate install (not something this folder's manifests
  control); `workerpool.yaml` here carries the same toleration for the
  worker pods it creates. Skipping either leaves that workload unschedulable
  or silently stuck.
- **A version/identity label on the gVisor nodes.** Not required by
  Kubernetes or GKE, but useful once more than one Substrate version has
  ever run on the cluster (e.g. after an upgrade): label gVisor nodes with
  the installed Substrate commit or version, and reuse that same key/value
  as `workerpool.yaml`'s `nodeSelector`, so workers only land on nodes
  running a compatible `atelet`.

## Install order

1. Cluster requirements above (GKE version, node pools, taints).
2. Agent Substrate itself — this is **out of scope for this folder**: use
   Substrate's own installer against `github.com/agent-substrate/substrate`
   (record which commit you installed; behavior in this README and in
   `../README.md` was verified against a specific commit, and drift is
   possible). This is what creates the `WorkerPool`/`SandboxConfig` CRDs,
   `ate-system` namespace, `ateapi`, and `atenet-router`.
3. Enable NetworkPolicy enforcement (below) — do this before applying
   `networkpolicy.yaml`; applying a NetworkPolicy object on a cluster with no
   enforcing CNI succeeds and enforces nothing, silently.
4. `workerpool.yaml` (this directory) — render and apply:
   ```sh
   export SUBSTRATE_WORKER_NAMESPACE=scion-agents
   export WORKER_SELECTOR_KEY=pool
   export WORKER_SELECTOR_VALUE=scion-agents
   export WORKER_POOL_REPLICAS=2
   export WORKER_IMAGE=...   # the Substrate-provided ateom worker image; see the table below
   export SUBSTRATE_VERSION_LABEL_KEY=ate.dev/substrate-version
   export SUBSTRATE_VERSION_LABEL_VALUE=...   # your installed Substrate commit/version; see "Cluster requirements" above

   envsubst < deploy/substrate/cluster/workerpool.yaml | kubectl apply -f -
   ```
5. `networkpolicy.yaml` (this directory) — needs `ATE_SYSTEM_NAMESPACE` and
   `BROKER_NAMESPACE` in addition to `SUBSTRATE_WORKER_NAMESPACE` above
   (same values `../broker.yaml` uses — see `../README.md`'s placeholder
   table):
   ```sh
   export ATE_SYSTEM_NAMESPACE=ate-system
   export BROKER_NAMESPACE=scion-substrate-broker

   envsubst < deploy/substrate/cluster/networkpolicy.yaml | kubectl apply -f -
   ```
6. `../broker.yaml` — see `../README.md` for the full sequence from there.

## Validating `workerpool.yaml`

`workerpool.yaml`'s `WorkerPool` object (`ate.dev/v1alpha1`) is a Substrate
CRD, not a builtin Kubernetes type — `kubeconform`'s default schema set has
no schema for it, so a plain `kubeconform` run treats it as unrecognized
rather than validated. Two options that actually check it:

```sh
# Option A: validate against the real upstream CRD, offline, before you
# have cluster access. Fetch the CRD Substrate itself installs, convert its
# openAPIV3Schema to a JSON schema kubeconform can use, and validate against
# that -- this is what was used to validate the manifest committed here.
curl -sSL -o /tmp/ate.dev_workerpools.yaml \
  https://raw.githubusercontent.com/agent-substrate/substrate/main/manifests/ate-install/generated/ate.dev_workerpools.yaml

git clone --depth 1 https://github.com/yannh/kubeconform.git /tmp/kubeconform-src
(cd /tmp/kubeconform-src/openapi2jsonschema-go && go build -o /tmp/openapi2jsonschema .)
mkdir -p /tmp/crd-schemas
(
  cd /tmp/crd-schemas
  /tmp/openapi2jsonschema /tmp/ate.dev_workerpools.yaml   # writes workerpool_v1alpha1.json
  cp workerpool_v1alpha1.json WorkerPool_v1alpha1.json    # match {{.ResourceKind}}'s case
)

# Run this from the repo root, so the relative manifest path below resolves:
envsubst < deploy/substrate/cluster/workerpool.yaml > /tmp/workerpool.rendered.yaml
kubeconform -strict -summary -kubernetes-version 1.31.0 \
  -schema-location default \
  -schema-location '/tmp/crd-schemas/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json' \
  /tmp/workerpool.rendered.yaml

# Option B: once Substrate is installed and you have cluster access, the
# live CRD is the schema -- no separate fetch/convert needed:
kubectl apply --dry-run=server -f /tmp/workerpool.rendered.yaml
```

If your installed Substrate version's CRD has drifted from `main`, prefer
Option B (or fetch the CRD from the commit you actually installed) — Option
A validates against upstream `main`, which may not exactly match an older
pinned commit.

## Enabling NetworkPolicy enforcement

Router ingress isolation (see `networkpolicy.yaml` and `../README.md`,
"Known limitations") only works if the cluster actually enforces
NetworkPolicy objects. **A GKE cluster created without an enforcing CNI
accepts every `NetworkPolicy` object and enforces none of them** —
`kubectl apply` succeeds either way. Confirm enforcement is really on,
don't assume it from the manifest applying cleanly:

```sh
gcloud container clusters describe <cluster> --location=<location> --project=<project> \
  --format='value(networkConfig.datapathProvider,networkPolicy.enabled,networkPolicy.provider)'
```

Expect either `datapathProvider=ADVANCED_DATAPATH` (GKE Dataplane V2), **or**
`networkPolicy.enabled=True` with `networkPolicy.provider=CALICO` — checking
only `datapathProvider` gives a false negative on a Calico cluster, since
that field only reflects Dataplane V2.

**Turning on NetworkPolicy enforcement on a cluster that already has actors
running is not a same-day, apply-and-go change.** On a Calico-enforced
cluster (not Dataplane V2), enabling it required, in order:

1. Enable the add-on, then enforcement itself:
   ```sh
   gcloud container clusters update <cluster> --location=<location> --project=<project> \
     --update-addons=NetworkPolicy=ENABLED
   gcloud container clusters update <cluster> --location=<location> --project=<project> \
     --enable-network-policy
   ```
2. **Label existing nodes** `projectcalico.org/ds-ready=true` — the rolling
   update that installs Calico does not auto-label nodes that already
   existed, so `calico-node` never schedules onto them without this step.
3. **Rolling-restart every pod that must be subject to policy** — at minimum
   `atenet-router`, every worker Deployment, and the `atelet` DaemonSet. Pods
   created before Calico keep GKE's original CNI (no `cali*` interface) and
   get **no enforcement at all**, silently: `kubectl get networkpolicy`
   still shows the object as applied even though a router pod that was never
   restarted leaves the policy completely unenforced.
4. **Recreate every golden `ActorTemplate` snapshot.** A snapshot taken
   before enforcement was live fails `runsc restore` with **exit 128** once
   Calico is active — the snapshotted network namespace state is
   incompatible with Calico's CNI. Delete the stale template
   (`kubectl ate delete actor-template`) and let the next create rebuild it.

Plan this as a maintenance window on any cluster with actors already
running, not a toggle. Check your own cluster's change history for the
already-executed procedure before repeating any of this.

## NetworkPolicy objects

`networkpolicy.yaml` requires an **enforcing CNI** — see "Enabling
NetworkPolicy enforcement" above. On a cluster without one, both objects
below apply cleanly and enforce nothing; confirm
enforcement is actually on before trusting either.

None of the objects here create the router→actor or bootstrap/exec allow
path — that traffic is permitted by NetworkPolicy objects Substrate's own
`atecontroller` WorkerPool controller generates per `WorkerPool`, not by
anything in this file. All three objects below are restrictions layered on
top of that controller-generated baseline, not replacements for it.

- **`atenet-router-restrict-ingress`** (`${ATE_SYSTEM_NAMESPACE}`) —
  restricts `atenet-router` ingress to the broker namespace
  (`${BROKER_NAMESPACE}`). It lives here, rather than in `../broker.yaml`,
  because it's a cluster/`ate-system` prerequisite rather than a
  broker-specific resource. Rollback: `kubectl delete networkpolicy atenet-router-restrict-ingress -n "${ATE_SYSTEM_NAMESPACE}"`.
  Note: a `kubectl apply --prune` workflow scoped to `../broker.yaml` alone
  would delete this object, since `../broker.yaml` does not contain it —
  apply (and prune) `networkpolicy.yaml` and `../broker.yaml` together, or
  scope any prune to both files.
- **`scion-worker-default-deny-ingress`** (`${SUBSTRATE_WORKER_NAMESPACE}`) —
  a bare default-deny ingress (`podSelector: {}`, `policyTypes: [Ingress]`,
  no rules) covering every pod in the worker namespace. NetworkPolicy
  objects selecting the same pods are OR'd — a peer is allowed if *any*
  applicable policy permits it — so a default-deny with zero rules adds no
  allow to that union; it cannot narrow what the controller-generated
  per-`WorkerPool` policy already permits, and it closes ingress for any pod
  in the namespace that policy doesn't reach. A hand-written *allow* policy
  here (e.g. "allow from `${ATE_SYSTEM_NAMESPACE}` to every pod in this
  namespace") would instead widen access beyond what the controller-generated
  policy grants — the same reasoning `../README.md`'s "Known limitations"
  documents for why this folder doesn't hand-write a worker-side allow rule.
  No egress policy is included: one risks breaking the egress gateway/DNS
  paths every actor depends on, and validating that is out of scope for this
  baseline. Rollback: `kubectl delete networkpolicy scion-worker-default-deny-ingress -n "${SUBSTRATE_WORKER_NAMESPACE}"`.
- **`scion-broker-default-deny-ingress`** (`${BROKER_NAMESPACE}`) — the same
  bare default-deny shape as the worker-namespace policy above, for the
  broker's own namespace. The broker only ever dials out (to ateapi, the
  router, and the hub); its one inbound port (broker-api, 9800) is called
  by kubelet's readiness/livenessProbe only, which — like the router's own
  probes — is exempt from NetworkPolicy enforcement, so a bare default-deny
  needs no peer list to stay complete. See `../README.md`, "Broker API
  exposure", for the HMAC auth layer this sits on top of. Rollback:
  `kubectl delete networkpolicy scion-broker-default-deny-ingress -n "${BROKER_NAMESPACE}"`.

## `worker_selector`

`worker_selector` (in `../broker.yaml`'s ConfigMap, and in
`settings.example.yaml`) must match **the target `WorkerPool` object's own
`metadata.labels`** — not any Kubernetes Pod label, and not the
`ate.dev/worker-pool=<pool-name>` label Substrate's own `atecontroller`
separately stamps onto the *generated worker Pods* for its own NetworkPolicy
use. These are two different label sets on two different objects. Getting
this wrong doesn't fail to apply — the manifest renders and `kubectl apply`
succeeds — it fails at runtime: `CreateActor` returns "no free workers"
because zero `WorkerPool`s match the selector, even with capacity to spare.

To find the correct value for your cluster:

```sh
kubectl get workerpool -n "${SUBSTRATE_WORKER_NAMESPACE}" --show-labels
```

Use one of the labels under that `WorkerPool` object's own `metadata`, not
anything you find by inspecting the worker Pods it created.

## Placeholders in this directory

`SUBSTRATE_WORKER_NAMESPACE`, `ATE_SYSTEM_NAMESPACE`, `BROKER_NAMESPACE`, and
`WORKER_SELECTOR_KEY`/`WORKER_SELECTOR_VALUE` are the same placeholders
`../broker.yaml` and `../settings.example.yaml` use — resolve them once and
reuse the same values across every manifest in this folder.

| Placeholder | Meaning | Used by |
|---|---|---|
| `SUBSTRATE_WORKER_NAMESPACE` | Namespace the WorkerPool and its worker pods run in (shared with `../broker.yaml`) | `workerpool.yaml`, `networkpolicy.yaml` |
| `ATE_SYSTEM_NAMESPACE` | Namespace hosting `atenet-router` (shared with `../broker.yaml`) | `networkpolicy.yaml` |
| `BROKER_NAMESPACE` | Namespace the broker runs in (shared with `../broker.yaml`) | `networkpolicy.yaml` |
| `WORKER_SELECTOR_KEY` / `WORKER_SELECTOR_VALUE` | Label on the WorkerPool's own `metadata.labels` (shared with `../broker.yaml`'s `worker_selector`) | `workerpool.yaml` |
| `WORKER_POOL_REPLICAS` | Number of worker pods — see "Sizing" below | `workerpool.yaml` |
| `WORKER_IMAGE` | The Substrate-provided `ateom` worker image for your installed version/sandbox class — not a scion image; resolved by your Substrate install | `workerpool.yaml` |
| `SUBSTRATE_VERSION_LABEL_KEY` / `SUBSTRATE_VERSION_LABEL_VALUE` | The node label your gVisor pool carries for the installed Substrate version (see "Cluster requirements" above) — drop the `nodeSelector` block entirely if your cluster doesn't label nodes this way | `workerpool.yaml` |

## Sizing

`workerpool.yaml`'s per-worker-pod `resources` size the worker's advertised
scheduling capacity (read from the `limits`), not the sandboxed actor
itself — that's sized separately by the ActorTemplate the substrate runtime
builds from the profile. `1 CPU / 4Gi` requested, `2 CPU / 8Gi` limit per
worker is enough for a single Claude Code-class agent; size `replicas` for
how many concurrent agents you expect, keeping in mind that a template's
first ("golden") build also consumes one worker slot while it runs (see
`../OPERATIONS.md`, "Warm the template before first use").

## TODO (not in this folder yet)

- **A doctor-style pre-flight check** that verifies NetworkPolicy
  enforcement and CPU-platform uniformity automatically, instead of the
  manual `gcloud`/`kubectl` commands above. Not built.
- **A Helm chart or Kustomize overlay** for this directory and `../broker.yaml`
  together. These remain plain manifests; see `../README.md`.
