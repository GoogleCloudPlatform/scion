---
title: "Runbook: Secret Name Migration on Cloud Run (Private SQL)"
description: Run `scion hub secret migrate-names` against a Cloud Run hub whose Cloud SQL instance has no public IP, using a one-off Cloud Run job.
---

`scion hub secret migrate-names` renames a hub's GCP Secret Manager secrets from the
legacy naming scheme to the hub-prefixed scheme (see
[IAM Permissions and Secret Naming](/scion/hosted/user/secrets/#iam-permissions-and-secret-naming)).
The command opens the hub's own database directly from a DSN. On a Cloud Run hub whose
Cloud SQL instance only has a private IP (the standard HA topology — see
[Deploy on GCP](/scion/hosted/ha/setup-gcp/)), an operator's workstation has no network
path to that database, so the CLI cannot be run from a laptop at all.

This runbook works around that with a **one-off Cloud Run job**: an ephemeral job
that borrows the running service's own identity, VPC wiring and Cloud SQL connection
to reach the database, runs the migration, and is deleted afterward. It is **not**
Terraform-managed — there is no `google_cloud_run_v2_job` resource for this. The job
is created ad hoc with `gcloud`, used for the four migration passes below, and deleted.
See the design decision (ptone/scion#2358) for why a permanent resource wasn't worth it
for a migration that `--delete-legacy` (and a later removal of the legacy IAM grant)
retires anyway.

No human ever sees the database credential: the DSN stays a Secret Manager reference
passed to the job with `--set-secrets`, the same way it reaches the real service.

Every command below uses placeholders — `PROJECT`, `PROJECT_NUMBER`, `REGION`, `HUB` —
and every real value is *discovered* from the live service with `gcloud`, not typed in
by hand. Do not fill in a real project ID or hub name in this page.

## What the job mounts, and why

The job must mirror enough of the [hub-cloudrun Terraform module](https://github.com/GoogleCloudPlatform/scion)'s
service definition to reach the same database as the same identity — but not more than
that. Checked against `cmd/hub_secret_migrate_names.go`:

| Service has it for... | migrate-names needs it? | Included in the job? |
| :--- | :--- | :--- |
| Service account (`hub_sa_email`) | Yes — it's the identity used for both Cloud SQL and Secret Manager access; no new IAM is created for the job. | Yes, `--service-account` |
| Direct VPC egress (network/subnet, `PRIVATE_RANGES_ONLY`) | Yes — private-IP Cloud SQL is only reachable from inside the VPC. | Yes, `--network`/`--subnet`/`--vpc-egress` |
| `/cloudsql` Cloud SQL volume | Yes — the DSN embeds `?host=/cloudsql/<connection name>`; without the volume the socket path doesn't exist and the connection fails. | Yes, `--set-cloudsql-instances` |
| `SCION_SERVER_DATABASE_URL` secret env (the DSN) | Yes — this is the only way the command opens the database. | Yes, `--set-secrets` |
| `settings.yaml` secret, mounted as a file | Yes, but only for one field: **`server.database.driver: postgres`**. `cfg.Database.Driver` defaults to `""`, which `openMigrateNamesStore`'s `switch` treats as `"sqlite"` — with no settings file and no driver env var, the job would silently try to open the Postgres DSN as a sqlite file. `--config` pointed at the mounted file is what makes the command see `driver: postgres`. The file also carries `server.hub.hub_id`, which is a second, corroborating source for the hub ID (see below) — nothing else in it matters. | Yes, `--set-secrets`, mounted at `/run/secrets/settings.yaml` |
| `SCION_SERVER_HUB_HUBID` env var | Not required once `--hub-id` is passed explicitly (see below) — but harmless, and requires no extra secret access, so it's included for a second corroborating source of the same value. | Yes, `--set-env-vars` |
| `SCION_SERVER_SESSION_SECRET` (session secret) | No — migrate-names never touches sessions, and `secret.NewGCPBackend` doesn't consume it. | **No** |
| Kubeconfig secret / `KUBECONFIG` env / `SCION_K8S_NAMESPACE` | No — migrate-names never talks to Kubernetes. | **No** |
| NFS volume | No — migrate-names does no workspace I/O. | **No** |
| `HOME`, `SCION_REQUIRE_STABLE_SIGNING_KEY` | No — these govern the running server's own boot behavior (OIDC key generation, home directory for the server process), not this one-shot command. | **No** |

Mounting only what's needed keeps the job's blast radius to exactly what the CLI reads:
the DSN and the driver.

## 1. Prerequisites

### Tools

`gcloud` (authenticated as the operator) and `jq`.

### Operator IAM

The operator needs, on the project (or a custom role with just these permissions):

- `run.jobs.create`, `run.jobs.get`, `run.jobs.update`, `run.jobs.run`, `run.jobs.delete`,
  `run.jobs.list` — to create, configure, execute, and clean up the job. `roles/run.developer`
  covers all of these, plus `run.services.get` (needed for discovery, below).
- `iam.serviceAccounts.actAs` on the hub's service account specifically — grant
  `roles/iam.serviceAccountUser` scoped to that one service account resource, not
  project-wide. This is what lets the job run *as* the hub SA.
- `logging.logEntries.list` (`roles/logging.viewer`) to read the job's output.

You do **not** need `run.jobs.runWithOverrides`: every pass below sets the job's args
with `gcloud run jobs update --args=...` first and then calls
`gcloud run jobs execute --wait` with no `--args` of its own, so nothing is overridden
at execute time. If you instead pass `--args` directly to `execute`, you need that
permission too.

You do **not** need any Secret Manager IAM on the DSN, the settings secret, or
anything else the job reads — the job runs as the hub SA, which already holds those
grants. The DSN is never something the operator's own credentials can read.

## 2. Discover the live service's configuration

Set your placeholders once:

```bash
export PROJECT="PROJECT"
export REGION="REGION"
export HUB="HUB"
```

Pull the full service definition once and query it with `jq` — nested fields like
annotations and env lists are awkward to reach with `--format=value()` alone:

```bash
SVC=$(gcloud run services describe "$HUB" --project "$PROJECT" --region "$REGION" --format=json)
```

**Service account:**

```bash
SA=$(echo "$SVC" | jq -r '.spec.template.spec.serviceAccountName')
```

**Network, subnet, VPC egress** (Direct VPC egress is expressed as annotations on the
revision template):

```bash
NET_JSON=$(echo "$SVC" | jq -r '.spec.template.metadata.annotations["run.googleapis.com/network-interfaces"]')
NETWORK=$(echo "$NET_JSON" | jq -r '.[0].network')
SUBNET=$(echo "$NET_JSON" | jq -r '.[0].subnetwork')
EGRESS=$(echo "$SVC" | jq -r '.spec.template.metadata.annotations["run.googleapis.com/vpc-access-egress"]')
```

**Cloud SQL connection:**

```bash
CONN=$(echo "$SVC" | jq -r '.spec.template.metadata.annotations["run.googleapis.com/cloudsql-instances"]')
```

**Hub ID** (from the live env var — cross-checked again in §3 against the settings
file the job itself mounts):

```bash
HUB_ID=$(echo "$SVC" | jq -r '.spec.template.spec.containers[0].env[] | select(.name=="SCION_SERVER_HUB_HUBID") | .value')
```

**DSN secret name and version** (do not print `$DSN_JSON`'s sibling fields — this
only ever holds the secret's *coordinates*, never the DSN value itself):

```bash
DSN_JSON=$(echo "$SVC" | jq -r '.spec.template.spec.containers[0].env[] | select(.name=="SCION_SERVER_DATABASE_URL") | .valueFrom.secretKeyRef')
DSN_SECRET=$(echo "$DSN_JSON" | jq -r '.name')
DSN_VERSION=$(echo "$DSN_JSON" | jq -r '.key')
```

**Settings secret name and version** (mounted as a file on the live service; the job
mounts the same coordinates):

```bash
SETTINGS_JSON=$(echo "$SVC" | jq -r '.spec.template.spec.volumes[] | select(.name=="settings") | .secret')
SETTINGS_SECRET=$(echo "$SETTINGS_JSON" | jq -r '.secretName')
SETTINGS_VERSION=$(echo "$SETTINGS_JSON" | jq -r '.items[0].version')
```

**Image, pinned to the serving revision's digest — not the tag:**

```bash
REVISION=$(echo "$SVC" | jq -r '.status.latestReadyRevisionName')
REV_JSON=$(gcloud run revisions describe "$REVISION" --project "$PROJECT" --region "$REGION" --format=json)
IMAGE_REPO=$(echo "$SVC" | jq -r '.spec.template.spec.containers[0].image' | cut -d: -f1)
IMAGE_DIGEST=$(echo "$REV_JSON" | jq -r '.status.imageDigest')
IMG="${IMAGE_REPO}@${IMAGE_DIGEST}"
```

:::caution[Why the digest, not the tag]
A non-dry `migrate-names` run also executes the hub's `ent` schema migration
(`cs.Migrate(ctx)`) against the live database. That's a no-op only when the job runs the
**exact same binary** the serving revision runs. A tag (`:latest`, `:v1.2.3` if it was
ever force-pushed, or a rolling `:stable`) can silently point at a different image by
the time the job runs than it did when you resolved it. The digest is immutable: pinning
to it is what makes "same binary as the service" a guarantee instead of an assumption.
:::

Sanity-check `$HUB_ID` once you have the settings secret's version — decode it and
confirm `server.hub.hub_id` agrees with the env var pulled above (both should be the
same value the service itself resolves):

```bash
gcloud secrets versions access "$SETTINGS_VERSION" --secret="$SETTINGS_SECRET" --project "$PROJECT" | grep hub_id
```

## 3. Create the job

```bash
gcloud run jobs create "${HUB}-migrate-names" \
  --project "$PROJECT" \
  --region "$REGION" \
  --image "$IMG" \
  --service-account "$SA" \
  --network "$NETWORK" \
  --subnet "$SUBNET" \
  --vpc-egress "$EGRESS" \
  --set-cloudsql-instances "$CONN" \
  --set-secrets "/run/secrets/settings.yaml=${SETTINGS_SECRET}:${SETTINGS_VERSION},SCION_SERVER_DATABASE_URL=${DSN_SECRET}:${DSN_VERSION}" \
  --set-env-vars "SCION_SERVER_HUB_HUBID=${HUB_ID}" \
  --max-retries 0 \
  --tasks 1 \
  --task-timeout 10m \
  --command scion
```

Notes on the flags (checked against `gcloud run jobs create --help`):

- `--max-retries 0`: a failed task must never silently retry a partially-applied
  migration pass. `migrate-names` is idempotent, but a retry would blur which attempt
  produced which log lines.
- `--tasks 1`: this is a single-task job, not a distributed one.
- `--task-timeout 10m`: generous headroom over the CLI's own default `--timeout` of 5m
  (raise further for a hub with an unusually large number of secrets).
- `--set-secrets` with a key that starts with `/` mounts a **file**; a bare key name
  sets an **env var**. Both forms are combined in one comma-separated flag here,
  mirroring exactly what the service does for `SCION_SERVER_DATABASE_URL` and the
  settings secret.
- No `--args` yet — every pass below sets them explicitly with `jobs update`, so the
  job is created without a default mode to run in.

## 4. The four migration passes

Each pass follows the same two-step shape: set the args (they **replace the whole
list**, so always restate `--gcp-project` and `--hub-id`), then execute and wait.

```bash
JOB="${HUB}-migrate-names"
BASE_ARGS="hub,secret,migrate-names,--gcp-project=${PROJECT},--hub-id=${HUB_ID},--config=/run/secrets/settings.yaml"
```

### Pass 1 — dry run

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--dry-run"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

Read the output (§5). Expect `MIGRATED` / `RESYNCED` / `REPAIRED REF` plan lines for
anything not yet on the hub-prefixed name, and `0 failed`.

### Pass 2 — run

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS}"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

Confirm `0 failed` and no `CONFLICT` lines before continuing. A `CONFLICT` here needs
diagnosis per `scion hub secret migrate-names --help` (§5) before you touch
`--delete-legacy`.

### Before pass 3 — repeat the delete precondition

`--delete-legacy` is only safe once **every** replica of this hub is running a binary
that includes the hub-prefixed naming scheme — i.e. the rollout is 100% complete and no
older revision is still serving traffic. GCP Secret Manager has no conditional delete:
if an old binary is still a live writer to the legacy name, its write can land between
the safety check and the delete, and the delete destroys that write's only copy.

Check the rollout:

```bash
echo "$SVC" | jq '.status.traffic'
```

You want exactly one entry, at `100` percent, whose `revisionName` matches
`$REVISION` (the same revision digest you pinned the job to in §2) — and
`"latestRevision": true`. If there's more than one entry, or the percentage isn't 100,
stop: an older revision is still serving, and it may still be writing legacy names.
Re-pull `$SVC` if time has passed since §2.

### Pass 3 — delete-legacy dry run

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--delete-legacy,--dry-run"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

Expect `DELETE LEGACY` plan lines for every secret still holding a legacy copy, and
`0 failed`.

### Before pass 4 — repeat the same check

Re-run the traffic check above. Do not skip this because you just did it for pass 3 —
time has passed, and a deploy could have started.

### Pass 4 — delete-legacy

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--delete-legacy"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

### Pass 5 — final verification

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--delete-legacy,--dry-run"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

**Acceptance:** the summary reports `0 failed`, and there are no `MIGRATE`, `RESYNC`,
`REPAIR REF`, or `DELETE LEGACY` lines anywhere in the output. Only then is this hub
done.

## 5. Reading the output

Job output goes to Cloud Logging, not your terminal. Get the execution name from the
last `execute` call's own output, or list it:

```bash
gcloud run jobs executions list --job "$JOB" --project "$PROJECT" --region "$REGION" \
  --format='table(metadata.name,status.conditions[0].status,startTime,completionTime)'
```

Read that execution's logs:

```bash
EXECUTION="${JOB}-xxxxx"   # from the list above

gcloud logging read '
  resource.type="cloud_run_job"
  resource.labels.job_name="'"$JOB"'"
  resource.labels.location="'"$REGION"'"
  labels."run.googleapis.com/execution_name"="'"$EXECUTION"'"
' --project "$PROJECT" --order=asc --format='value(textPayload)'
```

Look for:

- The `Using hub ID: ...` line — confirm it matches `$HUB_ID`.
- One plan line per candidate secret (or none, once converged).
- The summary line and the exit status in the execution's own condition
  (`status.conditions[0].status` in the executions list above — `True` for succeeded).

**`CONFLICT`**: a concurrent write changed the secret's ref between this command's read
and its write. It counts as failed. Do not immediately re-run with `--delete-legacy` —
follow the diagnosis procedure in `scion hub secret migrate-names --help` (a plain
re-run without `--delete-legacy` tells you whether it was a resolved false positive or
a true conflict).

**`ORPHAN`**: a hub DB record's stored ref points at a secret value that no longer
exists. This is reported and skipped, not treated as a failure — there's nothing left
to copy for that record.

## 6. Cleanup

Confirm every pass above actually completed before deleting the job — once it's gone,
so is its execution history.

```bash
gcloud run jobs executions list --job "$JOB" --project "$PROJECT" --region "$REGION"
```

Then delete it:

```bash
gcloud run jobs delete "$JOB" --project "$PROJECT" --region "$REGION"
```

Confirm nothing is left:

```bash
gcloud run jobs list --project "$PROJECT" --region "$REGION" --filter="metadata.name=${JOB}"
```

This should return no rows.

## 7. For Terraform-managed hubs: what can be removed afterward

Once the final dry run in §4 (pass 5) shows zero pending items for **every** hub
sharing a GCP project, the terraform-ha legacy IAM grant and the legacy pre-created
secret that only existed to bridge the old naming scheme can be removed, each as its
own per-resource acknowledgment rather than a single blanket change:

- the legacy conditioned `secretmanager.admin` (or equivalent) grant scoped to the
  hub's pre-migration secret-name prefix (`scion-hub-<hash>-*`) — see
  [IAM Permissions and Secret Naming](/scion/hosted/user/secrets/#iam-permissions-and-secret-naming)
  for how that prefix and its hub-prefixed replacement are computed;
- the Terraform-managed pre-create of the legacy-named OIDC signing key secret —
  once every hub image resolves the OIDC key under the hub-prefixed name instead,
  the pre-create under the legacy name is no longer read by anything.

Do this only after confirming the final dry run's "zero pending" result — removing the
legacy grant first means `migrate-names` can no longer even read the legacy names, so a
"zero pending" result measured after removal only proves IAM was narrowed, not that
migration finished.

## 8. Break-glass alternative

If the Cloud Run job approach is unavailable (for example, `gcloud` job creation is
blocked by an org policy), a bastion host or pod placed inside the same VPC can run the
CLI directly: fetch the DSN secret value, impersonate the hub SA for Secret Manager
access, and run `scion hub secret migrate-names` from there. This works, but it
requires a human (or a script running as a human's credentials) to read the DSN and to
impersonate the hub SA — both of which the Cloud Run job path in this runbook avoids
entirely. Use this only as a fallback, and treat the DSN as compromised once it has
been read onto a bastion — rotate it afterward if policy requires that.
