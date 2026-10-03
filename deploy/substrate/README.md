# Substrate deployment for scion

This folder is meant to be self-contained: starting from an empty GKE
cluster, it takes an operator to a running scion agent on the Substrate
runtime, using nothing outside this directory (plus the upstream Substrate
installer itself, which isn't scion's to vendor).

## Install sequence

1. **Cluster prerequisites** — GKE version, node pools, the gVisor taint, and
   NetworkPolicy enforcement. See `cluster/README.md`.
2. **Agent Substrate itself** — out of scope for this repo; install it with
   Substrate's own tooling against `github.com/agent-substrate/substrate`.
   See `cluster/README.md`, "Install order".
3. **Cluster-level scion objects** — the dedicated `WorkerPool` and the
   NetworkPolicy objects this deployment depends on. See `cluster/README.md`,
   `cluster/workerpool.yaml`, and `cluster/networkpolicy.yaml`.
4. **The in-cluster broker** — `broker.yaml`, below. This is the bulk of this
   README: what it deploys, every placeholder it needs resolved, the Secret
   it expects to already exist, and how to validate and verify it.
5. **First agent** — see "First agent" near the end of this file, and
   `OPERATIONS.md` for warming the template before pointing real traffic at
   it.

Day-2 operations (broker restarts, stuck actors, force-clean recovery) live
in `OPERATIONS.md`, not here — this file is the install path.

## What `broker.yaml` is

Deploys the scion runtime broker as an in-cluster workload on the Substrate
GKE cluster, connected to a scion hub, with the `substrate` runtime profile
it needs to run agents as Substrate actors. This is scaffolding
(`.design/kubernetes/substrate-runtime.md` §1): a minimal manifest for the
end-to-end slice, not a polished Helm chart (that's future work).

`broker.yaml` is a **plain manifest**, not a chart. It uses `${VAR}`-style
placeholders resolved with `envsubst` (or an equivalent templating pass)
before `kubectl apply` — every other manifest in this folder follows the
same convention.

## Why an in-cluster broker

Substrate has no authorization on its control API or inbound router — see
`.design/kubernetes/substrate-runtime.md` §1. The broker needs both the
`ateapi` service and the `atenet-router` reachable, and must run in-cluster
rather than reach in over a LoadBalancer/Ingress that would expose those
unauthenticated surfaces beyond the cluster boundary.

## Pin the agent image by digest (REQUIRED)

Substrate requires a digest-pinned agent image (`.design/kubernetes/substrate-runtime.md`
§3). Without one, `scion start` on a `substrate` profile fails closed
(`pkg/runtime/substrate_runtime.go:339`):

```
substrate: image "<image>" is not pinned by digest (@sha256:...); set a digest image in the agent's template or pass --image (tag resolution is not currently supported)
```

**How to pin:** pin by digest in the template's `scion-agent.yaml` `image:`
(honoured at runtime) or with `--image`.

**Resolution order:** harness-config → broker profile (fallback) → template
→ `--image`; the last one set wins. The broker active-profile image applies
only when neither the template nor `--image` sets one.

**Enforcement:** any image not pinned by digest is refused — the fail-closed
error above. ANY digest-pinned image is accepted: substrate does NOT
restrict which images users may run. An operator who needs that has no
control today. The broker profile pin is **not** an enforcement
point — it is the operator default/fallback, and it loses to the template
and `--image`. The only enforcement is the fail-closed "pinned by digest"
check above, which is source-agnostic.

**Pinning the digest in the substrate profile's `harness_overrides` alone
is NOT sufficient on a default install.** Why:

- The shipped `claude` harness-config sets `image: scion-claude:latest`.
- The broker copies that image into the agent's own persisted config at
  provisioning time (`pkg/agent/provision.go`, the harness-config merge,
  written into the agent's `scion-agent.json`).
- At start, that agent/template config image outranks the profile's
  `harness_overrides` pin (`pkg/agent/run.go`'s image resolution order).

**Recommended:** create a substrate template that sets
`image: <registry>/scion-claude@sha256:<digest>`, and create substrate
agents from that template. A template image overrides the harness-config
image, affects only agents created from that template, and survives
`scion harness-config upgrade --force` (which reseeds the harness-config's
own `image:` key, but doesn't touch the template).

**Per-agent alternative:** pass `--image <registry>/scion-claude@sha256:<digest>`
at `scion start` time.

**Not recommended:** editing `image:` directly in the hub's `claude`
harness-config. That config is shared by every broker and runtime on the
hub:

- removing the key breaks non-substrate profiles, unless settings also set
  `harness_configs.claude.image`;
- existing agents keep whatever image they already resolved and must be
  recreated to pick up a change;
- a non-forced `scion harness-config upgrade` only fills in missing or
  empty keys (`mergeMissingMapValues`, `pkg/config/harness_config_upgrade.go`),
  so a digest pin survives it, but *deleting* the `image:` key instead of
  editing it gets the key re-added as `scion-claude:latest` on the next
  upgrade;
- `scion harness-config upgrade --force` reseeds `image:` from the bundled
  default unconditionally, discarding any pin here regardless of how it was
  set — re-apply it afterward if you go this route.

**Diagnostic:** `scion-agent.json` (the `image` field, in the agent's
directory on the broker) carries the image the agent's persisted config
resolved to, which `start` uses unless `--image` is passed — `--image` is
applied only for that one `start` call (`pkg/agent/run.go:353`) and is
never written back to `scion-agent.json`.

**Hub column note:** the hub's template image/config fields are not
populated by the template-upload path, though the file is read at runtime.
Verify the running image from the resolved actor image or broker log line,
not from the hub template record.

## Placeholders

Resolve every one of these before applying `broker.yaml`. None are secret;
secrets are handled separately (see "Secret creation" below). Cluster-level
placeholders for `cluster/workerpool.yaml` and `cluster/networkpolicy.yaml`
are documented in `cluster/README.md` — several are shared with this table
(`SUBSTRATE_WORKER_NAMESPACE`, `WORKER_SELECTOR_KEY`/`WORKER_SELECTOR_VALUE`,
`ATE_SYSTEM_NAMESPACE`); resolve them once and reuse the same values across
every manifest.

| Placeholder | Meaning |
|---|---|
| `BROKER_NAMESPACE` | Namespace the broker Deployment and its RBAC live in. **Also the value the router NetworkPolicy's `namespaceSelector` is pinned to** — see the callout below the table. |
| `BROKER_IMAGE` | Branch-built image containing the `scion` binary (see "Building the broker image") |
| `ATE_SYSTEM_NAMESPACE` | Namespace hosting `ateapi` and `atenet-router` (upstream default: `ate-system`) |
| `SUBSTRATE_WORKER_NAMESPACE` | Namespace the actor **worker pods** run in. Not read by anything this manifest applies (agent logs are not currently supported on the substrate runtime — see "Known limitations" below); used only for locating Substrate's own controller-generated worker `NetworkPolicy` and for the verification commands below |
| `HUB_ENDPOINT` | The scion hub's URL |
| `HUB_BROKER_ID` | The broker's stable UUID from `scion runtime-broker register` (not secret — see below) |
| `HUB_CONNECTION_NAME` | The `--name` used at `register` time; also the credentials JSON filename |
| `CLUSTER_TRUST_BUNDLE_NAME` | The `ClusterTrustBundle` object verifying `ateapi` TLS (the router hop is plaintext today — see below) |
| `SANDBOX_CONFIG_NAME` | The `SandboxConfig` CRD instance actor templates use |
| `WORKER_SELECTOR_KEY` / `WORKER_SELECTOR_VALUE` | One label key/value pinning actors to a `WorkerPool` — matched against **the `WorkerPool` object's own `metadata.labels`**, not any Pod label (see `cluster/README.md`, "`worker_selector`") |
| `SNAPSHOT_STORAGE_URI` | Bucket/prefix for actor snapshots |

**`BROKER_NAMESPACE` is baked into the router NetworkPolicy at apply time,
not read live.** The router `NetworkPolicy`'s `namespaceSelector` matches on
`${BROKER_NAMESPACE}`'s resolved value — a literal namespace name in the
rendered manifest, the same way every other placeholder here resolves once
at `envsubst` time. If you later rename the broker's namespace, or redeploy
the broker into a different one, that `NetworkPolicy` object still points at
the *old* namespace until you re-render and re-apply it with the new value —
the broker's Deployment moving does not update it. The symptom is
exec/bootstrap timing out talking to the router even though the broker pod
itself looks healthy, because the router now refuses it.

**`egress_allow` entries are validated, and rejected entries block the
broker from starting an agent.** An actor's own egress policy must never let
it reach the router or other in-cluster services — that would defeat the
NetworkPolicy above, which is what keeps the bootstrap-nonce
fallback safe (see "Known limitations" below). `pkg/config.V1SubstrateConfig.Validate`
takes an allowlist-first approach: it accepts *only* public FQDNs
(optionally wildcarded as `*.example.com`), and rejects everything else,
including any IP/CIDR, catch-alls (`all`, `*`, `0.0.0.0/0`), non-ICANN or
internal-shaped hostnames, and hostnames ending in `.svc`, `.cluster.local`,
`.internal`, `.local`, or `.localhost`. See `ValidateEgressAllow`'s doc
comment (`pkg/config/substrate_egress.go`) for the exact rule set. None of
this protects against a validly-public hostname later resolving to a
private or in-cluster address — DNS rebinding, or a service that does this
by design.

**An actor's own git-clone remote and telemetry endpoint are added to its
egress allowlist only when `egress_allow` also covers them.** Both are
tenant-controllable (a workload's own `SCION_GIT_CLONE_URL`, or its
`SCION_OTEL_ENDPOINT`/`OTEL_EXPORTER_OTLP_*` telemetry endpoints), and
passing the same public-FQDN grammar above is not enough to trust them on
its own — a domain the tenant themselves registers, or a service like
nip.io/sslip.io that resolves an embedded IP octet on request (the cloud
metadata address included), is a perfectly well-formed public hostname. An
operator who wants an actor's real git remote or telemetry collector to
actually be reachable must list that host — or a wildcard covering it — in
`egress_allow` themselves; an uncovered host is silently dropped (logged,
never added) rather than granted by default.

**The hub host added to an actor's egress allowlist comes from broker or
operator configuration only** — the request-level hub endpoint, the hub
connection endpoint, or this broker's own configured hub endpoint — never
from project settings or any other tenant-controllable source; when none of
those operator sources is available, no hub host is added to egress at all.

**The entire `runtimes.<name>` block above — not just `egress_allow` — is
operator-only.** A project's merged settings (an in-repo `.scion/settings.*`,
or a hub-managed project's own settings file) may only *select* an
operator-defined substrate profile by name; it may never define or override
`api_endpoint`, `router_endpoint`, `ca_file`/`cluster_trust_bundle`,
`token_audience`, `egress_allow`, or any other key in this table. The broker
refuses to construct a substrate runtime outright — a clear config error,
never a silent merge — if a project's settings define a substrate runtime
block that doesn't match the operator's own global settings byte for byte.
This matters because this block is exactly what a hijacked or malicious repo
would need to control to redirect the bootstrap payload (hub token, resolved
secrets) to an attacker endpoint, or to widen egress arbitrarily.

### `egress_trust_bundle`: only needed under sdsmint

Substrate's plain `atenet-egress` only enforces `egress_allow` for TLS
*passthrough* on ADDRESS rules — every rule `egress_allow` emits is a
HostnameRule, which is enforced for HTTPS only under the **sdsmint** egress
gateway (`hack/install-ate.sh --deploy-atenet --experimental-use-sdsmint`).
Under sdsmint the gateway terminates every TLS connection and re-originates
it with a per-SNI leaf certificate chained to its own CA — not the origin's
— so an actor validating only the public roots rejects it and every HTTPS
request fails.

Set `runtimes.<name>.substrate.egress_trust_bundle: egress-mitm.ate.dev` —
the trust bundle name the vendored Substrate version resolves — **if and
only if the cluster runs sdsmint and the actor makes any HTTPS/TLS
request.** When set, `buildActorTemplate` (`pkg/runtime/substrate_template.go`)
adds a `system-info` volume that projects the gateway's CA to
`/run/ate/trust-bundle.pem`, and points `NODE_EXTRA_CA_CERTS`,
`GIT_SSL_CAINFO`, `SSL_CERT_FILE`, and `CURL_CA_BUNDLE` at that file, plus
`SSL_CERT_DIR=/run/ate`. See `docs/egress-trust-bundle.md` in
agent-substrate/substrate for the full guide.

`SSL_CERT_DIR` makes the gateway CA **exclusive** for Go and Python's `ssl`
module, but is only **additive** for curl, git, and Node — those keep
trusting the public root set too, so a curl/git success on its own isn't
proof the gateway did the validating (check the certificate issuer, or force
curl to use only the projected bundle with
`--capath /nonexistent --cacert /run/ate/trust-bundle.pem`). substrate-serve's
exec path (`/scion/v1/exec`) carries these vars through explicitly via a
direct credential drop (never `su`), so no particular `su` version or
feature needs to be present in the image for them to survive it.

**Setting this on a plain (non-sdsmint) install breaks every actor** — with
nothing backing the `ClusterTrustBundle` name, the actor fails to start
entirely. Leaving `egress_trust_bundle` empty (the default) is
byte-identical to not having this feature at all.

### `server.broker.broker_id` in the ConfigMap

`HUB_BROKER_ID` fills `server.broker.broker_id` (`pkg/config/settings_v1.go`
`V1BrokerConfig`), populated by the same `scion runtime-broker register` run
that produces the credentials Secret. It's a stable UUID, not a credential,
so it's fine in a ConfigMap. The same struct also has `broker_token` — that
field **is** the secret (the older single-shared-secret auth mode) and must
never go in this ConfigMap. This deployment authenticates with the newer,
per-connection `hub-credentials/<name>.json` file instead (see "Secret
creation"), so `broker_token` is deliberately absent from `broker.yaml`.

## Building the broker image

The broker is the same `scion` binary the Hub uses (see `Dockerfile.hub`,
which already builds `/usr/local/bin/scion` from `./cmd/scion/` with
`ENTRYPOINT ["/usr/local/bin/scion"]`). Build and push an image from this
repo:

```sh
docker build -f Dockerfile.hub -t "${BROKER_IMAGE}" .
docker push "${BROKER_IMAGE}"
```

Nothing broker-specific is baked into the image — `broker.yaml`'s `args`
select broker-only mode (`--enable-runtime-broker` without `--enable-hub`).

## Secret creation (out-of-band, never in this manifest)

The broker connects **outward** to the scion hub using HMAC credentials
obtained by registering, not by anything baked into the image or the
ConfigMap.

1. From a machine with hub access (not the cluster), register:

   ```sh
   scion runtime-broker start --foreground &   # or as a background daemon
   scion runtime-broker register --name "${HUB_CONNECTION_NAME}" \
     --hub "${HUB_ENDPOINT}"
   ```

   This writes `~/.scion/hub-credentials/${HUB_CONNECTION_NAME}.json` and
   prints the broker's `brokerId` — that's `HUB_BROKER_ID` above.

2. Create the Secret directly from that file — never paste its contents into
   a committed manifest:

   ```sh
   kubectl create secret generic scion-substrate-broker-hub-credentials \
     --namespace "${BROKER_NAMESPACE}" \
     --from-file="${HUB_CONNECTION_NAME}.json=${HOME}/.scion/hub-credentials/${HUB_CONNECTION_NAME}.json"
   ```

   `broker.yaml` mounts this Secret read-only (`defaultMode: 0400`) at
   `/home/scion/.scion/hub-credentials/${HUB_CONNECTION_NAME}.json` inside
   the broker container.

3. Rotation: re-run `register --force` on the registering machine, then
   `kubectl create secret ... --dry-run=client -o yaml | kubectl apply -f -`
   (or delete + recreate) with the refreshed file. There is no in-cluster
   rotation mechanism today.

## Apply order

```sh
# 0. Cluster prerequisites and cluster-level scion objects — see
#    cluster/README.md, cluster/workerpool.yaml, cluster/networkpolicy.yaml.
#    Do this first: broker.yaml's ConfigMap references a WorkerPool that
#    must already exist, and its NetworkPolicy verification steps below
#    assume enforcement is already on.

# 1. Resolve placeholders (envsubst reads ${VAR} from the environment).
export BROKER_NAMESPACE=scion-substrate-broker
export BROKER_IMAGE=...
export ATE_SYSTEM_NAMESPACE=ate-system
export SUBSTRATE_WORKER_NAMESPACE=scion-agents
export HUB_ENDPOINT=...
export HUB_BROKER_ID=...
export HUB_CONNECTION_NAME=scion-integration
export CLUSTER_TRUST_BUNDLE_NAME='servicedns.podcert.ate.dev:identity:primary-bundle'
export SANDBOX_CONFIG_NAME=gvisor-default
# Must match the target WorkerPool's own metadata.labels, NOT a Pod label —
# see cluster/README.md, "worker_selector".
export WORKER_SELECTOR_KEY=pool
export WORKER_SELECTOR_VALUE=scion-agents
export SNAPSHOT_STORAGE_URI=gs://<your-bucket>/scion/

envsubst < deploy/substrate/broker.yaml > /tmp/broker.rendered.yaml

# 2. Validate before touching the cluster (see "Validation" below).

# 3. Namespace + RBAC + ConfigMap first, so the Secret step below has
#    somewhere to land and the Deployment has everything it needs the
#    moment it starts. (The router NetworkPolicy is applied separately, in
#    step 0, from cluster/networkpolicy.yaml.)
kubectl apply -f /tmp/broker.rendered.yaml

# 4. Create the Secret (see "Secret creation" above) — do this AFTER step 3
#    creates the namespace, before the Deployment's pod actually starts
#    scheduling. A missing (non-optional) Secret volume source leaves the
#    pod in ContainerCreating with a FailedMount event, not CrashLoopBackOff,
#    until the Secret exists.
kubectl create secret generic scion-substrate-broker-hub-credentials ...
```

`envsubst` isn't preinstalled everywhere; on Debian/Ubuntu it's in the
`gettext-base` package. If you don't have it, any templating pass that
resolves `${VAR}` (kustomize's `configMapGenerator` + patches, `sed`, etc.)
works equally well — the manifests themselves have no envsubst-specific
syntax.

## Validation

`kubectl apply --dry-run=client` needs a reachable API server for discovery
even in client-only mode, so it's not usable from a machine with no cluster
access at all. Two options that don't require one:

```sh
# Static schema validation: https://github.com/yannh/kubeconform
go install github.com/yannh/kubeconform/cmd/kubeconform@latest
kubeconform -strict -summary -kubernetes-version 1.31.0 /tmp/broker.rendered.yaml

# Once you *do* have cluster access:
kubectl apply --dry-run=client -f /tmp/broker.rendered.yaml
kubectl apply --dry-run=server -f /tmp/broker.rendered.yaml   # catches RBAC/CRD-shape issues client-side can't
```

`cluster/workerpool.yaml` uses a Substrate CRD (`WorkerPool`,
`ate.dev/v1alpha1`) that a generic schema validator has no built-in schema
for — see `cluster/README.md`, "Validating `workerpool.yaml`", for the
CRD-schema `kubeconform` recipe (offline) and the `kubectl apply
--dry-run=server` alternative (once Substrate is installed).

## Broker API exposure

The Deployment passes `--host=0.0.0.0` so the broker's own API (port 9800,
used by the `readinessProbe`/`livenessProbe`) binds to all interfaces, not
just loopback. Without it, a standalone broker in `--hosted` mode (no
`--enable-hub`) binds to `127.0.0.1` by default — a safety net
(`loadAndReconcileConfig`, `cmd/server_foreground.go`) so a *fresh* broker
with no HMAC keys yet can still be reached locally by `scion runtime-broker
register` before those keys exist. kubelet's probes connect to the **pod
IP**, not to `127.0.0.1` inside the container's own network namespace, so
that default would make both probes fail with connection-refused.

**This is safe because the broker's own HMAC auth is unconditionally
"strict mode."** The live `ServerConfig` built for this deployment
(`startRuntimeBroker`, `cmd/server_foreground.go`) hardcodes `BrokerAuthEnabled: true,
BrokerAuthStrictMode: true` and, more importantly, **refuses to start at
all** on a non-loopback host unless valid HMAC keys are already loaded
(`validateBrokerAuthStartup`, `pkg/runtimebroker/server.go`). It does not
fall open to unauthenticated non-loopback listening under any configuration
this manifest produces. An **empty or invalid** Secret makes the container
fail closed (`CrashLoopBackOff`) rather than serve `:9800` unauthenticated. A
Secret that is **missing entirely** never reaches that code at all — the
non-optional Secret volume keeps the pod in `ContainerCreating` with a
`FailedMount` event until the Secret exists.

**There is now a bare default-deny-ingress NetworkPolicy on the broker
namespace** (`cluster/networkpolicy.yaml`'s `scion-broker-default-deny-ingress`),
denying any in-cluster peer other than kubelet (whose own readiness/
liveness probe traffic is exempt from NetworkPolicy enforcement, the same
as the router's probes — see `cluster/README.md`). This closes the gap a
previous version of this doc left open as "worth doing, but not added
here": it does not need the node-CIDR-dependent "kubelet only" *allow* rule
that question was stuck on, because a bare default-deny needs no peer list
at all — kubelet's probe traffic was never subject to a NetworkPolicy
denial in the first place. HMAC auth (above) is still the primary defense;
this NetworkPolicy is defense in depth, same as the worker-namespace
baseline.

## Bootstrap files: home delivery

`buildBootstrapFiles` (`pkg/runtime/substrate_bootstrap.go`) assembles the
`POST /scion/v1/bootstrap` payload's `files` array from three sources, in
precedence order: the broker-composed agent home (harness-config `home/`,
the template home, and skills), then resolved auth files, then file-type
resolved secrets. Later sources win on a path collision, and exactly one
entry per path reaches the wire.

**This delivery model is deliberately narrower than Docker/Podman or
cloudrun-sandbox, which bind-mount or relocate the home directly:**

- **Copy-in, one-way.** Files are read once, at bootstrap time. Nothing the
  actor writes afterwards syncs back.
- **Additive over the image's home, not a replacement.** The actor image's
  own `/home/scion` is not cleared first.
- **Only regular files are shipped.** The walk never follows a symlink, and
  skips (counting but never reading) fifos, sockets, and devices.

**No symlink traversal in a target's path.** A file-secret or auth target
that traverses a symlink anywhere in its path — including a system symlink
like `/var/run -> /run` — fails bootstrap with HTTP `422`
(`bootstrap_path_symlink`). Use the resolved form of the target instead
(`/run/secrets/...` rather than `/var/run/secrets/...`).

**Targets must resolve under the agent home.** A file-secret or auth target
that does not resolve under the agent home directory — an absolute path
outside it, or a `..`-relative escape from it — fails bootstrap with HTTP
`422` (`bootstrap_path_outside_home`), before anything is written.

**Size cap.** Total decoded size across every file (home + auth + secrets,
after dedup) is capped at 16 MiB (`maxBootstrapFilesTotalBytes`,
`pkg/runtime/substrate_bootstrap.go`) — sized with headroom under the 64 MiB
raw-body limit `sciontool substrate-serve`'s bootstrap handler enforces
(`maxBootstrapBodyBytes`, `pkg/sciontool/substrate/server.go`). Exceeding the
cap fails the run with an error naming only the cap and the total size,
never a path or file content.

**A separate, in-actor secrets path has its own symlink rule — not the same
rule as above.** Independent of the `POST /scion/v1/bootstrap` delivery
above, `sciontool init` also decodes and writes any `SCION_STAGED_SECRETS`
payload directly inside the actor (`pkg/stagedsecrets`) — the mechanism every
runtime, not just substrate, uses to stage file and variable secrets without
bind-mounting them from the host. This path has no "must resolve under the
agent home" rule at all (unlike the bootstrap targets above, any destination
is allowed), and its symlink handling is narrower than a blanket refusal: a
staged secret's parent directory is resolved component-by-component
(`dirfd.EnsureDirTrustedAncestorFollow`), and a symlink is followed, not
refused, when both it and its containing directory are root-owned and not
group/other-writable — a system-configured alias like `/var/run -> /run`
that no workload can have planted or redirected. Any symlink that doesn't
meet that trusted-ownership bar is refused, and init refuses the write
rather than following it.

## Verification commands (once applied to a real cluster)

```sh
# Broker pod is up and READY (the readinessProbe already hits /healthz;
# the broker image has no wget/curl, so don't exec a check into it).
kubectl -n "${BROKER_NAMESPACE}" get pods -l app=scion-substrate-broker

# Or hit /healthz from your own workstation:
kubectl -n "${BROKER_NAMESPACE}" port-forward deploy/scion-substrate-broker 9800:9800 &
curl -s localhost:9800/healthz

# Broker registered and heartbeating (from a machine with hub access):
scion runtime-broker status --broker "${HUB_BROKER_ID}"

# TokenRequest RBAC works (the runtime actually reaches ateapi):
kubectl -n "${BROKER_NAMESPACE}" logs deploy/scion-substrate-broker | grep -i "substrate: mint ateapi token"

# active_profile: substrate is doing its job — the broker's own heartbeat
# should never fall back to auto-detecting a local container runtime:
kubectl -n "${BROKER_NAMESPACE}" logs deploy/scion-substrate-broker | grep -i "docker ps failed"
# Expect: no output.

# ---- Router NetworkPolicy: verify this on the cluster, not just that it
# applied. Confirm the policy blocks an out-of-namespace caller and allows
# an in-namespace one: ----

# (a) From a throwaway pod OUTSIDE the broker namespace: must be refused.
kubectl run netpol-probe --rm -it --restart=Never \
  --namespace default \
  --image=curlimages/curl -- \
  curl -sS --max-time 5 "http://atenet-router.${ATE_SYSTEM_NAMESPACE}.svc/scion/v1/healthz"
# Expect: a timeout/connection error (dropped by the NetworkPolicy) — any
# HTTP status at all means the policy did NOT block the request.

# (b) From a throwaway pod INSIDE the broker namespace: must connect.
kubectl run netpol-probe --rm -it --restart=Never \
  --namespace "${BROKER_NAMESPACE}" \
  --image=curlimages/curl -- \
  curl -sS --max-time 5 -o /dev/null -w '%{http_code}\n' \
  "http://atenet-router.${ATE_SYSTEM_NAMESPACE}.svc/scion/v1/healthz"

# ---- Worker-namespace NetworkPolicy: this manifest does NOT create one —
# Substrate's own atecontroller auto-creates a per-WorkerPool NetworkPolicy
# (podSelector on the pool name, ingress only from atenet-router pods within
# ATE_SYSTEM_NAMESPACE, all ports). What's left to verify is that it exists
# and works, not to create it: ----

# (c) Confirm the controller-generated policy exists for the WorkerPool
# backing SUBSTRATE_WORKER_NAMESPACE:
kubectl -n "${SUBSTRATE_WORKER_NAMESPACE}" get networkpolicy \
  -l 'ate.dev/worker-pool' -o wide
# Expect at least one NetworkPolicy. No result means either the WorkerPool
# doesn't exist yet (start an agent on this profile first) or the
# controller isn't running — either way, the bootstrap fallback currently
# has no worker-side protection at all.

# (d) Confirm it actually blocks direct pod-IP access from outside
# ate-system. Requires a real worker pod IP (start at least one agent
# first). :8080 is a plain-HTTP port on the worker pod's own container,
# convenient to probe with curl:
WORKER_POD_IP=$(kubectl -n "${SUBSTRATE_WORKER_NAMESPACE}" get pods \
  -o jsonpath='{.items[0].status.podIP}')

kubectl run netpol-probe --rm -it --restart=Never \
  --namespace default \
  --image=curlimages/curl -- \
  curl -sS --max-time 5 "http://${WORKER_POD_IP}:8080/readyz"
# Expect: a timeout/connection error. Any HTTP response means the packet
# reached the pod and no applicable policy blocked it.

# (e) Confirm it still allows the router itself through:
kubectl run netpol-probe --rm -it --restart=Never \
  --namespace "${ATE_SYSTEM_NAMESPACE}" \
  --overrides='{"metadata":{"labels":{"app":"atenet-router"}}}' \
  --image=curlimages/curl -- \
  curl -sS --max-time 5 -o /dev/null -w '%{http_code}\n' \
  "http://${WORKER_POD_IP}:8080/readyz"

# (f) Confirm a plain pod in ate-system WITHOUT that label is refused, same
# as (d) — proves the selector is really an AND of namespace and pod label:
kubectl run netpol-probe --rm -it --restart=Never \
  --namespace "${ATE_SYSTEM_NAMESPACE}" \
  --image=curlimages/curl -- \
  curl -sS --max-time 5 "http://${WORKER_POD_IP}:8080/readyz"
# Expect: refused, same as (d).
```

## First agent

Once the broker is `Ready` and registered (verification commands above):

1. **Pin an image by digest** on the profile, a template, or `--image` — see
   "Pin the agent image by digest" above. Nothing starts on this runtime
   without one.
2. **Warm the template** before pointing real traffic at it — the first
   create against any template builds its golden snapshot and can take well
   over a minute. See `OPERATIONS.md`, "Warm the template before first use".
3. **Start the agent:**
   ```sh
   scion start --profile substrate --broker "${HUB_CONNECTION_NAME}" my-first-agent "echo hello"
   scion list | grep my-first-agent
   ```
4. If it doesn't reach `running`, check the broker logs and the verification
   commands above before assuming the runtime itself is at fault — most
   first-run failures are a placeholder resolved wrong (`worker_selector`
   mismatched against the WorkerPool's own labels is the most common one —
   see `cluster/README.md`) or the Secret from "Secret creation" missing.

## Known limitations

- **`scion stop` destroys the agent on substrate.** There is no suspend
  primitive, so `Stop` is `Delete`: the actor, its workspace, and its worker
  slot are all gone, not paused. A later `scion start` provisions a
  brand-new actor from the template, not a resumed one — any uncommitted
  work in the stopped actor's workspace is lost. Push before stopping.
- **Bootstrap auth is a fallback, not an identity-derived nonce.**
  `sciontool substrate-serve` defaults to accepting any bearer
  token, relying on a single-shot "first bootstrap wins" check plus multiple
  NetworkPolicies from *different owners*: the router policy (restricting
  `atenet-router` ingress to the broker namespace), the worker policy
  Substrate itself generates per `WorkerPool` (restricting worker-pod
  ingress to `atenet-router` pods within `ATE_SYSTEM_NAMESPACE`), and the
  default-deny ingress baseline this deployment adds for the worker
  namespace. All three live in `cluster/networkpolicy.yaml` and
  `cluster/README.md` (see "Files" below), not in this file. NetworkPolicy
  objects targeting the same pods are OR'd
  together: a **hand-written ALLOW policy** here would only *widen* access
  beyond what Substrate's tighter, purpose-built policy grants, which is why
  this folder doesn't hand-write one. A **default-deny with no rules** is
  different — it adds no allow to that union, so it cannot widen anything;
  it only closes ingress for pods the controller-generated policy doesn't
  already cover, which is why that one *is* shipped (`cluster/networkpolicy.yaml`).
  None of these alone is sufficient — verifying the router policy without
  also confirming the worker-side ones (verification steps (c)–(f) above)
  is not a complete check.
- **NetworkPolicy enforcement requires an enforcing CNI, and nothing warns
  if it's missing.** A GKE cluster created without Dataplane V2 or the
  Calico add-on silently accepts NetworkPolicy objects and enforces none of
  them. See `cluster/README.md`, "Enabling NetworkPolicy enforcement".
- **Kubelet health-check probes are exempt from NetworkPolicy on GKE, by
  design, on both enforcement backends.** Neither the router's own probes
  nor a worker pod's readiness probe need an explicit allow rule for this
  reason — a documented Kubernetes/GKE networking property, not something
  specific to this manifest.
- **Metrics/monitoring scraping is not accounted for.** The router
  NetworkPolicy only opens its client-facing ports to the broker namespace;
  it does not add an allow for a metrics scraper reaching an admin/metrics
  port directly. If router metrics go missing after applying this policy,
  that's the first thing to check.
- **The precise worker-pod-to-router allow rule is Substrate's
  responsibility, not this folder's.** This deployment adds a default-deny
  ingress floor for the worker namespace (`cluster/networkpolicy.yaml`), but
  the actual allow — worker pods reachable only from `atenet-router` pods —
  comes from Substrate's own per-`WorkerPool` controller-generated policy.
  This deployment has no control over that allow rule beyond verifying it
  exists (step (c) above) — if the pool controller isn't running, is
  misconfigured, or a future Substrate version changes that controller's
  behavior, worker pods lose their intended ingress restriction with no
  signal from anything in `deploy/substrate/` (though the default-deny
  baseline means the failure mode is "unreachable" rather than "open").
- **No Helm chart, no template GC, no doctor integration.** This is a
  minimal fixture. The polished chart, template garbage collection, and
  automated pre-flight checks are future work.
- **No in-cluster credential rotation.** See "Secret creation" step 3.
- **Bootstrap files — including the composed agent home — cross the
  broker→router hop in plaintext.** The router endpoint in this fixture is
  plain HTTP, and the router client has no CA configured. Out of scope for
  the same reasons the existing plaintext-hop risk is:
  this is meant for a dedicated cluster, router ingress is restricted to the
  broker namespace, and the single-shot bootstrap check applies. A shared
  cluster needs the router's TLS listener with a CA, or an end-to-end sealed
  payload, before this ships beyond a dedicated cluster.
- **The dialer's trust material is pinned for the broker process's
  lifetime, not re-read per agent start.** Rotating the CA behind
  `ca_file`/`cluster_trust_bundle` does not take effect until the broker
  process restarts (a rolling restart is sufficient).
- **Logs.** `scion logs` and the web log view return an explicit
  not-supported error for a substrate agent: worker pods are
  shared across atespaces and users, so an unfiltered worker-pod log read
  would return other tenants' actor output alongside the caller's own.
  Operators who already hold `get` on `pods/log` in the worker namespace can
  read a worker pod's full raw output directly.
  ```sh
  kubectl logs -n <worker-namespace> <worker-pod> -c ateom \
    | jq -Rc 'fromjson? | objects | select(.["logging.googleapis.com/labels"]?["ate.actor.uid"]? == "<actor-uid>")'
  ```
- **`profiles.local`/`profiles.remote` are repointed at the substrate
  runtime in the ConfigMap on purpose** — not an oversight. The embedded
  default settings always define these two profiles against the docker and
  Kubernetes runtimes; left alone, this broker would discover both as
  auxiliary runtimes on every request and their list calls would fail here
  (no docker binary, no pod-list RBAC). If your deployment ever names either
  profile intentionally expecting docker/Kubernetes semantics, treat this
  repoint as a one-line thing to revisit.

## TODO (out of this folder for now)

- **A hand-written worker-namespace ALLOW rule of our own.** Not shipped —
  see "Bootstrap auth is a fallback" above for why a hand-written *allow*
  would widen, not narrow, Substrate's own controller-generated policy. The
  default-deny baseline this folder does ship (`cluster/networkpolicy.yaml`)
  is a floor, not that allow rule; the real allow is Substrate's, and stays
  Substrate's.
- **A Helm chart.** `broker.yaml` and the manifests under `cluster/` stay
  plain manifests.

## Files

- `broker.yaml` — Namespace, ServiceAccount, RBAC (TokenRequest-on-self,
  ClusterTrustBundle read), ConfigMap (substrate runtime profile), and
  Deployment. The router NetworkPolicy lives in `cluster/networkpolicy.yaml`
  — see that file and `cluster/README.md`.
- `settings.example.yaml` — the same runtime/profile shape as `broker.yaml`'s
  ConfigMap, standalone, for reference or for a non-Kubernetes broker.
- `cluster/` — cluster-level prerequisites, including both NetworkPolicy
  objects; see `cluster/README.md`.
- `OPERATIONS.md` — day-2 operations: template warm-up, broker restart
  semantics, and stuck-actor recovery.
- This README — install sequence and the broker runbook.
