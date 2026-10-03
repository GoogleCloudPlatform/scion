# Scion HA deployment: Terraform

Terraform for the Cloud Run hub + IAP, Cloud SQL Postgres, GKE Autopilot
runtime, Filestore NFS pattern, with multiple hubs sharing one project's
infra. This README covers what an operator needs to actually run it: the
overall design rationale is tracked in `ptone/scion#1840`.

**Status.** The vertical slice (one shared-infra apply plus one hub) and
HA hardening (a second hub on the same shared infra, `REGIONAL` Cloud SQL,
`check` blocks, bucket lifecycle, password rotation) have both been
validated end-to-end against a real project. Modularity seams
(`shared_overrides` for hand-built infra, moving every remaining variable
to typed `validation`, per-module READMEs) are not implemented yet.

**Docs.** This README is the detailed reference. For a short operator
how-to, see [`docs/deploy/terraform-ha.md`](../../docs/deploy/terraform-ha.md).
For an AI agent running this end to end, see
[`docs/deploy/agent-runbook-terraform-ha.md`](../../docs/deploy/agent-runbook-terraform-ha.md).

## Layout

```
modules/
  project-services/ network/ cloudsql-instance/ filestore/ gke-autopilot/
  artifact-registry/          # shared layer
  shared-lookup/ cloudsql-database/ hub-identity/ agent-runtime-k8s/
  hub-cloudrun/                # hub layer
configurations/
  shared-infra/   # apply once per project
  hub/            # apply once per hub
```

Modules declare no `provider` or `backend` blocks — only the two
`configurations/*` roots do. The hub root never manages shared infra; it only
reads it, by naming convention, through the `shared-lookup` module (no
`terraform_remote_state`).

## Prerequisites (manual, once per project)

1. A project with billing enabled. The operator applying this Terraform
   needs Owner or an equivalent role set covering: Editor,
   `roles/compute.networkAdmin`, `roles/container.admin`,
   `roles/run.admin`, `roles/resourcemanager.projectIamAdmin`,
   `roles/iam.serviceAccountAdmin`, `roles/servicenetworking.networksAdmin`,
   `roles/storage.admin`, `roles/iap.admin`, `roles/secretmanager.admin`
   (needed for the `google_secret_manager_secret_iam_member` resources in
   `cloudsql-database` and `hub-cloudrun` — `resourcemanager.projectIamAdmin`
   covers only project-level policy, and Editor covers no IAM policy at all,
   so neither reaches `secretmanager.secrets.setIamPolicy` on a resource; a
   narrower custom role granting just that permission also works). This is
   a role on the *operator* identity applying Terraform, separate from the
   hub service account's conditioned, hub-prefixed `secretmanager.admin`
   grant discussed in "Troubleshooting" below — the two are not in tension.
   Grant all of these up front — a partial role set surfaces as a plan or
   apply failure partway through, not as a clean early error.
2. A GCS state bucket, versioned: `<project>-<name_prefix>-tfstate` (e.g.
   `my-project-tfha-tfstate`). Access limited to operators.
3. ~~An IAP OAuth web client~~ — **not a prerequisite.** `iap_enabled = true`
   works immediately with the project's Google-managed OAuth client; see
   "IAP OAuth client" below. It's a post-apply step (to enable agent
   transport), not something you need before the first apply.
4. The hub image, built and pushed to the Artifact Registry repo that
   `shared-infra` creates. There is no single-apply bootstrap trick here (the
   two-root split already separates "create the repo" from "use the image"):
   apply `shared-infra` first, build/push, then apply `hub`. **This module
   version requires a hub image that includes ptone/scion#2152 or later** —
   on an older image, a fresh hub can't read or create its hub-scope OIDC
   secret and fails to boot. See
   [`docs/deploy/agent-runbook-terraform-ha.md`](../../docs/deploy/agent-runbook-terraform-ha.md)
   for the upgrade note if you're applying this against an existing hub.

## Bootstrap sequence

```bash
# 1. Shared infra, once per project.
terraform -chdir=deploy/terraform/configurations/shared-infra init \
  -backend-config="bucket=<project>-<prefix>-tfstate" \
  -backend-config="prefix=<prefix>/shared"
terraform -chdir=deploy/terraform/configurations/shared-infra apply \
  -var-file=terraform.tfvars

# 2. Build and push hub_image using the repo shared-infra just created
#    (out of scope for this Terraform — see the agent runbook's step 5,
#    "Build and Push Images": immutable tags only, :latest moves only on
#    explicit ack, and the hub image is built for linux/amd64).

# 3. One hub.
terraform -chdir=deploy/terraform/configurations/hub init \
  -backend-config="bucket=<project>-<prefix>-tfstate" \
  -backend-config="prefix=<prefix>/hubs/<hub_name>"
terraform -chdir=deploy/terraform/configurations/hub apply \
  -var hub_name=<hub_name> -var state_prefix=<prefix>/hubs/<hub_name> \
  -var-file=<hub_name>.tfvars
```

`state_prefix` must always be passed and must equal the `-backend-config
prefix` used at `init` — Terraform cannot read its own backend config back,
so this is enforced by a `hub_name` variable `validation` block in
`configurations/hub` (not a `check` block: a `check` only warns, which would
still let the apply proceed onto the wrong hub's state). Getting it wrong
fails the plan loudly instead of silently applying one hub's variables onto
another hub's state.

Each further hub is just step 3 again with a new `hub_name`/prefix — the
shared layer is untouched.

## Post-apply step: Vertex environment for agents

Before an agent can start on a fresh hub, an admin must set two hub-scope
environment values:

```bash
scion hub env set --scope hub --always GOOGLE_CLOUD_PROJECT=<project>
scion hub env set --scope hub --always GOOGLE_CLOUD_REGION=<region>
```

(`GOOGLE_CLOUD_LOCATION` is accepted as an equivalent alternative to
`GOOGLE_CLOUD_REGION`.) Without this, an agent using GCP-identity auth
(Workload Identity or an assigned service account) fails to start with
`2 required environment variable(s) are missing: GOOGLE_CLOUD_PROJECT,
GOOGLE_CLOUD_LOCATION` — the runtime broker's preflight check rejects the
create before it ever reaches the pod.

This is a **manual, post-apply, per-hub step, not a Terraform variable**.
No key in `settings.yaml` reaches the broker's preflight: `harness_configs
.<h>.env` and `runtimes.<n>.env` are only applied later, after the
preflight has already run and (without the values above) already failed.
The hub-scope environment store is a database row set through the hub's own
admin API/CLI, not a file Terraform renders, and this Terraform module set
makes no `local-exec` calls, so it cannot run this step for you. Set it once
per hub, right after that hub's first apply.

### GCP identity for agents

Getting `GOOGLE_CLOUD_PROJECT`/`GOOGLE_CLOUD_REGION` set is not enough on
its own for an agent to authenticate to Vertex. This module (`hub-identity`,
`agent-runtime-k8s`) wires Workload Identity so the namespace's `default`
KSA maps to the `<hub>-agent` service account, which holds
`roles/aiplatform.user` by default. But a **new agent's GCP identity
defaults to Block** — the metadata server is blocked, no identity is
reachable, and the agent comes up unauthenticated to Vertex even though the
Workload Identity binding above is fully in place.

The setting to change is the agent's **GCP Identity** field (agent create/
configure), or a project's **Default Service Account** field (project
settings), which sets the default new agents in that project pick up. Both
expose the same three modes; the labels differ slightly by page: the
per-agent field offers **Block**, **Assign Service Account**, and
**Passthrough**; the project-level field offers **Block**, **Passthrough**,
and **Assign Service Account** (plus an inherited "None (default to
block)"). To let the Workload Identity binding actually reach the agent,
set **Passthrough** — it removes the metadata-server interception so the
agent inherits the broker's ambient GCP identity instead of being denied
one. See ptone/scion#2009 for the default-block behavior this addresses.

Passthrough only gets the agent an identity; it does not itself grant
access to a model. The model(s) an agent will call must also be enabled
for the project in Vertex AI Model Garden — that enablement is a
per-project step outside this Terraform.

## Harness images

Agents pull `<image_registry>/scion-<harness>:<tag>` (`image_registry` is
this module's variable of that name — see `configurations/hub/
variables.tf`). The set of harnesses a user can pick is the `harnesses/`
directory in the repo root, mirrored for the UI's fallback list in
`web/src/shared/harness-utils.ts`'s `KNOWN_HARNESS_NAMES`: at the time of
writing, `claude`, `codex`, `copilot`, `gemini-cli`, `opencode`,
`antigravity`, `hermes`, `grok-build`, and `muse-code`.

The image pipeline (`image-build/`) must publish an image for every one of
those harnesses into whatever registry `image_registry` points at — at
minimum `core-base`, `scion-base`, and each `scion-<harness>` (e.g.
`scion-claude`). A harness that exists in the catalog and is selectable in
the UI but has no published image builds an agent record fine and then
fails at pod start: the `workspace-provision` init container hits an
image-pull `NotFound` for that harness's image. `muse-code` is an example
of the gap today — it has a `harnesses/muse-code/config.yaml` and is listed
in `KNOWN_HARNESS_NAMES`, but `image-build/cloudbuild-harnesses.yaml` does
not build it. Check the harness catalog against
`cloudbuild-harnesses.yaml`'s build steps before offering a harness, and
whenever either list changes.

## Cold start

Agent create is synchronous upstream: the hub waits for the create to
actually finish before responding. On a fresh GKE Autopilot node, the
first agent pays for node provisioning plus pulling a roughly 1 GB harness
image, and that combined wait can exceed the hub's own client timeout for
calls to the runtime broker, which is hard-coded upstream (not a
Terraform variable, not in `settings.yaml`). When that happens the UI
shows a 503 even though the agent goes on to start moments later.
Subsequent agents on an already-warm node skip both costs and comfortably
finish inside the timeout.

This module raises the hub's own write timeouts (`hub_write_timeout`,
`broker_write_timeout` — see `modules/hub-cloudrun/variables.tf`) so the
hub's HTTP server itself no longer cuts a slow create off early. The
remaining limit — the hub-to-runtime-broker client timeout — is upstream
Go code this Terraform cannot reach; the durable fix is making agent
create asynchronous upstream, not a larger value here.

## IAP OAuth client

`iap_enabled = true` on the Cloud Run service turns on direct IAP using the
project's **Google-managed** OAuth client immediately — there is nothing to
create, and no Terraform input is required for the first apply. Terraform
manages no `google_iap_settings` and holds no OAuth client secret in state.

`iap_oauth_client_id` (optional, default `null`) feeds exactly one thing:
`settings.yaml`'s `auth.transport.oidc_audience` — the audience agents'
transport tokens must present when calling the hub over IAP. With it unset,
the hub and IAP browser login both work fine; only agent transport is
disabled (a `check` block warns on every plan/apply until it's set).

Two ways to get a real value, both post-apply:

**(a) In-org (the common case): discover the Google-managed client ID,
read-only, and re-apply.**

The durable way to find it is the **console**: Security → Identity-Aware
Proxy → the service's OAuth settings (or Google Auth Platform → Clients).
A convenience command exists for the same read-only lookup:
```bash
gcloud alpha iap oauth-clients list projects/<project_number>/brands/<brand_number>
```
but treat it as just that — it currently prints its own March 2026
shutdown warning, so don't depend on it as the only way to find the ID; the
console is where to look if/when it stops working.

This client exists in any project where IAP has ever been enabled —
including a project that already runs a live Scion stack — so on a
project like that, the first apply can pass it right away and wait on
nothing. In a genuinely fresh project it
only appears after IAP has been turned on by a first apply, so the flow there is
apply → discover → re-apply with `-var iap_oauth_client_id=<id>`. Changing
the value only re-renders the settings secret and rolls a new revision;
nothing else changes.

**(b) Cross-org: create a custom OAuth client in the console, set it on the
service's IAP settings, and re-apply immediately.** The IAP OAuth Admin API
no longer supports creating clients, so this is a console-only step:
1. APIs & Services → Credentials → Create Credentials → OAuth client ID,
   application type Web application.
2. Authorized redirect URI:
   `https://iap.googleapis.com/v1/oauth/clientIds/<CLIENT_ID>:handleRedirect`.
3. On the Cloud Run service's IAP settings (Security → Identity-Aware Proxy
   in the console), select this client.
4. **Re-apply right away** with `-var iap_oauth_client_id=<new client
   id>.apps.googleusercontent.com`. Until you do, IAP is already expecting
   the new audience and agent transport fails — this is a real, named
   outage window for agent traffic (not for human browser login, which IAP
   handles independently of this setting), so don't leave it dangling.

## Everything is prefixed; nothing is adopted

Every resource name derives from `name_prefix` (shared layer, default
`tfha`) or `hub_name` (hub layer). There are no unprefixed defaults, no
`import` blocks, and no create-if-missing logic: a name collision fails the
apply, which is the desired behavior on a project that also runs a live
Scion stack (e.g. `scion-hub`, `scion-hub-iap-proxy`,
`scion-a2a-bridge`, `scion-discord`, `scion-hub-runner`, `scion-hub-gke`).
All IAM grants are additive (`google_*_iam_member` only — never
`_iam_binding`/`_iam_policy`).

## Shared-infra trust domain and sizing

Hubs sharing one project's infra form one trust domain, not a hard
multi-tenancy boundary:

- A hub can create pods in its own namespace with an inline NFS volume
  mounted at the Filestore share root, so it can reach every other hub's
  directory on that share. Namespacing and RBAC stop a hub's *service
  account* from touching another hub's Kubernetes objects, but nothing
  stops a pod that mounts the raw share.
- Cloud SQL built-in database users all belong to `cloudsqlsuperuser` on
  the shared instance.
- What *is* isolated: separate databases and users, directories,
  namespaces with namespaced RBAC, service accounts, buckets, Cloud Run
  services, IAP policies and Terraform states per hub. Each hub's Secret
  Manager reach is scoped to its own `<hub>-*` secrets plus its own
  hub-scope prefix (see `hub-identity`'s IAM scope rule) — hubs cannot
  read each other's secrets or the live stack's.

This is fine for cost-sharing during development. Production multi-tenant
isolation needs dedicated infra per hub (a variation built from the same
modules, not yet implemented).

Size Cloud SQL's `max_connections` for the sum across hubs — see "Scaling"
below for the per-hub connection budget this module checks.

## Health endpoints

The hub exposes `/readyz` and `/healthz`, and `hub-cloudrun` wires up a
**startup probe on `/readyz` only — there is deliberately no liveness
probe.**

- **`/readyz`** answers `{"status":"ready"}`/200 once `store.Ping` succeeds
  and, because this module always sets `workspace_storage.backend: nfs`,
  the NFS mount also checks healthy; otherwise it's 503 `not_ready` with a
  reason. This is what gates traffic to a fresh revision (the startup
  probe), and what "the hub is up" means operationally.
- **`/healthz`** (aliased as `/health` on Cloud Run, where the literal path
  `/healthz` is reserved by Cloud Run's own infrastructure and 404s) always
  returns 200, but the handler it calls does unbounded work first:
  `store.Ping`, three more `List` queries against the same connection pool,
  and an NFS mount check — with no timeout of its own. A Postgres stall or
  pool exhaustion hangs this handler just as surely as it would hang
  `/readyz`.
- **There is no liveness probe, on purpose.** If `/healthz` (or any
  DB-touching endpoint) were wired up as a liveness probe, a shared Cloud
  SQL blip would make every hub instance fail its liveness check at once,
  and Cloud Run would restart all of them simultaneously — a
  self-inflicted thundering herd on top of the DB problem that caused it.
  There is no DB-free endpoint upstream to point a liveness probe at
  today; one should be added upstream before this module adds a liveness
  probe.

## Scaling

`max_instances` defaults to **3** (`min_instances` stays at **1**).
Multi-instance operation needs a hub image built from a commit that
contains `GoogleCloudPlatform/scion#2046`, which fixes
`ptone/scion#2090`: when the Cloud Run instance that owns *broker
affinity* goes away (scale-in, revision retirement, or graceful
shutdown), it marks every project's runtime-broker provider **offline**
for the shared `broker_id`. `#2046` adds a per-instance recurring
handler that re-marks a broker's provider rows **online** on every
scheduler tick (default 1 minute) for any broker that instance still
holds a live control-channel connection to, so there can be a short
window — up to about a minute, plus up to 30s of scheduler jitter —
after an affinity-owner scale-in before providers are restored on the
surviving instances.

On a hub image built **before** `#2046`, set `max_instances = 1`:
without the self-heal handler, an affinity-owner scale-in leaves the
shared `broker_id`'s providers stamped offline with nothing to bring
them back, and the next agent-create on an affected project fails with
"Default runtime broker is unavailable" until some instance reconnects
(often requiring a manual restart). `min_instances` stays at 1
regardless: broader multi-replica safety of the in-process runtime
broker beyond this one defect is not fully proven yet.

Whatever `max_instances` you run, `hub-cloudrun` checks a per-hub
**connection budget**: `max_instances × database.max_open_conns` (10,
single-sourced with the rendered settings) must not exceed
`max_connections_budget` (default 40) — at the new default that's
`3 × 10 = 30`, within budget. The shared Cloud SQL instance's own
`max_connections` (default 200, `shared-infra`'s `sql_max_connections`)
must in turn cover the sum of every hub's budget attached to it — see
"Shared-infra trust domain and sizing" above.

## SQL availability

The default is **`REGIONAL`** (`cloudsql-instance`'s `availability_type`),
giving Cloud SQL automatic failover to a standby in a second zone.
`ZONAL` is available for a smaller/cheaper dev footprint but gives up
failover entirely.

**Converting an existing `ZONAL` instance to `REGIONAL` is an in-place
update, not a replacement** — but it triggers a restart of the shared
instance with an observed outage of **about 7 minutes**. Every hub
attached to the shared instance sees a window of `5xx`s and
heartbeat/settings-refresh errors on their own `POST /status` calls during
that window; it self-recovers with no hub restarts once the instance is
back. **Schedule this**, the same way you would schedule any shared-infra
maintenance, rather than running it against a project with active agents.

A deliberate failover test (`gcloud sql instances failover`) has **not**
been run against this module set. `REGIONAL`'s automatic-failover behavior
is Cloud SQL's own documented guarantee, not something re-verified here.

## GKE deletion protection is Terraform-only

Unlike Cloud SQL (`deletion_protection` + `settings.deletion_protection_enabled`)
and Filestore (`deletion_protection_enabled` + `deletion_protection_reason`),
`google_container_cluster` has no separate API-level deletion-protection
field — only the one Terraform-level `deletion_protection` attribute, which
blocks `apply`/`destroy` from replacing or deleting the cluster while true.
There is no equivalent GCP API guard for the GKE cluster the way there is
for SQL/Filestore.

## Destroy runbook

**Order: every hub root first, then shared.** Never the reverse. This is
enforced by several complementary, deliberately redundant layers —
`terraform destroy` skips lifecycle preconditions entirely (see
"Why the interlock alone isn't enough" below), so no single one of these is
sufficient on its own:

> **Prohibited, always, without exception (verbatim from the design):**
> - Never run `terraform destroy -var deletion_protection=false` against
>   `shared-infra` outside step 2 below.
> - Never use `-target` together with `deletion_protection` on
>   `shared-infra`.
>
> Both turn the API-level protection flags off on `tfha-pg`/`tfha-nfs` while
> hubs may still be present, without ever evaluating `destroy_guard` (a
> `-target` apply skips it because it isn't targeted; a plain `destroy`
> skips it because Terraform doesn't evaluate preconditions on resources
> being destroyed). Doing either is two deliberate deviations from this
> runbook, not an accident — see the residual-risk note below.

Step 2 below is an `apply`, not a `destroy`; there is no step in this
runbook at which `terraform destroy -var deletion_protection=false` against
`shared-infra` is permitted.

1. For each hub: empty its artifacts bucket, **noncurrent versions
   included** — the bucket is versioned and deliberately has no
   `force_destroy`, so `destroy` fails otherwise:
   ```bash
   gcloud storage rm -r --all-versions gs://<project>-<hub>-artifacts/**
   ```
   Then plan and apply the destroy — never a bare `destroy` — through the
   same plan-review gate as any other apply:
   ```bash
   terraform -chdir=deploy/terraform/configurations/hub plan -destroy \
     -var hub_name=<hub_name> -var state_prefix=<prefix>/hubs/<hub_name> \
     -var-file=<hub_name>.tfvars -out=/tmp/<hub_name>-destroy.tfplan
   terraform -chdir=deploy/terraform/configurations/hub apply /tmp/<hub_name>-destroy.tfplan
   ```
   This removes only that hub's resources; it never touches shared infra,
   because the hub root only reads shared infra via data sources.
2. A normal (non-destroy) `plan`/`apply` that only flips
   `deletion_protection` off — not `plan -destroy`, since `destroy_guard`
   below is a `plan`/`apply`-time check, not a `terraform destroy`:
   ```bash
   terraform -chdir=deploy/terraform/configurations/shared-infra plan \
     -var-file=terraform.tfvars -var deletion_protection=false \
     -out=/tmp/shared-unprotect.tfplan
   terraform -chdir=deploy/terraform/configurations/shared-infra apply /tmp/shared-unprotect.tfplan
   ```
   The `terraform_data.destroy_guard` precondition makes the **plan** above
   **fail** while any hub database still exists on the shared Cloud SQL
   instance — every hub creates exactly one, so this is the proxy for "a hub
   still exists". This apply succeeds once every hub is gone.
3. On a teardown branch (never merged to `main`), commit removing
   `lifecycle { prevent_destroy = true }` from the three shared stateful
   modules (`cloudsql-instance`, `filestore`, `gke-autopilot`). This is a
   literal in each module, not a variable — Terraform doesn't allow a
   variable-driven `prevent_destroy` — so intentional teardown costs a
   one-line commit per module. That friction is intended for infra every
   hub depends on.
4. From that branch, plan and apply the destroy — never a bare
   `destroy` — through the same plan-review gate:
   ```bash
   terraform -chdir=deploy/terraform/configurations/shared-infra plan -destroy \
     -var-file=terraform.tfvars -out=/tmp/shared-final-destroy.tfplan
   terraform -chdir=deploy/terraform/configurations/shared-infra apply /tmp/shared-final-destroy.tfplan
   ```
5. The operator compares a before/after `tfha*` resource inventory to
   confirm nothing outside the prefix was touched, and that nothing was
   left behind.

**Why the interlock alone isn't enough, and why step 3 exists.** Guardrail
2 above (`destroy_guard`) only protects the *state transition* from
protected to unprotected — but `terraform destroy` never evaluates lifecycle
preconditions on the resources it's destroying, `destroy_guard` included. A
direct `terraform destroy -var deletion_protection=false` against
`shared-infra` walks straight past it. What's left at that point: the
API-level flags refuse `tfha-pg` and `tfha-nfs`, and the provider refuses
`tfha-agents` by reading `deletion_protection` from state — but **nothing
stops the Artifact Registry repo, the subnet, the PSA address, or the
service-networking connection**, none of which carry any protection. The
result is a *partial* destroy: the data resources survive, orphaned from
their own network, with Terraform state disagreeing with reality — worse
than a clean loss, because the natural recovery (re-apply) is exactly where
an accidental adopt-then-destroy of live resources happens. `prevent_destroy`
(step 3) closes this: it fails at **plan** time, before anything is applied,
so a full (or `-target`) destroy of protected plumbing aborts entirely
instead of partially succeeding.

**Residual, not closed:** `apply -target=module.<x> -var
deletion_protection=false` skips `destroy_guard` (it isn't targeted) and is
an in-place *update*, which `prevent_destroy` doesn't cover — it can turn
the API flags off on live shared resources with hubs still present, after
which an out-of-band `gcloud … delete` would succeed. This needs two
deliberate deviations from this runbook to reach; it is prohibited above,
not enforced in config. GKE deletion protection is also Terraform-only (see
"GKE deletion protection is Terraform-only" above) — any operator identity
holding `container.clusters.delete` on the project can delete `tfha-agents`
out-of-band regardless of any of this. Both are documented here as residual
risk, not omissions.

## Rotating a hub's DB password

`cloudsql-database` has a declarative rotation lever, so rotating a hub's
database password never needs an imperative `-replace` step. Set
`db_password_rotation` to a new value (e.g. a date, `"2026-09-28"`) **in the
hub's `<hub_name>.tfvars` file — not with `-var`** — and apply:

```bash
terraform -chdir=deploy/terraform/configurations/hub apply \
  -var hub_name=<hub_name> -var state_prefix=<prefix>/hubs/<hub_name> \
  -var-file=<hub_name>.tfvars
```

**Once set, keep the marker in the tfvars file permanently.** To rotate
again, change it to a new value. Never remove it or reset it to `""` —
either one takes `random_password.db`'s `keepers` back to `null`, which
triggers another, unplanned rotation (a new revision and destroyed secret
versions) on the next apply — including one that only meant to pass the
marker with `-var` and omitted it, since `-var` doesn't persist between
applies the way the tfvars file does. Always review the plan before
applying: an unexpected `random_password.db` replace means the marker went
missing or was reset.

The default `""` is a no-op — `random_password.db`'s `keepers` stay `null`,
so a plan against existing state with the default shows no diff. Changing
the value replaces the password and cascades: `google_sql_user` password
update, then the `db_password`/`db_dsn` secret versions are
create-before-destroy replaced, then `hub-cloudrun`'s Cloud Run service
picks up the new pinned DSN secret version and rolls a new revision, and
only then are the old secret versions destroyed. Expected per hub: 3 add / 2
change / 3 destroy (`random_password.db` is state-only) — review the plan's
per-resource acks before applying.

**Known window:** from the `google_sql_user` password update until the new
revision is Ready, the old revision's *new* DB connections fail, while its
existing pooled connections survive.

## Editing settings.yaml.tftpl

Any edit to `hub-cloudrun`'s `templates/settings.yaml.tftpl` — including a
comment-only one — changes the rendered `secret_data` that
`google_secret_manager_secret_version.settings` forces a replace on, which
rolls a new Cloud Run revision. There is no comment-only, no-op edit to
this file; plan and review before merging one.

## Second-plan expectations

**A clean deployment's second `plan` exits 0 — "No changes."** on every
root (`shared-infra` and each hub). That's the acceptance bar this module
set is built to. CI cannot check this directly — it never plans against a
real project — so this is a live-deployment check the operator runs after
every apply, the same way `terraform test`'s mocked plans are what CI
checks instead.

A **refresh-only note with no planned action** is benign and does not
violate that bar — it means Terraform detected drift between state and the
real resource on `refresh`, but nothing in *this* apply's config actually
changes as a result. Two you may see:

- The artifacts bucket's `lifecycle_rule` condition fields (e.g. an
  API-normalized default that was left unset in config) refreshing to
  their server-side default value.
- A project IAM member's `etag` or similar server-churn field moving.

Neither shows up as a planned add/change/destroy; only exit code 0 with
"No changes" (or a `~` refresh-only note in `-refresh-only` mode) is the
pass bar. An actual add/change/destroy on a plan you expected to be clean
is not benign — treat it as a real finding and read the diff before
applying.

## Troubleshooting

**A 403 on a secret named `scion-<h12>-...` shortly after the first
`hub` apply** means IAM propagation, not a wrong condition: the hub SA's
conditioned `secretmanager.admin` grant (hub-identity) can take longer than
the built-in 120s guard (`time_sleep.hub_iam_propagation`) to become
consistent. **Re-apply** — do not widen the IAM condition to work around it.
Widening it is exactly the mistake this whole scoping exercise exists to
prevent (see hub-identity's IAM scope rule comment).

- **An agent starts but can't reach Vertex, despite Workload Identity being
  wired** — its GCP identity is probably still Block. See "GCP identity for
  agents" above.
- **An agent's pod fails to start with an image-pull `NotFound` on
  `workspace-provision`** — the harness image isn't published to the
  registry `image_registry` points at. See "Harness images" above.
- **Agent create returns a 503 even though the agent goes on to start** —
  likely a cold Autopilot node exceeding the hub's upstream client timeout,
  not a real failure. See "Cold start" above.
- **A hub apply fails with `persistentvolumes "<hub_name>-nfs" already
  exists`** after an NFS server or share path change. This is a manual
  migration; see the agent runbook's [Operational
  Traps](../../docs/deploy/agent-runbook-terraform-ha.md#11-operational-traps)
  ("Changing a hub's NFS endpoint is a manual migration").
- **Creating a user or project secret fails with** "the Hub service account
  lacks the required Secret Manager permission. Grant
  `roles/secretmanager.admin` to the Hub Runner service account" — this
  means the hub image doesn't yet carry ptone/scion#2152's hub-prefixed
  secret names. **Do not follow the hint** in a shared project: an
  unconditioned project-wide `roles/secretmanager.admin` gives this hub
  every other hub's and co-tenant workload's secrets, including their
  signing keys. `hub-identity`'s conditioned grant already covers hub, user,
  and project scope under the hub-prefixed naming (see [What's not here
  yet](#whats-not-here-yet)); the fix is rolling a hub image that includes
  #2152.

## What's not here yet

- `shared_overrides` for hand-built (non-shared) infra to plug into the hub
  layer instead of `shared-lookup`'s naming-convention data sources.
- User- and project-scope secret creation also needs that same #2152+ hub
  image (hub-prefixed secret names) — see "Prerequisites" above for why a
  pre-#2152 image can't boot at all, let alone create these. The IAM grant
  for user/project scope already exists (`hub-identity`'s
  `hub_secretmanager_admin_hub_prefixed`); until a hub runs that image, it
  has no prefix to create user/project-scope secrets under at all.
- Typed `validation` blocks on every remaining variable, and a
  per-module README generated with `terraform-docs`.
