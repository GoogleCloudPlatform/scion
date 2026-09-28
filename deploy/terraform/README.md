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
   `roles/storage.admin`, `roles/iap.admin`. Grant all of these up front —
   a partial role set surfaces as a plan or apply failure partway through,
   not as a clean early error.
2. A GCS state bucket, versioned: `<project>-<name_prefix>-tfstate` (e.g.
   `ptone-emblem-tfha-tfstate`). Access limited to operators.
3. ~~An IAP OAuth web client~~ — **not a prerequisite.** `iap_enabled = true`
   works immediately with the project's Google-managed OAuth client; see
   "IAP OAuth client" below. It's a post-apply step (to enable agent
   transport), not something you need before the first apply.
4. The hub image, built and pushed to the Artifact Registry repo that
   `shared-infra` creates. There is no single-apply bootstrap trick here (the
   two-root split already separates "create the repo" from "use the image"):
   apply `shared-infra` first, build/push, then apply `hub`.

## Bootstrap sequence

```bash
# 1. Shared infra, once per project.
terraform -chdir=deploy/terraform/configurations/shared-infra init \
  -backend-config="bucket=<project>-<prefix>-tfstate" \
  -backend-config="prefix=<prefix>/shared"
terraform -chdir=deploy/terraform/configurations/shared-infra apply \
  -var-file=terraform.tfvars

# 2. Build and push hub_image using the repo shared-infra just created
#    (out of scope for this Terraform — see the image pipeline).

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
including `ptone-emblem` today — so on a project like that, the first apply
can pass it right away and wait on nothing. In a genuinely fresh project it
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
apply, which is the desired behavior on a project that also runs live
Scion infra (`ptone-emblem`: `scion-hub`, `scion-hub-iap-proxy`,
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

Size Cloud SQL's `max_connections` for the sum across hubs: with
`max_open_conns = 10` per hub instance, the default `max_instances = 1`
(down from 3 — see `max_instances`'s description for the upstream defect,
`ptone/scion#2090`, this avoids) uses at most 10 connections per hub;
raising `max_instances` back toward 3 lets a hub use up to 30. Either way
the default `max_connections = 200` supports roughly 5 dev hubs with
headroom (`hub-cloudrun`'s `max_connections_budget`, default 40 per hub,
checks this per hub).

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

1. For each hub: empty its artifacts bucket, **noncurrent versions
   included** — the bucket is versioned and deliberately has no
   `force_destroy`, so `destroy` fails otherwise:
   ```bash
   gcloud storage rm -r --all-versions gs://<project>-<hub>-artifacts/**
   ```
   Then `terraform -chdir=configurations/hub destroy` (with that hub's
   backend prefix and tfvars). This removes only that hub's resources; it
   never touches shared infra, because the hub root only reads shared infra
   via data sources.
2. `terraform -chdir=configurations/shared-infra apply -var
   deletion_protection=false ...`. The `terraform_data.destroy_guard`
   precondition makes this specific apply **fail** while any hub database
   still exists on the shared Cloud SQL instance — every hub creates exactly
   one, so this is the proxy for "a hub still exists".
3. On a teardown branch (never merged to `main`), commit removing
   `lifecycle { prevent_destroy = true }` from the three shared stateful
   modules (`cloudsql-instance`, `filestore`, `gke-autopilot`). This is a
   literal in each module, not a variable — Terraform doesn't allow a
   variable-driven `prevent_destroy` — so intentional teardown costs a
   one-line commit per module. That friction is intended for infra every
   hub depends on.
4. `terraform -chdir=configurations/shared-infra destroy`, from that branch.
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

## Troubleshooting

**A 403 on a secret named `scion-hub-<h12>-...` shortly after the first
`hub` apply** means IAM propagation, not a wrong condition: the hub SA's
conditioned `secretmanager.admin` grant (hub-identity) can take longer than
the built-in 120s guard (`time_sleep.hub_iam_propagation`) to become
consistent. **Re-apply** — do not widen the IAM condition to work around it.
Widening it is exactly the mistake this whole scoping exercise exists to
prevent (see hub-identity's IAM scope rule comment).

## What's not here yet

- `shared_overrides` for hand-built (non-shared) infra to plug into the hub
  layer instead of `shared-lookup`'s naming-convention data sources.
- User- and project-scope secrets have no per-hub prefix to condition an
  IAM grant on (only the hub-scope prefix does — see `hub-identity`'s IAM
  scope rule), so creating one under the current IAM fails with a 403.
  Resolving this is a future seams item.
- Typed `validation` blocks on every remaining variable, and a
  per-module README generated with `terraform-docs`.
