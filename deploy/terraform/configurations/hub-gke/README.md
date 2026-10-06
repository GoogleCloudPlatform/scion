# configurations/hub-gke: one Scion hub on GKE

This configuration deploys one Scion hub into the existing shared GKE Autopilot
cluster, using the in-repo Helm chart (`deploy/helm/scion-hub`). A global
external HTTPS load balancer with IAP, owned by Terraform, sits in front of it.
It is a sibling of `configurations/hub` (the Cloud Run hub). The two share the
shared infra, the per-hub naming rules and the state layout.

```
Internet -> global external ALB (managed cert, IAP on the backend service,
            timeout_sec 86400) -> zonal standalone NEGs -> hub pod (chart) in GKE
```

The load balancer is Terraform resources, not a GKE Ingress or Gateway, so the
24-hour backend timeout (the maximum WebSocket lifetime) and IAP stay under
Terraform's control.

## What it creates

| Module | Creates |
|---|---|
| `shared-lookup` | nothing; it reads the shared infra by naming convention |
| `hub-identity` | the hub, transport and agent GSAs and their IAM, plus (opt-in `hub_workload_identity_ksa`) `roles/iam.workloadIdentityUser` on the hub GSA for `<hub>-system/scion-hub` only |
| `cloudsql-database` | the hub's database, user and password |
| `agent-runtime-k8s` | the agent namespace `<hub>`, the agent KSA's Workload Identity binding, and the NFS PV/PVC. With `create_hub_rbac = false`, the chart owns the hub's RBAC instead |
| `hub-lb` | global IP (`prevent_destroy`), managed cert, HTTPS proxy and forwarding rule, HTTP->HTTPS redirect, health check (`/readyz` on 8080), health-check firewall rule (destination: the pod range only), backend service (IAP, Google-managed OAuth client), and IAP accessor grants |
| `hub-gke` | namespace `<hub>-system`, the NEG Service, the session secret, the artifacts bucket and the `helm_release` |

**Front-door contract.** `hub-gke` takes exactly two values from the front
door: `public_url` and `iap_audience`. It references no other `hub-lb`
attribute, so a different front (for example a future Cloud Run IAP front)
that produces those two values can replace `hub-lb`.

**No DNS resources.** DNS is external. See "DNS" below.

## Prerequisites

- `configurations/shared-infra` is applied, and its `name_prefix` is this
  configuration's `shared_prefix`.
- The hub-gke image is built and pushed to the shared Artifact Registry repo as
  `<repo>/scion-hub-gke` (see `image-build/cloudbuild-hub-gke.yaml`).
- `hub_image_digest` is set in your tfvars to **that build's** digest
  (`sha256:<64 hex>`). It is required and has no default: a digest exists only
  in the registry it was pushed to, so another build's digest will not pull.
  Read it from the build output or the Artifact Registry console.
- The shared cluster's control plane runs GKE **1.36.2-gke.3104000 or later**.
  The NEG Service uses the annotation's `zones` field (see "NEG zones"), and
  plan fails with a clear message on an older cluster.
- The operator has the roles listed in `../../README.md` ("Prerequisites").
  Kubernetes and Helm both authenticate as the operator's own Google identity
  against the cluster, so `roles/container.admin` covers them.

## Apply sequence

```bash
HUB=tfha-gke-h3
PREFIX=tfha
cd deploy/terraform/configurations/hub-gke
cp terraform.tfvars.example $HUB.tfvars    # then fill it in; tfvars are gitignored

terraform init \
  -backend-config="bucket=<project>-$PREFIX-tfstate" \
  -backend-config="prefix=$PREFIX/hubs/$HUB"

terraform plan  -var-file=$HUB.tfvars -out=/tmp/$HUB.tfplan
terraform apply /tmp/$HUB.tfplan
```

`state_prefix` in the tfvars must equal the `-backend-config` prefix, and
`hub_name` must start with `<shared_prefix>-`. Validation refuses anything else,
so one hub's variables can never be applied onto another hub's state. Cloud Run
hubs and GKE hubs share the `<prefix>/hubs/` namespace.

Everything happens in **one apply**, in this order. Terraform derives the order
from references; nothing here sleeps or shells out.

1. `hub-gke` creates the NEG Service (`kubernetes_service_v1 "<hub>-neg"`,
   annotated `cloud.google.com/neg: {"exposed_ports":{"8080":{"name":"<hub>-hub-neg"}}}`
   and selecting the chart's pods by `app.kubernetes.io/name` and
   `app.kubernetes.io/instance`). The annotation also carries
   `"zones": [<neg_zones>]`, so the GKE NEG controller creates one standalone
   NEG in every listed zone, with or without nodes there.
2. `hub-lb` reads each zonal NEG (`data "google_compute_network_endpoint_group"`,
   one per zone in `neg_zones`).
3. `hub-lb` creates the backend service and URL map, which produce `iap_audience`.
4. `hub-gke` installs the `helm_release`, with `auth.proxy.iap.audience` set to
   that `iap_audience`.

## First install: the IAP OAuth client ID

The chart needs `auth.transport.oidcAudience`, which is the IAP OAuth client ID
that agents present as their token audience. It refuses to render without it,
and the hub's HA preflight accepts no other transport mode on this shape
(postgres + gcs + IAP proxy). So `iap_oauth_client_id` controls whether the hub
itself is installed:

| `iap_oauth_client_id` | Result |
|---|---|
| unset (null) | Everything **except** the hub: namespace, NEG Service and NEGs, the load balancer with IAP, IAM, database and bucket. `helm_release` is skipped, the output `hub_installed = false`, and the `transport_audience_configured` check warns on every plan. |
| set | The hub is installed too, and `hub_installed = true`. |

**Two-step first install (the general case).**

1. Apply with `iap_oauth_client_id` unset. The backend service is created with
   IAP on, which uses the project's Google-managed OAuth client.
2. Discover the client ID, read-only. In the console, open Security ->
   Identity-Aware Proxy, find the `<hub>-hub-backend` backend service, and read
   its OAuth client. Google Auth Platform -> Clients also lists it. (The
   `gcloud alpha iap oauth-clients list` convenience command described in
   `../../README.md`, "IAP OAuth client", may also work, but it announces its
   own shutdown, so don't rely on it.)
3. Set `iap_oauth_client_id = "<id>.apps.googleusercontent.com"` in the tfvars
   and apply again. That apply installs the hub.

**A single-apply install may be possible.** The Google-managed client is per
project. If the project already runs IAP-protected hubs, the operator may
already know the client ID, for example from an existing hub's tfvars. Setting
it on the first apply should then install the hub in one go. This is possible
but not guaranteed: confirm the ID shown on this hub's backend service after
the apply matches what you set. The operator verifies this path on the first
live install.

**Do not unset it later.** With the release gated on this variable, going back
to null plans a **destroy** of the hub's `helm_release`.

## DNS

Terraform creates no DNS records. After the apply:

```bash
terraform output dns_record    # { name = "<hostname>", type = "A", value = "<lb_ip>" }
```

Create that A record in your DNS provider. The managed certificate stays
`PROVISIONING` until the record resolves to `lb_ip`, and until then HTTPS fails
with a certificate error. That is expected, and no apply waits for it. Check
progress with:

```bash
gcloud compute ssl-certificates list --filter="name~^<hub>-hub-"
```

The IP has `prevent_destroy` because it is the one value copied into DNS by
hand. See "Destroy".

## NEG zones

**The documented default is nodes-only zones.** Google's GKE docs say: "By
default, the GKE NEG controller creates a standalone NEG only in the zones
where the cluster has nodes." The cluster's `node_locations` lists every zone
nodes *may* run in, and a small Autopilot cluster often has no nodes in some of
them. Without the `zones` field below, `hub-lb`'s read of the NEG in such a
zone fails with a not-found error on every apply, and re-running does not help.

So `hub-gke` adds the optional `zones` field to the NEG annotation:

```
cloud.google.com/neg: {"exposed_ports":{"8080":{"name":"<hub>-hub-neg"}},"zones":["us-central1-a","us-central1-b","us-central1-c"]}
```

With an explicit list, the controller pre-provisions a NEG (empty until pods
land there) in each listed zone, plus any other zone that has nodes. `hub-lb`
reads exactly the listed zones, so every zone it reads has a NEG.

Source: <https://docs.cloud.google.com/kubernetes-engine/docs/how-to/standalone-neg>,
"Pre-provisioning empty NEGs". Points from that page that matter here:

- It needs GKE **1.36.2-gke.3104000 or later**. The `data
  "google_container_cluster" "agents"` postcondition parses the cluster's
  `master_version` as numbers (major, minor, patch, GKE build) and fails the
  plan on anything older or unparseable.
- The page marks pre-provisioning as **Preview**.
- Every listed zone must be in the cluster's region. The list comes from the
  cluster's own `node_locations`, so it always is.
- A malformed `zones` value is not an error: the controller falls back to
  nodes-only zones and raises a Warning event on the Service. If an apply fails
  at a NEG read, run `kubectl -n <hub>-system describe service <hub>-neg`
  and check its events.
- Empty NEGs count against the project's NEG quota.

**There is no zone override.** The zone list is always the cluster's
`node_locations`, used in both the annotation and `hub-lb`'s reads. The
`neg_zones` output shows it. An override is left out on purpose. The
annotation's `zones` field only adds zones: the controller still creates a NEG
in every zone that has nodes, but `hub-lb` attaches only the listed zones. With
a subset, a hub pod scheduled in an unlisted zone would sit in a NEG the load
balancer never sends traffic to. Requests routed to it would get 503s, while
helm still reports the release as healthy. Listing every zone the nodes may
run in rules that out.

### NEG race (known failure mode)

The NEG data reads in step 2 depend on the GKE NEG controller having created the
NEGs. `hub-gke`'s `neg_name` output is unknown until the NEG Service exists, so
on a fresh hub the reads move to apply time, after the Service is created. A
plan shows them as `will be read during apply`. They are not retried, and this
configuration deliberately adds **no** sleeps, `local-exec` or `time_sleep` to
cover them.

If the controller is slower than the read, the apply fails at
`module.hub_lb.data.google_compute_network_endpoint_group.this["<zone>"]`
with a not-found error for `<hub>-hub-neg`. The NEG Service and everything
before it already exist. **Recovery:** run the same plan/apply again. Once the
NEGs exist, later plans read them at plan time.

This has not been measured on a live cluster yet; the operator will measure
it on the first live install. If the race proves real, a later phase may
switch to a two-phase apply (NEG Service first, then everything else).

## Hub pod identity and boot

- The chart creates the KSA `scion-hub` in `<hub>-system`, annotated with the
  hub GSA. `hub-identity` grants that one KSA `roles/iam.workloadIdentityUser`
  on the hub GSA. The hub reaches Secret Manager (`gcpsm`), Cloud SQL (through
  the proxy sidecar) and GCS as the hub GSA.
- The release depends on those grants existing (`boot_prerequisites`), but IAM
  takes time to propagate. If the first pod starts before it has, expect
  restarts until it settles. There is no sleep for it.
- The chart renders its own Role/RoleBinding in the agent namespace `<hub>` for
  its KSA, so `agent-runtime-k8s` runs with `create_hub_rbac = false`.

## Rotating the DB password

This works as in `configurations/hub` (see `../../README.md`, "Rotating a hub's
DB password"): set `db_password_rotation` to a new value in the tfvars and
keep it there permanently. The chart deliberately does not roll pods on a
password-only change, because its settings checksum excludes the credential.
So `hub-gke` puts the marker (never the password) in a pod annotation,
`scion.io/db-password-rotation`, and the same apply rolls the pods.

## Health-check firewall

`<hub>-hub-allow-lb-hc` allows `35.191.0.0/16` and `130.211.0.0/22` to TCP 8080
on the shared network, with the cluster's pod range as the only destination
(`destination_ranges`, from the cluster's
`ip_allocation_policy[0].cluster_ipv4_cidr_block`). NEG endpoints are pod IPs,
so the rule reaches the pods and no other VM or alias IP in the shared VPC.
Autopilot nodes carry no tags Terraform controls, so the rule is scoped by
port, source range and destination range rather than by target tag.

That scope is the whole pod range, not just this hub. The rule admits the
Google health-check and GFE ranges to tcp/8080 on **every pod in the
cluster**: every hub's pods and every agent pod, not only this hub's. This is
accepted because Autopilot offers no tighter scope without manual steps.
A single shared-infra rule, replacing the per-hub rules, is tracked in
ptone/scion#3519.

## Outputs

| Output | Meaning |
|---|---|
| `public_url` | `https://<hostname>` |
| `iap_audience` | `/projects/<number>/global/backendServices/<id>` |
| `lb_ip`, `dns_record` | the IP and the record to create in DNS |
| `hub_installed` | false while `iap_oauth_client_id` is unset |
| `backend_service` | name, `timeout_sec`, health-check path and port |
| `hub_image` | `<repo>/scion-hub-gke@<digest>` |
| `chart_values` | every non-secret value handed to the chart |
| `neg_zones` | zones the NEGs are created in and read from (the cluster's `node_locations`) |

## Destroy

The global address has `prevent_destroy`, so `terraform destroy` of this
configuration fails on purpose. To retire a hub, first remove the
`prevent_destroy` line from `modules/hub-lb/main.tf` in a deliberate change,
then destroy. Expect the hub's DNS record to stop resolving. The agent
namespace's NFS data follows the same rules as a Cloud Run hub (see
`../../README.md`, "Destroy runbook").

## Tests

```bash
terraform init -backend=false
terraform validate
terraform test
```

The tests run with mock providers and need no credentials. They prove the root
composes and plans, the front-door contract, the LB's timeout and health check,
the digest pin, `auth=password`, `create_hub_rbac = false`, both
first-install states, the NEG zones (from `node_locations`) and the GKE version
check, and the firewall's pod-range destination. They cannot prove the APIs accept the resources, the NEG
timing, or that the hub boots. Those are live checks.
