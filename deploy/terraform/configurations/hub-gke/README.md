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
| `hub-lb` | global IP (`prevent_destroy`), managed cert, HTTPS proxy and forwarding rule, HTTP->HTTPS redirect, health check (`/readyz` on 8080), health-check firewall rule, backend service (IAP, Google-managed OAuth client), and IAP accessor grants |
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
  `<repo>/scion-hub-gke` (see `image-build/cloudbuild-hub-gke.yaml`), and its
  digest is the one in `hub_image_digest` (the default, or your tfvars).
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
   `app.kubernetes.io/instance`). The GKE NEG controller then creates one
   standalone NEG per zone.
2. `hub-lb` reads each zonal NEG (`data "google_compute_network_endpoint_group"`,
   one per zone in the cluster's `node_locations`).
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
the apply matches what you set. vm-deploy verifies this path.

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

## NEG race (known failure mode)

The NEG data reads in step 2 depend on the GKE NEG controller having created the
NEGs. `hub-gke`'s `neg_name` output is unknown until the NEG Service exists, so
on a fresh hub the reads move to apply time, after the Service is created. A
plan shows them as `will be read during apply`. They are not retried, and this
configuration deliberately adds **no** sleeps, `local-exec` or `time_sleep` to
cover them.

What can go wrong, and what it looks like:

- **The controller is slower than the read.** The apply fails at
  `module.hub_lb.data.google_compute_network_endpoint_group.this["<zone>"]`
  with a not-found error for `<hub>-hub-neg`. The NEG Service and everything
  before it already exist. **Recovery:** run the same plan/apply again. Once
  the NEGs exist, later plans read them at plan time.
- **A zone has no NEG.** One NEG is read per zone in the cluster's
  `node_locations` (output `neg_zones`). If the controller only creates NEGs in
  zones that currently have nodes, a zone without Autopilot nodes fails the
  read every time, not just once. Re-running won't fix that.

Neither case has been measured on a live cluster yet. vm-deploy will measure
both. If the race proves real, a later phase may switch to a two-phase apply
(NEG Service first, then everything else).

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
on the shared network. Autopilot nodes carry no tags Terraform controls, so the
rule is scoped by port and source range rather than by target tag.

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
| `neg_zones` | zones a NEG is read from |

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
the digest pin, `auth=password`, `create_hub_rbac = false`, and both
first-install states. They cannot prove the APIs accept the resources, the NEG
timing, or that the hub boots. Those are live checks.
