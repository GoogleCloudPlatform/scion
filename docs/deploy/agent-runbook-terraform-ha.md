# Agent Runbook: Multi-Hub HA Deployment (Terraform)

Deploy the multi-hub HA pattern (Cloud Run hubs + IAP, shared Cloud SQL,
Filestore, and GKE Autopilot) using the Terraform module set under
`deploy/terraform/`. This runbook is written for an AI agent to read and
execute end-to-end. Follow the sections in order.

For architecture, module interfaces, and every edge case, see
[`deploy/terraform/README.md`](../../deploy/terraform/README.md) — this
runbook is the procedure; the README is the reference. For a short
human-oriented version of this procedure, see
[terraform-ha.md](terraform-ha.md).

**This runbook mutates real cloud infrastructure and, on teardown, can
destroy it.** Every apply and destroy below has an explicit stop-and-confirm
gate. Do not skip a gate because a plan "looks fine" — the gate exists
precisely so a human, not the agent, makes that call.

---

## 0. Obtain the Repository

If you are not already in a local clone of the Scion repository, clone it:

```bash
git clone --depth 1 https://github.com/GoogleCloudPlatform/scion.git /tmp/scion-repo
cd /tmp/scion-repo
```

> **Important for AI agents:** clone the repository locally rather than
> reading this runbook or the Terraform README via a URL-fetching tool.
> Long documents can be silently truncated, and this module set has several
> pages of prerequisites and gates that must not be missed.

---

## 1. Prerequisites

| Tool | Check command | Expected output |
|------|---------------|------------------|
| Terraform | `terraform version` | `>= 1.9` (cross-variable `validation` blocks require it) |
| gcloud CLI | `gcloud --version` | Version string (any version) |
| An authenticated GCP identity | `gcloud auth list --filter=status:ACTIVE --format='value(account)'` | An email or service account |

If any check fails, stop and tell the user what is missing.

### Operator role set

The identity applying this Terraform needs Owner, or Editor plus:
`roles/compute.networkAdmin`, `roles/container.admin`, `roles/run.admin`,
`roles/resourcemanager.projectIamAdmin`, `roles/iam.serviceAccountAdmin`,
`roles/servicenetworking.networksAdmin`, `roles/storage.admin`,
`roles/iap.admin`. **Ask for all of these up front** — a partial role set
surfaces as a plan or apply failure partway through, not as a clean early
error, and re-diagnosing which specific grant is missing mid-apply wastes
much more time than asking once.

---

## 2. Questions to Ask the User

Ask these before generating any tfvars file. Detect defaults from the
ambient environment (`gcloud config get-value project`, `gcloud config
get-value compute/region`) and present them for confirmation rather than
prompting one at a time.

| # | Question | Notes |
|---|----------|-------|
| 1 | GCP project ID | Must have billing enabled; verify with `gcloud projects describe`. |
| 2 | Region (and zone) | e.g. `us-central1` / `us-central1-a`. Used by both roots. |
| 3 | Name prefix (shared layer) | e.g. `tfha`. Every shared resource name derives from this. |
| 4 | Hub name(s) | One per hub, ≤ 16 characters (service account names derived from it must fit ≤ 30 chars), lowercase, `^[a-z][a-z0-9-]*$`. |
| 5 | IAP OAuth client ID | **Not required for a first apply.** See step 6 below — this is normally discovered read-only *after* the first shared-infra apply, not gathered up front. Only ask now if the user already has one (e.g. a cross-org custom client). |
| 6 | Hub image | Full image reference (`<registry>/hub:<tag-or-digest>`), built from the same commit as the agent images. If the user hasn't built one yet, this comes after step 5 below, not before. |

**Do not** ask for a GCS state bucket name as a free-text answer without
checking it exists first — see step 3.

---

## 3. Generate tfvars

tfvars files are **gitignored** (`*.tfvars` in `.gitignore`, except the
committed `*.tfvars.example` templates) — write them to disk but never
commit them, and don't put real project IDs into anything that does get
committed.

1. Verify or create the versioned GCS state bucket:
   ```bash
   gsutil ls -b gs://<project>-<prefix>-tfstate || \
     gcloud storage buckets create gs://<project>-<prefix>-tfstate \
       --project=<project> --location=<region> --uniform-bucket-level-access
   gcloud storage buckets update gs://<project>-<prefix>-tfstate --versioning
   ```
2. Copy the example tfvars and fill in the values gathered in step 2:
   ```bash
   cp deploy/terraform/configurations/shared-infra/terraform.tfvars.example \
      deploy/terraform/configurations/shared-infra/terraform.tfvars
   cp deploy/terraform/configurations/hub/terraform.tfvars.example \
      deploy/terraform/configurations/hub/<hub_name>.tfvars
   ```
3. Leave `iap_oauth_client_id` unset (commented out) in the hub tfvars for a
   first apply — see step 6. Leave `hub_image` blank until step 5 produces
   one.
4. **Stop and tell the user** the tfvars content before running `init` —
   confirm the project, region, prefix, and hub name(s) are correct. A wrong
   `name_prefix` or `hub_name` here is expensive to unwind later (state
   prefixes and IAM conditions are derived from them).

---

## 4. Apply Shared Infra

```bash
terraform -chdir=deploy/terraform/configurations/shared-infra init \
  -backend-config="bucket=<project>-<prefix>-tfstate" \
  -backend-config="prefix=<prefix>/shared"
terraform -chdir=deploy/terraform/configurations/shared-infra plan \
  -var-file=terraform.tfvars -out=/tmp/shared.tfplan
```

### Plan-review gate (before every apply, every root)

Read the plan output before applying. **What counts as expected on a first
shared-infra apply:** all-adds, no changes, no destroys — creating the VPC,
PSA connection, Cloud Router/NAT, Cloud SQL instance, Filestore instance,
GKE Autopilot cluster, Artifact Registry repo, and enabled APIs. On a
*subsequent* plan against existing shared infra, expect **no changes at
all** unless you are deliberately changing a variable (e.g. `sql_
availability_type`).

**Any planned destroy or replace, on any resource, on any root, requires an
explicit human acknowledgment before you apply — no exceptions, including a
replace that looks purely cosmetic.** State the exact resource address and
the action (e.g. "`google_secret_manager_secret_version.settings` will be
replaced") and wait for the user's explicit go-ahead per resource before
proceeding. Do not paraphrase a `~ change` as safe on the agent's own
judgment; some changes that look like in-place updates (e.g. an IAM
condition edit) are actually forced replacements with real consequences —
see "Operational traps" below.

Once acknowledged:

```bash
terraform -chdir=deploy/terraform/configurations/shared-infra apply /tmp/shared.tfplan
```

Applying a *saved* plan file (not re-running `apply` with `-var-file`) means
what gets applied is exactly what was reviewed. This apply is dominated by
the GKE cluster create; expect roughly 10 minutes total.

**Stop and tell the user** once this completes, before building the hub
image (step 5) — the shared apply must succeed first, since the hub image
is pushed into the Artifact Registry repo this step creates.

---

## 5. Build and Push the Hub Image

Out of scope for this Terraform (see the README's "Non-Goals"). The hub and
agent images must be built from the **same commit** and pushed to the AR
repo shared-infra just created (`terraform output artifact_registry_repo_url`
on the shared-infra root). Follow the project's image pipeline for this
step; **ask the user** for the build/push command or credentials if it
isn't already scripted in this environment.

**Pin the hub image by immutable digest or an immutable tag — never
`:latest`.** See "Image rolls and rollback" below for why.

---

## 6. IAP OAuth Client

`iap_enabled = true` works immediately using the project's Google-managed
OAuth client — nothing to create before the first hub apply. Two cases:

- **In-org (common case):** after the shared-infra apply has enabled IAP,
  discover the Google-managed client ID read-only:
  ```bash
  gcloud alpha iap oauth-clients list projects/<project_number>/brands/<brand_number>
  ```
  (Treat this as a convenience only — the console, Security → Identity-Aware
  Proxy, is the durable source if this command stops working.) Set
  `iap_oauth_client_id` in the hub tfvars to this value. Until it's set,
  the hub and browser login work fine, but agent transport is disabled — a
  `check` block will warn on every plan.
- **Cross-org:** a custom OAuth client must be created in the console (the
  IAP OAuth Admin API cannot create clients). **Stop and tell the user** to
  do this manually; it cannot be scripted. Once they've set it on the
  service's IAP settings, take the client ID and set
  `iap_oauth_client_id`, then re-apply the hub root **immediately** — until
  the re-apply, IAP already expects the new audience and agent transport is
  broken (a real outage window for agent traffic, not for human browser
  login).

---

## 7. Apply a Hub

```bash
terraform -chdir=deploy/terraform/configurations/hub init \
  -backend-config="bucket=<project>-<prefix>-tfstate" \
  -backend-config="prefix=<prefix>/hubs/<hub_name>"
terraform -chdir=deploy/terraform/configurations/hub plan \
  -var hub_name=<hub_name> -var state_prefix=<prefix>/hubs/<hub_name> \
  -var-file=<hub_name>.tfvars -out=/tmp/<hub_name>.tfplan
```

`state_prefix` must equal the `-backend-config prefix` passed at `init`,
exactly. A `validation` block on `hub_name` enforces this and fails the plan
loudly if it doesn't match — **do not work around a validation failure here
by adjusting the variable to make it pass**; re-check which hub's state you
actually meant to target instead.

Apply the **plan-review gate** described in step 4 again — same rule: any
destroy or replace needs an explicit per-resource human ack. On a first hub
apply, expect an all-adds plan. Then:

```bash
terraform -chdir=deploy/terraform/configurations/hub apply /tmp/<hub_name>.tfplan
```

**A 403 on a secret named `scion-hub-<hash>-...` shortly after this apply is
IAM propagation, not a wrong condition.** Re-apply; do not widen any IAM
condition to work around it.

Repeat this whole section for each additional hub, with a new `hub_name`
and a matching backend prefix/`state_prefix`. The shared layer is untouched
by a hub apply.

---

## 8. Verify

1. **Second plan is clean.** Re-run `plan -detailed-exitcode` on every root
   touched. **Exit code 0 ("No changes") is the pass bar.** A refresh-only
   note with no planned action (a bucket lifecycle condition normalizing to
   its server-side default, an IAM member's `etag` moving) is benign. Any
   actual add/change/destroy on a plan you expected to be clean is a real
   finding — stop and investigate before declaring success.
2. **Health endpoints.** `/readyz` on the hub's Cloud Run URL should answer
   `{"status":"ready"}` — this is what gates traffic to the revision. Don't
   treat `/healthz` the same way: it always returns 200 but does unbounded
   DB/NFS work with no timeout, so it's not proof anything is actually
   healthy.
3. **Broker registration**, read-only, from the revision's own logs — do not
   infer this from the service simply responding:
   ```bash
   deploy/terraform/tools/check-broker.sh -p <project> -s <hub_name> <revision-name>
   ```
4. **A cold first agent may return a 503 and still go on to start** — a
   known limitation of a cold GKE Autopilot node (roughly 90–120s for node
   provisioning plus image pull) exceeding the hub's client timeout to the
   runtime broker. This is not a deployment failure; retry the agent-create
   check rather than treating the 503 as terminal.

---

## 9. Post-Apply: Hub Environment

Before an agent can start on a fresh hub, set two hub-scope environment
values. This is a manual, per-hub, post-apply step — Terraform has no
`local-exec` in this module set and cannot do it for you:

```bash
scion hub env set --scope hub --always GOOGLE_CLOUD_PROJECT=<project>
scion hub env set --scope hub --always GOOGLE_CLOUD_REGION=<region>
```

(`GOOGLE_CLOUD_LOCATION` is an accepted alternative to
`GOOGLE_CLOUD_REGION`.) Without this, agent create fails before it reaches
the pod, with `2 required environment variable(s) are missing:
GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_LOCATION`.

### GCP identity: Block vs Passthrough

Getting the two env values above set is **not sufficient** on its own. A
new agent's GCP identity defaults to **Block** — the metadata server is
blocked, so the agent has no reachable identity and comes up unauthenticated
to Vertex even though the module's Workload Identity binding (the
namespace's `default` KSA mapped to the `<hub>-agent` service account,
holding `roles/aiplatform.user`) is fully in place.

To let that Workload Identity binding actually reach the agent, set the
agent's **GCP Identity** field (per-agent, on create/configure) — or the
project's **Default Service Account** field, to change the default new
agents pick up — to **Passthrough**. This removes the metadata-server
interception so the agent inherits the broker's ambient GCP identity.
Passthrough only grants an identity; the model(s) an agent will call must
also be separately enabled in Vertex AI Model Garden for the project.

**Stop and tell the user** if agent identity is not something you (the
agent) have permission to configure in this environment — it's a Scion-side
setting, not a Terraform variable, so it may need to be set by a hub admin
through the UI or API.

---

## 10. Image Rolls and Rollback

- **Pin by immutable digest, or an immutable tag — never move `:latest`.**
  Moving a `:latest` tag that a running `hub_image` variable already
  references does not trigger a Terraform diff (the string value in state
  is unchanged), so the cluster silently drifts from what Terraform believes
  it deployed. Always change `hub_image` to a new, distinct value (a new
  digest or a new immutable tag) to roll.
- **Keep the previous revision.** Cloud Run keeps prior revisions unless
  something explicitly deletes them; do not delete the previous revision
  until the new one has been verified (step 8). The previous revision, plus
  its previous secret version (see the traps below on `deletion_policy`),
  is the rollback target.
- **To roll:** change `hub_image` in `<hub_name>.tfvars` to the new
  reference, plan, apply the plan-review gate, apply. Expect a single Cloud
  Run revision change with no other resource actions.
- **To roll back:** set `hub_image` back to the previous, known-good value
  and re-apply the same way. This only works if the previous revision's
  backing secret versions haven't been destroyed — see the settings-secret
  trap below.

---

## 11. Operational Traps

These are lessons from real applies against this module set, generalized.
Read this section before touching an existing (not brand-new) deployment.

- **IAM condition fields are `ForceNew`.** Editing an IAM condition's
  `title`, `description`, or `expression` — even a description-only,
  no-semantic-change edit — **replaces the grant**, which briefly removes
  the access it grants on a live hub. A plan showing an IAM member resource
  as "replace" because of a condition-field diff is not cosmetic; treat it
  with the same care as a data-plane change and get an explicit ack.
- **Any edit to the hub settings template renders a new secret version and
  rolls a new revision.** There is no comment-only, no-op edit to
  `hub-cloudrun`'s settings template — even a comment changes the rendered
  `secret_data`, which forces a replace of the settings secret version and
  rolls a new Cloud Run revision. Plan and get an ack before merging or
  applying what looks like a trivial template edit.
  - The settings secret version uses `deletion_policy = ABANDON`, so older
    versions (and the revisions that reference them) keep working after a
    replace. **A `destroy` of that resource, however, uses the *prior*
    state's deletion policy** — if the policy itself was only just changed
    to `ABANDON` in the same apply that also replaces the version, the
    ordering matters: get the policy change applied and confirmed *before*
    the template edit that triggers the replace, not in the same apply.
- **Converting Cloud SQL from `ZONAL` to `REGIONAL` restarts the shared
  instance.** It's an in-place update, not a resource replacement, but it
  triggers a real restart with an observed outage of about 7 minutes of
  `5xx`s across every hub attached to that instance. Schedule this like any
  shared-infra maintenance window; never run it against a project with
  active agents without warning the user first.
- **Connection budget.** `max_instances × per-hub connection-pool size` must
  not exceed that hub's share of the shared Cloud SQL instance's
  `max_connections`. A `check` block enforces the per-hub side of this; the
  project-wide sum across all hubs is the operator's responsibility (the
  shared instance's `max_connections` must cover it).
- **A cold Autopilot node's first agent can take 90–120s and return a 503**
  while the agent is still starting. This is a known upstream limitation
  (tracked as ptone/scion#1956), not a deployment defect — don't chase it as
  one.
- **The pass bar for "is this deployment healthy" is a second plan with
  exit code 0.** Refresh-only notes (an IAM `etag`, a bucket lifecycle
  condition normalizing to its API default) are benign and do not violate
  this. An actual planned action on a plan you expected to be clean is a
  real finding.
- **Secret Manager grants must be conditioned per hub, on hub-prefixed
  secret names — never a project-wide `secretmanager.admin` in a shared
  project.** A hub's Secret Manager reach should be scoped to its own
  hub-prefixed secrets, not the whole project's — a project-wide grant
  would let one hub read every other hub's (and any co-tenant's) signing
  keys. If a hub image's error message suggests granting broad
  `secretmanager.admin` project-wide, **do not follow that suggestion** in
  a shared project; it means the hub image predates the hub-prefixed secret
  naming and needs an upgrade instead. A legacy, wider grant made obsolete
  by a hub-prefixed migration is removed only after running the migration
  tool (see
  [`docs/deploy/migrate-names-cloudrun.md`](migrate-names-cloudrun.md) —
  landing separately; if that file doesn't exist yet in this checkout, the
  legacy grant should stay in place until it does).

---

## 12. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `403` on `scion-hub-<hash>-...` shortly after a hub's first apply | IAM condition propagation delay | Re-apply. Do not widen the condition. |
| Agent starts but can't reach Vertex despite Workload Identity being wired | GCP identity is still Block | Set Passthrough — see step 9. |
| Agent pod fails with image-pull `NotFound` on `workspace-provision` | Harness image not published to `image_registry` | Publish the image, or pick a different harness. See the README's "Harness images" section. |
| Agent create returns `503` but the agent goes on to start | Cold Autopilot node exceeding the hub's client timeout to the runtime broker | Not a failure — retry the check. See step 8. |
| Creating a user or project secret fails with a hint to grant `roles/secretmanager.admin` | Hub image predates hub-prefixed secret names | Do not follow the hint in a shared project (see "Operational traps"). Roll a hub image with hub-prefixed secret support instead. |
| Second plan on any root is not clean | Real drift, or a genuinely intended config change not yet applied everywhere | Read the diff. Don't assume it's benign; only refresh-only notes with no planned action are. |
| Hub apply fails partway through with a permission error | Operator role set was incomplete | Check the role list in step 1 was granted in full before the apply started; grant the missing role and re-apply. |

---

## 13. Teardown

**Order: every hub root first, then shared-infra — never the reverse.**
This is a multi-step, deliberately redundant sequence in the README's
"Destroy runbook" section; **read it in full and follow it exactly** before
tearing anything down; do not improvise a shortcut. In outline:

1. Empty each hub's artifacts bucket (all versions), then
   `terraform destroy` that hub's root.
2. `terraform apply -var deletion_protection=false` against `shared-infra`
   — this is expected to **fail** (by design, via `destroy_guard`) while
   any hub database still exists; that failure means step 1 wasn't
   completed for every hub.
3. On a dedicated teardown branch (never merged to `main`), remove
   `lifecycle { prevent_destroy = true }` from the three shared stateful
   modules.
4. `terraform destroy` against `shared-infra`, from that branch.
5. Compare a before/after resource inventory to confirm nothing outside the
   deployment's prefix was touched.

**Never** run `terraform destroy -var deletion_protection=false` against
`shared-infra` outside step 2 of that exact sequence, and **never** combine
`-target` with `deletion_protection` on `shared-infra` — both bypass the
guard and can leave a partial, inconsistent destroy behind. Every destroy
step above is itself a plan-review gate: get an explicit human ack before
running any of them, the same as an apply.
