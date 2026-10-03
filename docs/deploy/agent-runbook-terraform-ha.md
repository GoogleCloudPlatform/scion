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
cd /tmp/scion-repo || exit 1
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
| `gcloud alpha` component | `gcloud components list --filter='id:alpha' --format='value(state.name)'` | `Installed`; used for step 6's IAP client discovery as a convenience only — **do not stop on this check failing.** If it's missing, fall back to the console (Security → Identity-Aware Proxy) for that step. |
| `gcloud storage` | `gcloud storage --help` | Use `gcloud storage` throughout this runbook; do not mix in `gsutil` on the same bucket. |
| `python3` | `python3 --version` | Needed by `check-broker.sh` (step 8). |
| `git` | `git --version` | Needed for step 0 and the teardown branch in §13. |
| An authenticated GCP identity | `gcloud auth list --filter=status:ACTIVE --format='value(account)'` | An email or service account |
| The `scion` CLI, authenticated to the new hub | `scion hub status` | Only available after step 7's first hub apply — see step 9's gate below. |

If any check fails, stop and tell the user what is missing — except the
`gcloud alpha` component, which is a convenience only (see its row above).

### Operator role set

The identity applying this Terraform needs Owner, or Editor plus:
`roles/compute.networkAdmin`, `roles/container.admin`, `roles/run.admin`,
`roles/resourcemanager.projectIamAdmin`, `roles/iam.serviceAccountAdmin`,
`roles/servicenetworking.networksAdmin`, `roles/storage.admin`,
`roles/iap.admin`, `roles/secretmanager.admin`. The last one is needed for
the five `google_secret_manager_secret_iam_member` resources across
`cloudsql-database` and `hub-cloudrun` — none of the other roles above
grant `secretmanager.secrets.setIamPolicy` (`roles/editor` has no
`setIamPolicy` permissions at all, and `resourcemanager.projectIamAdmin`
only covers project-level policy, not resource-level secret policy); a
narrower custom role granting just `secretmanager.secrets.setIamPolicy`
also works. This is a grant on the *operator* identity applying Terraform —
it is unrelated to, and not a relaxation of, §11's rule against a
project-wide `secretmanager.admin` on a *hub's* runtime service account.
**Ask for all of these up front** — a partial role set surfaces as a plan
or apply failure partway through, not as a clean early error, and
re-diagnosing which specific grant is missing mid-apply wastes much more
time than asking once.

Verify the role set is actually granted before proceeding — don't take the
user's word for it:

```bash
gcloud projects get-iam-policy <project> --flatten=bindings \
  --filter="bindings.members:<account>" --format='value(bindings.role)'
```

Compare the output against the list above. `Owner` covers the whole list.
`Editor` does **not** — `roles/editor` carries no `setIamPolicy`
permissions, so it covers neither `roles/resourcemanager.projectIamAdmin`
(the module makes many `google_project_iam_member` grants) nor
`roles/iam.serviceAccountAdmin`'s IAM-policy permissions, in addition to
never covering `roles/iap.admin`. With `Editor`, every role listed above
must also appear explicitly in the `get-iam-policy` output. **Stop and ask**
if anything is missing, rather than discovering it as a partial-apply
failure later.

This command only shows grants made directly to `<account>` at the project
level — it does **not** show grants that come through a group membership,
or inherited from a folder or organization policy. A role missing from its
output is not proof the identity lacks it; if in doubt, stop and ask rather
than asserting the role is absent.

---

## 2. Questions to Ask the User

Ask these before generating any tfvars file. Detect defaults from the
ambient environment (`gcloud config get-value project`, `gcloud config
get-value compute/region`) and present them for confirmation rather than
prompting one at a time.

| # | Question | Notes |
|---|----------|-------|
| 1 | GCP project ID | Must have billing enabled; verify with `gcloud billing projects describe <project> --format='value(billingEnabled)'` (`gcloud projects describe` does not show billing status). |
| 2 | Region (and zone) | e.g. `us-central1` / `us-central1-a`. Used by both roots. |
| 3 | Name prefix (shared layer) | e.g. `tfha`. 2–8 chars, lowercase letters/digits, no hyphens (`^[a-z][a-z0-9]{1,7}$`, `configurations/shared-infra/variables.tf`'s `name_prefix` validation). Every shared resource name derives from this. |
| 4 | Hub name(s) | One per hub. Must be `<name_prefix>-<suffix>`, 3–16 chars total, `^[a-z][a-z0-9-]{2,15}$` (`configurations/hub/variables.tf`'s `hub_name` validation), e.g. `tfha-h1`. |
| 5 | IAP members (users/groups) | `iap_members`, default `[]`. Granted `roles/iap.httpsResourceAccessor` on this hub's Cloud Run service only — it is the only *human* grant of that role on the service (the transport service account has its own, separate grant). Leaving it empty means nobody can use the hub, and step 9 (a hub admin logging in) cannot happen. Ask for at least one user or group. |
| 6 | Hub admin email(s) | `admin_emails`, default `[]`. Seeded as hub admins on first boot. Leaving it empty means no hub admin exists once the hub is up, and step 9's `scion hub env set --scope hub` work needs an authenticated hub admin. |
| 7 | SQL availability (shared layer) | `sql_availability_type`, module default `REGIONAL` (automatic Cloud SQL failover) — **recommend `REGIONAL` for production.** `shared-infra/terraform.tfvars.example` sets `ZONAL`; that file is an example value for a smaller/cheaper dev footprint, not a recommendation. Converting `ZONAL` → `REGIONAL` later is in place but restarts the shared instance (~7 minute outage across every hub attached to it) — see §11. |
| 8 | IAP OAuth client ID | **Not required for a first apply.** See step 6 below — this is normally discovered read-only *after* the first shared-infra apply, not gathered up front. Only ask now if the user already has one (e.g. a cross-org custom client). |
| 9 | Hub image | Full image reference (`<registry>/hub:<tag-or-digest>`), built from the same commit as the agent images. If the user hasn't built one yet, this comes after step 5 below, not before. |
| 10 | `max_instances` (per hub) | `hub/terraform.tfvars.example` sets `3`. That value is only safe on a hub image built from a commit containing `GoogleCloudPlatform/scion#2046`; on an older hub image it must be `1` (README "Scaling") — otherwise an affinity-owner scale-in can leave providers offline ("Default runtime broker is unavailable"). Ask the user whether the hub image in question 9 contains `#2046`; if unsure, use `1`. |

**Do not** ask for a GCS state bucket name as a free-text answer without
checking it exists first — see step 3.

---

## 3. Generate tfvars

tfvars files are **gitignored** (`*.tfvars` in `.gitignore`, except the
committed `*.tfvars.example` templates) — write them to disk but never
commit them, and don't put real project IDs into anything that does get
committed.

1. Check whether the versioned GCS state bucket already exists, using
   `gcloud storage` only (do not mix in `gsutil`):
   ```bash
   gcloud storage buckets describe gs://<project>-<prefix>-tfstate \
     --format='value(versioning_enabled)'
   ```
   - If the bucket does **not exist**, **stop and ask** the user before
     creating it:
     ```bash
     gcloud storage buckets create gs://<project>-<prefix>-tfstate \
       --project=<project> --location=<region> --uniform-bucket-level-access
     gcloud storage buckets update gs://<project>-<prefix>-tfstate --versioning
     ```
   - If the bucket **already exists** and versioning is off, **stop and
     ask** before enabling it — it may belong to the user and already hold
     state you shouldn't alter unasked:
     ```bash
     gcloud storage buckets update gs://<project>-<prefix>-tfstate --versioning
     ```
   - If it already exists with versioning on, do nothing further here.
2. Copy the example tfvars and fill in every value below (gathered in step
   2), per file:
   ```bash
   cp deploy/terraform/configurations/shared-infra/terraform.tfvars.example \
      deploy/terraform/configurations/shared-infra/terraform.tfvars
   cp deploy/terraform/configurations/hub/terraform.tfvars.example \
      deploy/terraform/configurations/hub/<hub_name>.tfvars
   ```
   - **shared** (`shared-infra/terraform.tfvars`): `project_id`, `region`,
     `zone`, `name_prefix`, `sql_availability_type` (recommend `REGIONAL`;
     the example file's `ZONAL` is an example value, not a recommendation —
     see question 7), `deletion_protection = true`.
   - **hub** (`hub/<hub_name>.tfvars`): `project_id`, `region`, `zone` (must
     match the shared tfvars exactly), `shared_prefix` (must equal
     `name_prefix` above), `hub_name`, `state_prefix` (=
     `<name_prefix>/hubs/<hub_name>`), `hub_image`, `iap_members`,
     `admin_emails`, `max_instances` (`3` only if `hub_image` is built from a
     commit containing `GoogleCloudPlatform/scion#2046`, otherwise `1` — ask
     the user if unsure; see question 10).
3. Leave `iap_oauth_client_id` unset (commented out) in the hub tfvars for a
   first apply — see step 6. Leave `hub_image` blank until step 5 produces
   one.
4. **Stop and tell the user** the tfvars content before running `init` —
   confirm the project, region, prefix, hub name(s), `iap_members`,
   `admin_emails`, `sql_availability_type`, and `max_instances` are correct.
   A wrong `name_prefix` or `hub_name` here is expensive to unwind later
   (state prefixes and IAM conditions are derived from them); an empty
   `iap_members` or `admin_emails` deploys a hub nobody can reach or
   administer; `max_instances = 3` on a pre-`#2046` hub image can leave
   providers offline under scale-in.

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

This gate applies **every single time you are about to run `apply`**, on
either root, with no exceptions — including a "re-apply" mentioned anywhere
else in this runbook (the 403 in step 7, the cross-org re-apply in step 6,
image rolls in §10). **"Re-apply" always means: re-run `plan -out` into a
new plan file, then go through this gate again. Never run a bare `apply`
without a freshly reviewed plan file.**

1. Run `plan -out=<file>` (as shown above).
2. Report the add/change/destroy counts, and **every resource address with a
   non-create action** (change or destroy), to the user.
3. Report **every** `Warning: Check block assertion failed` in the plan
   output. The only one expected on an ordinary plan is
   `transport_audience_configured` while `iap_oauth_client_id` is still
   null (see step 6). Treat any other check warning — including
   `connection_budget` (see "Operational traps") — as a stop: it means
   something in the plan needs the user's attention even though Terraform
   will otherwise proceed.
4. **What counts as expected on a first shared-infra apply:** all-adds, no
   changes, no destroys — creating the VPC, PSA connection, Cloud
   Router/NAT, Cloud SQL instance, Filestore instance, GKE Autopilot
   cluster, Artifact Registry repo, and enabled APIs. On a *subsequent*
   plan against existing shared infra, expect **no changes at all** unless
   you are deliberately changing a variable (e.g.
   `sql_availability_type`).
5. **Wait for an explicit go-ahead before every apply — no exceptions,
   including a replace that looks purely cosmetic.** State the exact
   resource address and the action (e.g.
   "`google_secret_manager_secret_version.settings` will be replaced") and
   get a per-resource ack for **each** destroy or replace before
   proceeding. Do not paraphrase a `~ change` as safe on the agent's own
   judgment; some changes that look like in-place updates (e.g. an IAM
   condition edit) are actually forced replacements with real consequences
   — see "Operational traps" below.
6. Apply only the plan file just reviewed and acknowledged — never a bare
   `apply -var-file=...`. If the apply fails or is only partially applied,
   the saved plan file is now stale (Terraform will refuse to apply it
   again); go back to step 1 and re-plan into a new file rather than
   retrying the old one.

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

## 5. Build and Push Images

Out of scope for this Terraform (see the README's "Non-Goals"). The hub and
agent images must be built from the **same commit** and pushed to the AR
repo shared-infra just created:

```bash
terraform -chdir=deploy/terraform/configurations/shared-infra output \
  -raw artifact_registry_repo_url
```

**Hub image.** Its source is `scripts/cloudrun/Dockerfile` — **not**
`image-build/hub/Dockerfile`. The latter builds the GKE-oriented `scion-hub`
image produced by `image-build/scripts/build-images.sh --target hub` (and
pulled in by `--target common`/`--target all`); it runs as root and is the
wrong hub image for this Cloud Run pattern.

**Build it for `linux/amd64` explicitly.** `scripts/cloudrun/Dockerfile`
cross-compiles the Go binary with `GOARCH=amd64` (Stage 2), but its base
images (`node:20-slim`, `golang:1.26`, `debian:bookworm-slim`) are all
unpinned, multi-arch images — a plain `docker build` picks whatever
architecture the build host is. On an arm64 build host (Apple Silicon, for
example — the same case "Why Cloud Build only" cites below for harness
images), that produces an arm64 runtime layer wrapping an amd64-only
binary, which Cloud Run rejects at hub apply. Use:
```bash
docker buildx build --platform linux/amd64 \
  -f scripts/cloudrun/Dockerfile \
  -t <registry>/hub:<immutable-tag> --push .
```
or whatever build tooling the user already has, **as long as it passes
`linux/amd64`** — this is not a request to add a second documented build
path. **Ask the user** for the actual build/push command or credentials if
this isn't already scripted in this environment.

**Do not run `scripts/cloudrun/deploy.sh`** to produce this image. It is
the only scripted build of `scripts/cloudrun/Dockerfile` in this repo, but
it has no build-only mode (its one flag is `--skip-build`): running it
creates its own service accounts, secrets, and IAM bindings and stands up a
**separate, non-Terraform Cloud Run hub** — it does not just build an image
for this Terraform-managed hub. It also pushes
`hub:${SCION_IMAGE_TAG:-latest}`, i.e. `hub:latest` by default, which is
exactly the implicit `:latest` move this runbook forbids. Its project
defaults to `${SCION_PROJECT:-deploy-demo-test}`, which inside a Scion
agent container is typically set to the *Scion* project, not necessarily
`<project>` — another reason not to treat it as "already scripted" for this
flow.

Push the hub image under a **new immutable tag or digest** (never
`:latest`) and set `hub_image` to that reference — this never moves a tag
anything else resolves against, so it needs no `:latest` ack. Apply the
same kind of tag pre-check used for the harness images below to
`<registry>/hub` before pushing:
```bash
gcloud artifacts docker tags list <registry>/hub \
  --filter='tag:<immutable-tag>'
```

**Agent harness images — Cloud Build only.** This runbook documents a
single supported build path: `--builder cloud-build`, one stage at a time,
all three stages under the same immutable tag. Do not use `--builder
local-docker`/`local-podman`, and do not build `--target all` or hybrid
(local base + Cloud Build harnesses) — those paths are out of scope for
this runbook (see "Why Cloud Build only" below).

Each harness image builds `FROM <registry>/scion-base:<tag>`, and
`scion-base` in turn builds `FROM <registry>/core-base:<tag>`
(`cloudbuild-scion-base.yaml`'s `BASE_IMAGE=$_REGISTRY/core-base:$_TAG`
build-arg, and `cloudbuild-harnesses.yaml`'s
`BASE_IMAGE=$_REGISTRY/scion-base:$_TAG` build-arg with `--pull` on every
step). `--target harnesses` only builds the harnesses themselves, and
`--target scion-base` only builds `scion-base` — neither builds its parent
— so the chain must be built **in order, under the same tag**, one stage at
a time.

**Before asking for the build ack, show the user the exact list of images
and tags the three stages will push.** `--builder cloud-build`'s
`--dry-run` only prints the `gcloud builds submit` command and its
`_TAG`/`_REGISTRY` substitutions — unlike the per-image builders, it prints
no `BASE_IMAGE=` line and no per-image tag list, because the whole target is
handed off to a static YAML. Read the pushed tags from that YAML instead
(image-build's README "Cloud Build Configs" table names the file per
target):
- `core-base` → `cloudbuild-core-base.yaml` pushes
  `core-base:<immutable-tag>` and `core-base:<short-sha>`.
- `scion-base` → `cloudbuild-scion-base.yaml` pushes
  `scion-base:<immutable-tag>` and `scion-base:<short-sha>`.
- `harnesses` → `cloudbuild-harnesses.yaml` pushes
  `scion-<harness>:<immutable-tag>` and `scion-<harness>:<short-sha>` for
  each harness **it** hardcodes — currently 8, **not** the 9-image catalog
  (it omits `scion-muse-code`; this is a pre-existing drift between the
  static YAML and the harness catalog, out of scope for this runbook to
  fix). Read the file itself for the current list.

`<short-sha>` is the `(+ :<sha>)` value in the stage's `--dry-run` header
(`Tag:      <immutable-tag> (+ :f6642c16)`), i.e. `git rev-parse --short
HEAD` in this clone. Get it by running the target with `--dry-run` and the
same `GCLOUD_PROJECT=<project>` prefix as the real command, before asking
for the ack.

"Immutable" is a naming convention here, not something the tool enforces —
`--tag <immutable-tag>` will happily overwrite an existing tag of the same
name, and the auto-added `:<short-sha>` can already exist too if another
operator already built the same commit. Either re-points an existing tag,
which is exactly what §10 requires an ack for. **Pre-check before asking,
for every image in the push list above (`core-base`, `scion-base`, and each
harness), not harnesses alone, checking both the immutable tag and the
auto-added short-sha tag:**
```bash
gcloud artifacts docker tags list <registry>/<image> \
  --filter='tag:<immutable-tag> OR tag:<short-sha>'
```
An empty result means both tags are new; any output means this push would
re-point one of them.

Then **ask the user**, and make the question state explicitly whether
`:latest` will move **and for which image families** (harnesses,
`core-base`, `scion-base` — the ack must cover each one the user is being
asked about, since harnesses are what every hub actually resolves while
`core-base`/`scion-base` `:latest` are only the defaults other build
configs fall back to), e.g.: *"This pushes `core-base:<immutable-tag>` plus
`core-base:<short-sha>`, `scion-base:<immutable-tag>` plus
`scion-base:<short-sha>` (each new, or re-pointed if already present — see
the pre-check above), and `scion-<harness>:<immutable-tag>` plus
`:<short-sha>` (new, or re-pointed) for [list of harnesses from
`cloudbuild-harnesses.yaml`] to `<registry>`. It will **not** move any of
their `:latest` tags. Do you also want `:latest` moved for any of these —
and if so, which ones?"* This applies equally to a first deployment into an
empty registry — pushing the *initial* `:latest` establishes the tag every
future hub on this registry inherits, so it is the same explicit ack, not
something to do implicitly as part of "build and push."

**Do not run any stage command below until the push list, pre-check and
build ack above are done.**

```bash
GCLOUD_PROJECT=<project> image-build/scripts/build-images.sh --builder cloud-build \
  --target core-base --registry <registry> --tag <immutable-tag> \
  2>&1 | tee /tmp/stage-core-base.log
```
Wait for it to reach `SUCCESS` (see "Every submit is asynchronous" below),
then:
```bash
GCLOUD_PROJECT=<project> image-build/scripts/build-images.sh --builder cloud-build \
  --target scion-base --registry <registry> --tag <immutable-tag> \
  2>&1 | tee /tmp/stage-scion-base.log
```
Wait for `SUCCESS`, then:
```bash
GCLOUD_PROJECT=<project> image-build/scripts/build-images.sh --builder cloud-build \
  --target harnesses --registry <registry> --tag <immutable-tag> \
  2>&1 | tee /tmp/stage-harnesses.log
```
Wait for `SUCCESS` before moving on to step 6/7. **Set `GCLOUD_PROJECT=<project>`
explicitly on every one of the three commands above** — see below for why.
(`--target` maps to
`cloudbuild-core-base.yaml`, `cloudbuild-scion-base.yaml`, and
`cloudbuild-harnesses.yaml` respectively — image-build's README "Cloud
Build Configs" table. `--push` and `--platform` are both ignored by
`--builder cloud-build`: the YAMLs always push, and they hardcode
`--platform linux/amd64,linux/arm64` on every `docker buildx build` step,
so neither flag has anything left to control.)

**`GCLOUD_PROJECT` must be set explicitly, on every stage invocation.**
`builder_run_target` (`image-build/scripts/builders/cloud-build.sh`)
resolves the Cloud Build project as `$GCLOUD_PROJECT`, then `gcloud config
get-value project`, and only parses a project out of `<registry>` when
*both* of those are empty. None of the three commands above sets it any
other way, so on an agent's shell the build can silently run in whatever
project the ambient `gcloud config` names — not `<project>` — and get
billed and executed there instead. If a command's output contains
`Warning: Cloud Build project '<p>' differs from registry project
'<reg>'`, **stop**: the build service account in `<p>` usually can't push
to `<registry>`, and stage 1's `verify-registry` step will fail. This is
why every command above is prefixed with `GCLOUD_PROJECT=<project>` — never
drop it.

**The Cloud Build service account needs push access on the shared
registry.** Don't assert which service account Cloud Build runs as — look
it up, since it depends on the project's age:
```bash
gcloud builds get-default-service-account --project=<project>
```
This prints the service account email directly. On projects that predate
the 2024 Cloud Build default-service-account change it's the legacy
`<project_number>@cloudbuild.gserviceaccount.com`; on newer projects it's
`<project_number>-compute@developer.gserviceaccount.com` (the Compute Engine
default SA) instead. Grant `roles/artifactregistry.writer` on
`<name_prefix>-scion` (the repo `modules/artifact-registry/main.tf` creates)
to *whichever account the lookup above printed* —
`cloudbuild.googleapis.com` itself is already enabled by
`project-services`, so nothing else is needed there. On newer projects, or
under an org policy that disables automatic default-SA grants, this grant
is often missing. Stage 1's `verify-registry` step
(`image-build/scripts/verify-registry.sh`) fails fast in that case — that's
safe. If it fails, **stop and ask the user** to grant push access; do not
grant it yourself.

If you point the user at `image-build/scripts/setup-cloud-build.sh` as a
convenience, give it every flag explicitly: `--project <project> --location
<region> --repo <name_prefix>-scion` (`verify-registry.sh` itself prints
these exact values on failure). **Never suggest running it bare:** with no
arguments it falls back to `--repo scion --location us-central1` in
whatever project the ambient `gcloud config` names, and it **creates** an
Artifact Registry repo there — a stray repo outside Terraform, not the one
this module set uses. It also only grants the legacy
`<project_number>@cloudbuild.gserviceaccount.com`, not necessarily the
account `get-default-service-account` named above, and it separately
enables the Cloud Build and Artifact Registry APIs as a side effect. The
agent still does not run this script itself.

**Every submit is asynchronous.** `--builder cloud-build` always runs
`gcloud builds submit --async` (`builders/cloud-build.sh`'s
`builder_run_target`), so each `build-images.sh` command above returns
"Build submitted" immediately — before the image exists. Each command is
piped through `tee` into its own log file so the build ID can be read from
the submit's own output.

**Capture `BUILD_ID` from that output — never from `builds list
--ongoing`.** `gcloud builds submit --async` always prints `Created
[https://cloudbuild.googleapis.com/v1/projects/<p>/locations/<region>/builds/<ID>].`
before returning — confirmed against the installed `gcloud`'s
`submit_util.Build`, which calls `log.CreatedResource` unconditionally,
including under `--async`; that status line goes to stderr, which is why
the commands above redirect it into the log with `2>&1`. Parse the ID out
of the log instead of guessing at it afterwards:
```bash
BUILD_ID=$(grep -oE 'builds/[0-9a-f-]{36}' /tmp/stage-core-base.log | head -1 | cut -d/ -f2)
[[ -n "${BUILD_ID}" ]] || { echo "No build ID found in the stage's output. STOP." >&2; exit 1; }
```
(Use the matching `/tmp/stage-<target>.log` for each of the three stages.)
Do **not** use `gcloud builds list --project=<project> --ongoing
--sort-by=~createTime --limit=1` for this: it returns the newest in-flight
build in the whole project, not necessarily the one you just submitted —
another operator's build or a trigger build in the same project can be
picked up instead, and a build that already failed fast (`verify-registry`
fails in seconds) is no longer `--ongoing` at all, so the command silently
names some *other* build, or none, and a foreign build reaching `SUCCESS`
can then pass the gate for a stage that actually failed. If you want a
`builds list` cross-check at all, filter it on this run's own tag instead
of `--ongoing`: `--filter='substitutions._TAG="<immutable-tag>"'`.

Then poll in a bounded loop whose window is **derived from that stage's own
Cloud Build `timeout:` value** — not one fixed number shared by all three.
Read verbatim from the YAMLs (verified against the files in this checkout):
- `cloudbuild-core-base.yaml`: `timeout: 10800s` (3h)
- `cloudbuild-scion-base.yaml`: `timeout: 1800s` (30min)
- `cloudbuild-harnesses.yaml`: `timeout: 2400s` (40min)

Set `MAX_WAIT` to that stage's timeout plus a margin (600s below) before
running the loop:
```bash
# core-base: MAX_WAIT=11400 ; scion-base: MAX_WAIT=2400 ; harnesses: MAX_WAIT=3000
MAX_WAIT=<stage timeout + 600>
STATUS=""
for i in $(seq 1 $((MAX_WAIT / 30))); do
  STATUS=$(gcloud builds describe "${BUILD_ID}" --project=<project> \
    --format='value(status)')
  case "${STATUS}" in
    SUCCESS) break ;;
    FAILURE|INTERNAL_ERROR|TIMEOUT|CANCELLED)
      echo "Build ${BUILD_ID} ended in ${STATUS}. STOP." >&2; exit 1 ;;
  esac
  sleep 30
done
[[ "${STATUS}" == "SUCCESS" ]] || { echo "Build ${BUILD_ID} did not reach SUCCESS within ${MAX_WAIT}s. STOP. Do not resubmit the stage while it may still be QUEUED or WORKING -- check with a single gcloud builds describe call first." >&2; exit 1; }
```
If your shell or harness enforces a per-command time limit shorter than
`MAX_WAIT` (up to 3h10m for `core-base`), do not run this loop as one
foreground command that would be killed mid-wait: run it in the
background, or poll with single `gcloud builds describe` calls spaced
across separate tool invocations, and do not re-submit the stage while it
may still be `QUEUED`/`WORKING` — a concurrent second submit pushes the
same tags from a second build.

On `FAILURE`, `TIMEOUT`, `CANCELLED` or `INTERNAL_ERROR` at any stage,
**stop** — do not proceed to the next stage, and do not "fix" it by
dropping `--tag`. The wait-for-`SUCCESS` gate after `core-base` and after
`scion-base` is also the existence check for the next stage's base image:
if the previous stage didn't reach `SUCCESS`, its image was never pushed,
and the next stage will fail on its first step.

Both `build-images.sh --tag` and every `image-build/cloudbuild-*.yaml`'s
`_TAG` substitution **default to `latest`.** Agent harness images are
resolved as `<image_registry>/scion-<harness>:latest` by *every* hub on this
shared infra (each `harnesses/<h>/config.yaml` pins `image:
scion-<h>:latest`; README "Harness images"). There is no per-hub or
per-harness image pin in this module set today (upstream settings-based
agent image pinning exists as `ptone/scion#2156`, but this module set does
not expose it yet). **Running any of the three commands above without
`--tag` therefore moves `:latest` in the shared registry implicitly — never
do this, and never "fix" a failed build by dropping `--tag`** to fall back
to it — that's the same implicit move by another route. Always pass the
same explicit immutable tag to all three stages.

**Why Cloud Build only.** `--builder cloud-build`'s `docker buildx build`
steps always build `linux/amd64,linux/arm64` — a multi-arch manifest,
built server-side regardless of the operator's own machine (image-build's
README "Builders" table: cloud-build is "always amd64+arm64
(server-side)") — so the amd64 variant GKE Autopilot's nodes need is always
produced. None of the commands above pass `--platform`; with
`--builder local-docker`/`local-podman` that means the builder's *native*
architecture, i.e. the operator's own host. An operator on an arm64 host
(Apple Silicon, for example) running a local build would push arm64-only
images, and every agent create would then fail on Autopilot's amd64 nodes
with an exec-format error. Local and arm64 builds are out of scope for this
runbook for that reason. A local-base/Cloud-Build-harnesses hybrid has the
same failure mode (a single-arch local base under a multi-arch
`cloudbuild-harnesses.yaml` harness build) and is likewise not documented
here.

If the user declines moving `:latest`, the consequence differs by case —
state the one that applies:

- **Existing shared registry (there is a current `:latest`):** every hub on
  this shared infra keeps pulling that **current** `:latest` for new
  agents, so the freshly built agent harness images and the `hub_image`
  you're about to set are no longer at the same commit — contradicting
  question 9's "same commit" requirement — until `:latest` is moved later.
  Get an explicit acknowledgment of that skew before proceeding; do not
  move `:latest` yourself later without asking again.
- **Empty registry (first deployment, no current `:latest`):** there is
  nothing for a decline to "keep" — no `scion-<harness>:latest` exists at
  all. Every agent create fails with an image-pull `NotFound` (§12
  Troubleshooting) until `:latest` is pushed, so step 8.4 (the first agent)
  cannot pass. This is a **stop-and-ask gate, not a skew to acknowledge**:
  either the user acks the initial `:latest` push now, or tell them plainly
  that no agent can start on this shared infra, and **stop before the hub
  apply** (step 7) rather than proceeding to a hub that can't run agents.

**If the user acks moving `:latest`** (for either case above), the only
sanctioned way to do it is to **re-tag the already-built immutable image**
— never rebuild with `--tag latest`. A rebuild produces a different digest
from the one the user just approved, re-points the `:<short-sha>` tags the
approved build already pushed minutes earlier, and (for `harnesses`) costs
up to another 40 minutes. After all three stages have reached `SUCCESS`
above, for **each image the user named in the ack**:
```bash
gcloud artifacts docker tags add \
  <registry>/<image>:<immutable-tag> <registry>/<image>:latest
```
Then verify the retag landed on the `:<immutable-tag>` digest — this proves
the `tags add` call took effect, not that the digest is unchanged since the
ack (`tags add` always resolves `:<immutable-tag>` at retag time, and both
calls below read the current state afterwards, so this cannot catch a
digest that changed underneath you between the ack and the retag):
```bash
gcloud artifacts docker images describe <registry>/<image>:latest \
  --format='value(image_summary.digest)'
gcloud artifacts docker images describe <registry>/<image>:<immutable-tag> \
  --format='value(image_summary.digest)'
```
The two digests must be equal — if they aren't, the retag didn't take and
`:latest` still points elsewhere; stop and ask again before proceeding. Do
this once per image the user named; no rebuild, and never drop `--tag` from
a stage command as a way to "move" `:latest` instead.

See "Image Rolls and Rollback" (§10) for how a `hub_image` roll differs
from moving `:latest`.

---

## 6. IAP OAuth Client

`iap_enabled = true` works immediately using the project's Google-managed
OAuth client — nothing to create before the first hub apply. Two cases:

- **In-org (common case):** after the shared-infra apply has enabled IAP,
  discover the Google-managed client ID read-only. You need the project
  number and the IAP brand number first:
  ```bash
  terraform -chdir=deploy/terraform/configurations/shared-infra output \
    -raw project_number
  gcloud alpha iap oauth-brands list --project=<project>
  gcloud alpha iap oauth-clients list projects/<project_number>/brands/<brand_number>
  ```
  (Treat the `oauth-clients list` command as a convenience only — the
  console, Security → Identity-Aware Proxy, is the durable source if this
  command stops working.) Set `iap_oauth_client_id` in the hub tfvars to
  this value. Until it's set, the hub and browser login work fine, but
  agent transport is disabled — a `check` block will warn on every plan.
  **If no client is listed yet** (a genuinely fresh project: the
  Google-managed client only appears once IAP has actually been turned on
  by a hub's Cloud Run service, not merely by shared-infra enabling the
  API) — leave `iap_oauth_client_id` null, do the first hub apply (step 7)
  to turn IAP on, then come back here to discover the client ID and
  re-apply the hub root through the plan-review gate.
  Setting `iap_oauth_client_id` replaces the settings secret version and
  rolls a new revision — that's expected, and it still needs the replace
  ack from the plan-review gate.
- **Cross-org:** a custom OAuth client must be created in the console (the
  IAP OAuth Admin API cannot create clients). **Stop and tell the user** to
  do this manually; it cannot be scripted. Once they've set it on the
  service's IAP settings, take the client ID and set
  `iap_oauth_client_id`, then re-apply the hub root **immediately, after
  the plan-review gate** — don't let the urgency below skip the gate. Until
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

**A 403 on a secret named `scion-<hash>-...` shortly after this apply is
IAM propagation, not a wrong condition.** Re-apply — meaning re-plan into a
new plan file and go through the plan-review gate again — do not widen any
IAM condition to work around it.

Repeat this whole section for each additional hub, with a new `hub_name`
and a matching backend prefix/`state_prefix`. The shared layer is untouched
by a hub apply.

---

## 8. Verify

1. **Second plan is clean.** Re-run `plan` on every root touched, with the
   same arguments as the apply plus `-detailed-exitcode` (for the hub root,
   that means the same `-var hub_name=... -var state_prefix=... -var-file=...`):
   ```bash
   terraform -chdir=deploy/terraform/configurations/hub plan \
     -var hub_name=<hub_name> -var state_prefix=<prefix>/hubs/<hub_name> \
     -var-file=<hub_name>.tfvars -detailed-exitcode
   ```
   **Expected: exit code 0 ("No changes")** — that is the pass bar. A
   refresh-only note with no planned action (a bucket lifecycle condition
   normalizing to its server-side default, an IAM member's `etag` moving)
   is benign. Any actual add/change/destroy on a plan you expected to be
   clean is a real finding — stop and investigate before declaring success.
2. **Health endpoints**, without an IAP-authenticated `curl` (the service
   is behind IAP, so an unauthenticated request only gets a redirect or
   401 — no useful signal from `/readyz` directly):
   ```bash
   terraform -chdir=deploy/terraform/configurations/hub output -raw service_uri
   gcloud run services describe <hub_name> --region <region> --project <project> \
     --format='value(status.latestReadyRevisionName,status.conditions)'
   ```
   **Expected:** a `latestReadyRevisionName` is present. A Ready revision
   means the `/readyz` startup probe already passed for it — a sound proxy
   for "the hub is up" with no IAP token required. Note the revision name
   printed here; it's `<revision-name>` for the next check. Don't probe
   `/healthz` as a readiness signal: on Cloud Run the literal path
   `/healthz` is reserved by Cloud Run's own infrastructure and 404s — the
   handler is aliased to `/health` instead (README "Health endpoints"). A
   404 on `/healthz` does not mean the deploy is broken.
3. **Broker registration**, read-only, from the revision's own logs — do not
   infer this from the service simply responding. `<revision-name>` is the
   `latestReadyRevisionName` from the previous step:
   ```bash
   deploy/terraform/tools/check-broker.sh -p <project> -s <hub_name> <revision-name>
   ```
4. **A cold first agent may return a 503 and still go on to start.** On a
   cold GKE Autopilot node, provisioning plus pulling a roughly 1 GB
   harness image can exceed the hub's own client timeout for calls to the
   runtime broker (upstream, hard-coded — see the README's "Cold start"
   section and ptone/scion#1956). This is not necessarily a deployment
   failure — but before retrying the create, confirm whether the agent
   actually started (list agents, or check for the agent's pod in the
   hub's GKE namespace) rather than retrying blindly: a blind retry on top
   of an agent that did start risks creating a duplicate ("created,
   response lost" is a known failure mode, not merely hypothetical).

---

## 9. Post-Apply: Hub Environment

**Stop and ask the user** if you do not already have authenticated `scion`
CLI access to the new hub — this step needs it, and nothing earlier in this
runbook provisions it for you (it's separate from the GCP operator
credentials used for the Terraform applies above).

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

- **Never push, retag, or move `:latest` (or any existing tag) in the
  shared registry without an explicit user ack.** Agent harness images are
  pulled as `scion-<harness>:latest` by every hub on this shared infra, so
  moving that tag is a cross-hub change, not one scoped to the hub you're
  rolling. Stop and ask the user — only they decide whether and when. See
  step 5 for the full rule.
- **Pin `hub_image` by immutable digest, or an immutable tag — never
  `:latest`.** Cloud Run resolves a tag to a digest once, at the moment it
  creates a revision — it does not track the tag afterwards. So moving a
  `:latest` tag that a running `hub_image` value already references does
  **not** retroactively change any existing revision. The real risks are:
  (1) the *next* revision roll — even an unrelated settings edit that
  forces a new revision for another reason (see §11) — silently picks up
  whatever `:latest` now points to, not what you last verified; and (2) the
  previous state can no longer be named for rollback, because the tag
  string in `hub_image` no longer identifies the image that's actually
  running. Always change `hub_image` to a new, distinct value (a new
  digest or a new immutable tag) to roll — push new images under new
  immutable tags only.
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
  backing secret versions haven't been destroyed:
  - The **settings** secret version uses `deletion_policy = ABANDON`, so an
    older settings version (and any revision still pinned to it) keeps
    working after a replace — rollback to it is possible.
  - The **`db_password`/`db_dsn`** secret versions keep the module's
    default `deletion_policy = DELETE` (`hub-cloudrun/main.tf`) — once a DB
    password rotation (§ README "Rotating a hub's DB password") replaces
    them, the pre-rotation versions are destroyed and that password is
    dead. **Rollback to a revision older than the hub's last
    `db_password_rotation` change is not possible**, regardless of whether
    the revision itself still exists.
- **If an apply fails or only partially completes:** do not destroy
  anything and do not add `-target` to work around it. Re-plan into a new
  plan file, report the plan to the user (it will likely show the partial
  state alongside the originally intended change), and go through the
  plan-review gate again before applying anything further.

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
- **Connection budget.** `max_instances × 10` (the rendered
  `database.max_open_conns`, fixed in the module) should stay ≤ 40
  (`hub-cloudrun`'s `max_connections_budget` default — a module variable
  **not exposed by `configurations/hub`**, so it can't be raised from hub
  tfvars). A `check "connection_budget"` block (`hub-cloudrun/main.tf`)
  **warns, it does not block the apply.** Treat that warning as a stop
  under the plan-review gate rather than letting the apply proceed past it.
  The sum across all hubs sharing the instance must in turn stay ≤
  `shared-infra`'s `sql_max_connections` (default 200) — that project-wide
  sum is the operator's responsibility; nothing in this module set checks
  it automatically.
- **A cold Autopilot node's first agent can return a 503 and still go on to
  start.** Node provisioning plus pulling a roughly 1 GB harness image can
  exceed the hub's own client timeout for calls to the runtime broker,
  which is hard-coded upstream — not a Terraform variable, not in
  `settings.yaml` (see the README's "Cold start" section). This is a known
  upstream limitation (tracked as ptone/scion#1956), not a deployment
  defect — don't chase it as one, but do confirm the agent actually started
  (see step 8) before retrying, rather than retrying blindly.
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
  naming and needs an upgrade instead. The modules no longer carry the
  legacy hub-scope grant or the legacy OIDC-signing-key pre-create at all
  (a fresh hub generates and stores its own OIDC key on first boot under
  the hub-prefixed name) — see
  [`docs/deploy/migrate-names-cloudrun.md`](migrate-names-cloudrun.md) for
  the migration that preceded their removal.
- **Upgrading an existing hub to a module version without the legacy
  grant/pre-create:** see
  [`docs/deploy/migrate-names-cloudrun.md`](migrate-names-cloudrun.md#7-for-terraform-managed-hubs-what-can-be-removed-afterward)
  §7 for the required run order and what to expect in the plan.
- **Changing a hub's NFS endpoint is a manual migration.**
  - **Symptom:** a hub apply stops with
    `persistentvolumes "<hub_name>-nfs" already exists` (or the same for
    `storageclasses`). It happens after a change to the NFS server address
    or the share path, for example a new or recreated Filestore instance
    (new IP) or a changed `share_name` in shared-infra.
  - **Why:** `agent-runtime-k8s` gives the PV, PVC and StorageClass fixed
    names (`<hub_name>-nfs`). `hub-cloudrun`'s settings secret version is
    `create_before_destroy` and embeds the PV name (`pv_name` in
    `settings.yaml`). Terraform therefore applies create-before-destroy to
    the PV and to the PV's dependencies: the StorageClass (through
    `storage_class_name`) and the `nfs-init` Job (the PV `depends_on` the
    Job). The replacement PV is created under the same name before the old
    one is deleted, and the Kubernetes API rejects it.
  - **What forces the replace** (`hashicorp/kubernetes` 2.38.0, the version
    in `.terraform.lock.hcl`):
    - PV: any change under `spec.persistent_volume_source`, which here means
      `nfs.server` (the Filestore IP from `shared-lookup`) or `nfs.path`
      (`<share_path>/<hub_name>`). `volume_mode` and `metadata.name` also
      force a replace, but the module never changes them. `mount_options`,
      `capacity`, `access_modes`, `persistent_volume_reclaim_policy` and
      `storage_class_name` update in place.
    - StorageClass: `storage_provisioner`, `parameters`,
      `volume_binding_mode`, `mount_options`, `allowed_topologies` and
      `metadata.name`. All of these are fixed in the module, and none come
      from the NFS endpoint, so an endpoint change alone does not replace
      the StorageClass. Only an edit to the module itself does.
    - PVC: not replaced. Its replace-forcing fields (`volume_name`,
      `storage_class_name`, `access_modes`, `selector`, `volume_mode` and
      `resources.limits`) are fixed names or literals, or left unset.
    - The `nfs-init` Job is also replaced, because its pod template mounts
      the same server and share. It uses `generate_name`, so the new Job
      does not hit a name collision.
  - **Cloud Run also changes.** The hub service mounts NFS from the same
    inputs (`nfs_server` from `shared-lookup`, `nfs_export` =
    `<share_path>/<hub_name>` from `agent-runtime-k8s`). These values are
    also rendered into `settings.yaml`. So the same plan replaces the
    settings secret version and updates the service in place, which rolls a
    new revision mounting the new endpoint.
  - **It fails safe.** The PV create fails before anything is destroyed,
    and the PV's reclaim policy is `Retain`. No data on either share is
    removed. Because the Job is create-before-destroy and the PV depends on
    it, the new `nfs-init` Job has normally already run against the new
    share by the time the PV create fails. It only creates
    `<hub_name>/<subpath_root>` and sets ownership on those two directories.
  - **The data copy needs the old share to still exist.** In
    `hashicorp/google` 8.4.0, `file_shares.name` is ForceNew, so a
    `share_name` change replaces the Filestore instance. Replacing or
    recreating the instance destroys the old share and every hub's data on
    it. The `filestore` module sets `prevent_destroy = true` and deletion
    protection, so the shared-infra plan stops with an error until both are
    removed. That shared-infra change runs before any hub apply hits the
    symptom above, so step 2 below is too late in that case. Before a
    `share_name` change or an instance recreate:
    - copy each hub's `<share_path>/<hub_name>/` off the old share, or
      create the new instance alongside the old one and copy across, before
      the shared-infra change is applied;
    - run the procedure below for every hub on the shared instance, not
      just one.
  - **Procedure:**
    1. Get cluster credentials:
       `gcloud container clusters get-credentials <shared_prefix>-agents --region <region> --project <project>`.
       Stop or delete the hub's agents, and confirm that no agent pods
       remain: `kubectl get pods -n <hub_name>`. Completed `nfs-init` Job
       pods do not matter, because they mount the share directly, not
       through the PVC. Do not start agents or create projects until step 6.
       Until the apply in step 5, the hub's Cloud Run revision still mounts
       the old share, so anything written after the step 2 copy stays on the
       old share and is missing after the cutover.
    2. If the workspace data is being kept, copy it to the same path on the
       new share. Run this as root from a host or pod that mounts both
       shares (the share has no root squash):
       ```bash
       rsync -aH --numeric-ids <old_mount>/<hub_name>/ <new_mount>/<hub_name>/
       ```
       The trailing slashes copy the contents of the old `<hub_name>`
       directory into the new one, which the `nfs-init` Job may already
       have created, rather than nesting it as `<hub_name>/<hub_name>`.
       `-a` with `--numeric-ids` keeps the numeric ownership
       (`nfs_uid`:`nfs_gid`, default 1000:1000). The copy is a
       point-in-time snapshot; see step 1.
    3. Delete the PVC, then the PV:
       ```bash
       kubectl delete pvc <hub_name>-nfs -n <hub_name>
       kubectl delete pv <hub_name>-nfs
       ```
       The PV is `Retain`, so the data on the old share is not touched. If
       the PVC stays `Terminating`, a pod is still using it; go back to
       step 1.
    4. Delete the StorageClass (`kubectl delete storageclass <hub_name>-nfs`)
       only if the plan shows it being replaced. Deleting it does not affect
       existing volumes.
    5. Re-plan into a new plan file and go through the plan-review gate
       (§4, "Plan-review gate"). The PV and PVC (and the StorageClass, if
       deleted) should now show as creates, not replaces. Expect the
       settings secret version to be replaced and the Cloud Run service to
       be updated. For the `nfs-init` Job:
       - after the failed apply described under Symptom, the new Job is
         already current in state, so expect only a destroy of the old,
         deposed Job, not a Job replace;
       - expect a Job replace only if you are running this procedure
         before any failed apply, or if the Job create itself failed in the
         earlier apply.

       Apply. Terraform recreates the objects under the same names.
    6. Verify per §8.

---

## 12. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `403` on `scion-<hash>-...` shortly after a hub's first apply | IAM condition propagation delay | Re-apply. Do not widen the condition. |
| Agent starts but can't reach Vertex despite Workload Identity being wired | GCP identity is still Block | Set Passthrough — see step 9. |
| Agent pod fails with image-pull `NotFound` on `workspace-provision` | Harness image not published to `image_registry` | Publish the image, or pick a different harness. See the README's "Harness images" section. |
| Agent create returns `503` but the agent goes on to start | Cold Autopilot node exceeding the hub's client timeout to the runtime broker | Not necessarily a failure. Confirm whether the agent started (step 8.4) before retrying the *create* — a blind retry on an agent that did start risks creating a duplicate. |
| Creating a user or project secret fails with a hint to grant `roles/secretmanager.admin` | Hub image predates hub-prefixed secret names | Do not follow the hint in a shared project (see "Operational traps"). Roll a hub image with hub-prefixed secret support instead. |
| Second plan on any root is not clean | Real drift, or a genuinely intended config change not yet applied everywhere | Read the diff. Don't assume it's benign; only refresh-only notes with no planned action are. |
| Hub apply fails partway through with a permission error | Operator role set was incomplete | Check the role list in step 1 was granted in full before the apply started; grant the missing role and re-apply. |

---

## 13. Teardown

**Stop: explicit human ack per step, before every single step below — no
exceptions, and no proceeding to the next step on the strength of an ack
already given for a previous one.**

**Order: every hub root first, then shared-infra — never the reverse.**
This is a multi-step, deliberately redundant sequence in the README's
"Destroy runbook" section; **read it in full for the rationale and the
prohibitions, then execute the gated sequence below — it supersedes any
paraphrased or bare command in the README's prose.** The step numbers and
commands below match the README's exactly:

1. **Stop: get an explicit ack, naming the exact bucket, before running
   this.** It is irreversible data deletion with no plan to review. For
   each hub: empty its artifacts bucket, **all versions**:
   ```bash
   gcloud storage rm -r --all-versions gs://<project>-<hub>-artifacts/**
   ```
   Then plan and apply the destroy through the plan-review gate — never a
   bare `destroy`:
   ```bash
   terraform -chdir=deploy/terraform/configurations/hub plan -destroy \
     -var hub_name=<hub_name> -var state_prefix=<prefix>/hubs/<hub_name> \
     -var-file=<hub_name>.tfvars -out=/tmp/<hub_name>-destroy.tfplan
   terraform -chdir=deploy/terraform/configurations/hub apply /tmp/<hub_name>-destroy.tfplan
   ```
2. This is a normal (non-destroy) `plan`/`apply` that only flips
   `deletion_protection` off — do not use `plan -destroy` here, since the
   `destroy_guard` precondition below is a `plan`/`apply`-time check, not a
   `terraform destroy`:
   ```bash
   terraform -chdir=deploy/terraform/configurations/shared-infra plan \
     -var-file=terraform.tfvars -var deletion_protection=false \
     -out=/tmp/shared-unprotect.tfplan
   ```
   The `destroy_guard` precondition is checked at **plan** time (it's a
   `terraform_data` precondition), so a failure shows up on the `plan`
   command above, before you ever reach the gate or an apply. **If the plan
   fails on `destroy_guard`**, that means a hub database still exists — step
   1 wasn't completed for every hub. **Stop** and go back to step 1; do not
   proceed past this by any other means. Otherwise, go through the
   plan-review gate, then:
   ```bash
   terraform -chdir=deploy/terraform/configurations/shared-infra apply /tmp/shared-unprotect.tfplan
   ```
   This apply **succeeds once every hub is gone.**
3. **Stop: only proceed here when the user has explicitly directed a full
   shared-infra teardown** — this step removes the safety net for every
   piece of shared, stateful infrastructure. On a dedicated teardown branch
   (never merged to `main`), commit removing
   `lifecycle { prevent_destroy = true }` from the three shared stateful
   modules (`cloudsql-instance`, `filestore`, `gke-autopilot`).
4. **Stop: get an explicit ack before this destroy**, the same as any
   other. From that branch:
   ```bash
   terraform -chdir=deploy/terraform/configurations/shared-infra plan -destroy \
     -var-file=terraform.tfvars -out=/tmp/shared-final-destroy.tfplan
   terraform -chdir=deploy/terraform/configurations/shared-infra apply /tmp/shared-final-destroy.tfplan
   ```
5. Compare a before/after resource inventory to confirm nothing outside the
   deployment's prefix was touched.

**Never** run `terraform destroy -var deletion_protection=false` against
`shared-infra`, at any step — step 2 above is an ordinary `plan`/`apply`,
not a `destroy`, and is not an exception to this rule. Likewise **never**
combine `-target` with `deletion_protection` on `shared-infra` — both bypass
the guard and can leave a partial, inconsistent destroy behind. Every
destroy step above is itself a plan-review gate: get an explicit human ack
before running any of them, the same as an apply.
