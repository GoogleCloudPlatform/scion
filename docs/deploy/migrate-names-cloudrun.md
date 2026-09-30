# Runbook: Secret Name Migration on Cloud Run (Private SQL)

Run `scion hub secret migrate-names` against a Cloud Run hub deployed with the
hub-cloudrun Terraform module, using a one-off Cloud Run job.

`scion hub secret migrate-names` renames a hub's GCP Secret Manager secrets from the
legacy naming scheme to the hub-prefixed scheme (see
[Secrets: IAM Permissions and Secret Naming](https://scion-ai.dev/scion/hosted/user/secrets/#iam-permissions-and-secret-naming)).
The command opens the hub's own database directly from a DSN.

**Scope:** this runbook is for Cloud Run hubs deployed with the hub-cloudrun
Terraform module — private-IP Cloud SQL (reachable only from inside the VPC), Direct
VPC egress, the DSN delivered as the `SCION_SERVER_DATABASE_URL` secret env var, and
`settings.yaml` mounted from a secret volume. An operator's workstation has no
network path to that database, so the CLI cannot be run from a laptop at all. Other
Cloud Run layouts (including a hub whose database has a public IP, or one wired up
by hand rather than by that module) will need the discovery steps in §2 adapted to
their own service definition — and if the database *is* reachable from your
workstation, you don't need this runbook at all: run
`scion hub secret migrate-names` directly with the CLI (`--help` for its flags).

This runbook works around the no-network-path problem with a **one-off Cloud Run
job**: an ephemeral job that borrows the running service's own identity, VPC wiring
and Cloud SQL connection to reach the database, runs the migration, and is deleted
afterward. It is **not** Terraform-managed — there is no `google_cloud_run_v2_job`
resource for this. The job is created ad hoc with `gcloud`, used for the five
migration passes below, and deleted. A permanent resource isn't worth it for a
one-time migration that `--delete-legacy` (and a later removal of the legacy IAM
grant) retires anyway.

No human ever sees the database credential: the DSN stays a Secret Manager reference
passed to the job with `--set-secrets`, the same way it reaches the real service.

Every command below uses placeholders — `PROJECT`, `PROJECT_NUMBER`, `REGION`, `HUB` —
and every real value is *discovered* from the live service with `gcloud`, not typed in
by hand. Do not fill in a real project ID or hub name in this page.

## What the job mounts, and why

The job must mirror enough of the hub-cloudrun Terraform module's service definition
to reach the same database as the same identity — but not more than that. Checked
against `cmd/hub_secret_migrate_names.go`:

| Service has it for... | migrate-names needs it? | Included in the job? |
| :--- | :--- | :--- |
| The service's runtime service account | Yes — it's the identity used for both Cloud SQL and Secret Manager access; no new IAM is created for the job. | Yes, `--service-account` |
| Direct VPC egress (network/subnet, `PRIVATE_RANGES_ONLY`) | Yes — private-IP Cloud SQL is only reachable from inside the VPC. | Yes, `--network`/`--subnet`/`--vpc-egress` |
| `/cloudsql` Cloud SQL volume | Yes — the DSN embeds `?host=/cloudsql/<connection name>`; without the volume the socket path doesn't exist and the connection fails. | Yes, `--set-cloudsql-instances` |
| `SCION_SERVER_DATABASE_URL` secret env (the DSN) | Yes — this is the only way the command opens the database. | Yes, `--set-secrets` |
| `settings.yaml` secret, mounted as a file | Yes, but only for one field: **`server.database.driver: postgres`**. Without the settings file (or `SCION_SERVER_DATABASE_DRIVER`), the driver resolves to sqlite — `openMigrateNamesStore`'s `switch` treats an empty/default driver as `"sqlite"` (the legacy loader's own default), so the job would silently try to open the Postgres DSN as a sqlite file. `--config` pointed at the mounted file is what makes the command see `driver: postgres`. The file also carries `server.hub.hub_id`, a second corroborating source for the hub ID (see below) — nothing else in it matters. | Yes, `--set-secrets`, mounted at `/run/secrets/settings.yaml` |
| `SCION_SERVER_HUB_HUBID` env var | Not required once `--hub-id` is passed explicitly (see below) — but harmless, and requires no extra secret access, so it's included as a second corroborating source of the same value. | Yes, `--set-env-vars` |
| `SCION_SERVER_SESSION_SECRET` (session secret) | No — migrate-names never touches sessions, and `secret.NewGCPBackend` doesn't consume it. | **No** |
| Kubeconfig secret / `KUBECONFIG` env / `SCION_K8S_NAMESPACE` | No — migrate-names never talks to Kubernetes. | **No** |
| NFS volume | No — migrate-names does no workspace I/O. | **No** |
| `HOME`, `SCION_REQUIRE_STABLE_SIGNING_KEY` | No, but `HOME` still matters: `LoadGlobalConfig` reads `$HOME/.scion/settings.yaml` **before** `--config`'s directory, and if `$HOME` can't be resolved it skips settings.yaml entirely. Leaving `HOME` out is safe only because the image sets `ENV HOME=/home/scion` and the job bypasses `entrypoint.sh`, so nothing gets copied into `~/.scion` that could shadow `--config`. | **No** |

Mounting only what's needed keeps the job's blast radius to exactly what the CLI reads:
the DSN and the driver.

## 1. Prerequisites

### Tools

`gcloud` (authenticated as the operator) and `jq`.

Run every command in this runbook with **bash**, not `zsh` — zsh's `echo` can mangle
embedded JSON (its handling of backslashes and quoting differs from bash's), which
breaks the `jq` pipelines in §2. If you're in an interactive zsh shell, start a bash
subshell first (`bash`) before pasting any of the commands below.

### Operator IAM

The operator needs, on the project (or a custom role with just these permissions):

- `run.jobs.create`, `run.jobs.get`, `run.jobs.update`, `run.jobs.run`, `run.jobs.delete`,
  `run.jobs.list`, `run.executions.get`, `run.executions.list`, `run.revisions.get` — to
  create, configure, execute, read the results of, and clean up the job (the
  executions/revisions permissions cover `execute --wait`, §5's executions list, §6's
  cleanup, and §2's revision lookup). `roles/run.developer` covers all of these, plus
  `run.services.get` (needed for discovery, below).
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
grants. The DSN is never something the operator's own credentials can read, and (see
§2) this runbook never reads the settings secret's contents either.

`gcloud run jobs delete` (§6) prompts for confirmation interactively; the cleanup
command below passes `--quiet` to skip that. If you abandon this procedure midway for
any reason, still do §6 — an orphaned job with a pinned old digest is a trap for
whoever finds it next.

## 2. Discover the live service's configuration

Set your placeholders once:

```bash
export PROJECT="PROJECT"
export REGION="REGION"
export HUB="HUB"
```

`PROJECT` here doubles as `--gcp-project` below (the Secret Manager project) — that's
correct as long as this hub's `server.secrets.gcp_project_id` is the same project the
service itself runs in, which is the case for the hub-cloudrun module's default
wiring. If your hub's secrets live in a different project, use that project for
`--gcp-project` instead of `$PROJECT`.

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

**Hub ID** (from the live env var — this is the *only* hub-ID guard this procedure
has; see the note below):

```bash
HUB_ID=$(echo "$SVC" | jq -r '.spec.template.spec.containers[0].env[] | select(.name=="SCION_SERVER_HUB_HUBID") | .value')
```

**DSN secret name and version** (`$DSN_JSON` holds only the secret's *name and
version* — a `secretKeyRef` — never the DSN value itself, so it's safe to print):

```bash
DSN_JSON=$(echo "$SVC" | jq -r '.spec.template.spec.containers[0].env[] | select(.name=="SCION_SERVER_DATABASE_URL") | .valueFrom.secretKeyRef')
DSN_SECRET=$(echo "$DSN_JSON" | jq -r '.name')
DSN_VERSION=$(echo "$DSN_JSON" | jq -r '.key')
```

**Settings secret name and version** (mounted as a file on the live service; the job
mounts the same coordinates). The Cloud Run v1 API's secret volume item is
`{key, path}` — the Secret Manager version is in `.key` (there is no `.version`
field); select by `path` so this doesn't silently pick the wrong item if the volume
ever carries more than one:

```bash
SETTINGS_JSON=$(echo "$SVC" | jq -r '.spec.template.spec.volumes[] | select(.name=="settings") | .secret')
SETTINGS_SECRET=$(echo "$SETTINGS_JSON" | jq -r '.secretName')
SETTINGS_VERSION=$(echo "$SETTINGS_JSON" | jq -r '.items[] | select(.path=="settings.yaml") | .key')
```

**Revision and image, pinned to the serving revision's digest — not the tag.** Pick
the revision from the traffic split (not `latestReadyRevisionName`), so a service with
pinned, non-latest traffic is handled correctly too:

```bash
REVISION=$(echo "$SVC" | jq -r '.status.traffic[] | select(.percent==100) | .revisionName')
REV_JSON=$(gcloud run revisions describe "$REVISION" --project "$PROJECT" --region "$REGION" --format=json)
IMG=$(echo "$REV_JSON" | jq -r '.status.imageDigest')
```

`.status.imageDigest` on a v1 Revision is already the resolved **full reference**
(`<registry>/<path>@sha256:<hex>`), not a bare `sha256:<hex>` — use it as-is for
`--image` below. Do not reconstruct it from the service spec's image plus this
digest; that doubles the repo path.

> **Caution — why the digest, not the tag:** A non-dry `migrate-names` run also
> executes the hub's `ent` schema migration (`cs.Migrate(ctx)`) against the live
> database. That's a no-op only when the job runs the **exact same binary** the
> serving revision runs. A tag (`:latest`, `:v1.2.3` if it was ever force-pushed, or a
> rolling `:stable`) can silently point at a different image by the time the job runs
> than it did when you resolved it. The digest is immutable: pinning to it is what
> makes "same binary as the service" a guarantee instead of an assumption.

**Check before you continue.** `jq -r` prints the literal string `null` for a missing
path, and several of the lookups above pipe one `jq` result into another — if one
lookup misses, a job could get created with `--network null` or a secret `null:null`
without anything failing loudly. None of these values is a secret (the DSN and
settings secret are referenced by name/version only, never by content), so printing
them is safe:

```bash
for v in SA NETWORK SUBNET EGRESS CONN HUB_ID DSN_SECRET DSN_VERSION SETTINGS_SECRET SETTINGS_VERSION REVISION IMG; do
  eval "val=\$$v"; case "$val" in ""|null) echo "MISSING: $v" >&2; false;; *) printf '%s=%s\n' "$v" "$val";; esac
done
```

**On the hub-ID check:** passing `--hub-id` explicitly (§4) turns off
`migrate-names`'s own built-in cross-check against existing hub DB records
(`checkMigrateNamesHubIDAgainstExistingRecords` in `cmd/hub_secret_migrate_names.go`
runs only when `--hub-id` is *not* passed) — so the `HUB_ID` env-var read above is the
only guard this procedure has against a wrong prefix. Trust it because it's the same
value the running hub server itself resolves at boot, and because every pass's own
`Using hub ID: ...` log line (§5) echoes it back, letting you confirm it reached the
job unchanged. This runbook deliberately does **not** read the settings secret's
content to double-check it — that would require a human (or a script running as one)
to read a secret value, which this whole procedure is designed to avoid. If you want
independent corroboration, compare the job's `Using hub ID: ...` log line across
passes instead of reading the settings secret.

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
- `--command scion` replaces the image entrypoint, which would otherwise start the
  hub server.
- No `--args` yet — every pass below sets them explicitly with `jobs update`, so the
  job is created without a default mode to run in.

## 4. The migration passes

Each pass follows the same two-step shape: set the args (they **replace the whole
list**, so always restate `--gcp-project` and `--hub-id`), then execute and wait.
Passes 1, 3 and 5 are dry runs: `migrate-names` opens Postgres with
`default_transaction_read_only=on`, skips `cs.Migrate`, and calls only read-only
Secret Manager methods for a dry run, which is why they need no traffic gate.

```bash
JOB="${HUB}-migrate-names"
BASE_ARGS="hub,secret,migrate-names,--gcp-project=${PROJECT},--hub-id=${HUB_ID},--config=/run/secrets/settings.yaml,--global"
```

`--global` is required: the root command's `PersistentPreRunE` demands a scion
project for `hub secret ...` subcommands unless `--global` is passed, and this job's
container has none. `migrate-names` never uses a project path (it resolves everything
through `--config`/`LoadGlobalConfig`), so `--global` is safe here.

The **rollout check** below re-appears before passes 2, 3, and 4 — it always fetches
live state, so it can't be fooled by a stale `$SVC` captured back in §2:

```bash
gcloud run services describe "$HUB" --project "$PROJECT" --region "$REGION" --format=json \
  | jq -e --arg rev "$REVISION" '
      .status.traffic | length == 1
      and .[0].percent == 100
      and .[0].revisionName == $rev
      and (.[0].tag // "") == ""' \
  && echo "OK: 100% on $REVISION" || echo "STOP: rollout not complete or revision changed"
```

You want exactly one traffic entry, at 100%, on the revision whose digest you pinned
the job to in §2. If the check reports `STOP`, an older or newer revision is now
serving: wait for the rollout to finish, delete the job (§6), and restart from §2 so
the job is re-pinned to the new serving digest. Never run a non-dry pass from a job
whose digest isn't the 100%-traffic revision's — retrying later from the same job
won't help, since it's still pinned to the old digest.

### Pass 1 — dry run

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--dry-run"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

Read the output (§5). Expect `WOULD MIGRATE` / `WOULD RESYNC` / `WOULD REPAIR REF`
lines (joined with ` AND ` when more than one action applies to the same secret, e.g.
`WOULD REPAIR REF AND DELETE LEGACY`) for anything not yet on the hub-prefixed name,
and a summary:

```
Migrate-names dry run complete: N migrated, N skipped (already migrated or absent), 0 failed, 0 legacy secrets deleted
```

In a dry run, *migrated* counts planned candidates, not completed ones. A hub that's
already fully converged shows no `WOULD` lines at all — this is the actual output
from a live validation run against a converged hub:

```
Using hub ID: <hub-id> (prefix: scion-<12hex>-)
Migrate-names dry run complete: 0 migrated, 7 skipped (already migrated or absent), 0 failed, 0 legacy secrets deleted
```

(the count of 7 and the specific prefix are that hub's own — expect different numbers
on yours).

### Before pass 2 — check the rollout

Run the rollout check above. This is the first non-dry pass — it runs `cs.Migrate`
against the live database — so confirm the job is still pinned to the revision
actually serving traffic before running it.

### Pass 2 — run

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS}"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

Expect `MIGRATED` / `RESYNCED` / `REPAIRED REF` lines matching pass 1's plan, and:

```
Migrate-names complete: N migrated, N skipped (already migrated or absent), 0 failed, 0 legacy secrets deleted
```

Confirm `0 failed` and no `CONFLICT` lines before continuing. A `CONFLICT` here needs
diagnosis (§5) before you touch `--delete-legacy`.

### Before pass 3 — check the rollout

`--delete-legacy` is only safe once **every** replica of this hub is running a binary
that includes the hub-prefixed naming scheme — i.e. the rollout is 100% complete and no
older revision is still serving traffic. GCP Secret Manager has no conditional delete:
if an old binary is still a live writer to the legacy name, its write can land between
the safety check and the delete, and the delete destroys that write's only copy.

Run the rollout check above.

### Pass 3 — delete-legacy dry run

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--delete-legacy,--dry-run"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

Expect `WOULD DELETE LEGACY` lines (or `WOULD <action> AND DELETE LEGACY` if a ref
still needed repair) for every secret still holding a legacy copy, and:

```
Migrate-names dry run complete: N migrated, N skipped (already migrated or absent), 0 failed, 0 legacy secrets deleted
```

(legacy-deleted is always 0 in a dry run — that's what `--dry-run` means).

### Before pass 4 — repeat the same check

Run the rollout check above again. Do not skip this because you just did it for pass
3 — time has passed, and a deploy could have started.

### Pass 4 — delete-legacy

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--delete-legacy"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

Expect `DELETED LEGACY` lines matching pass 3's plan, and:

```
Migrate-names complete: N migrated, N skipped (already migrated or absent), 0 failed, N legacy secrets deleted
```

with the final N equal to pass 3's `WOULD ... DELETE LEGACY` count. A `CONFLICT` can
also appear here if a ref repair happens in this pass — handle it as in §5.

### Pass 5 — final verification (dry run)

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--delete-legacy,--dry-run"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

**Acceptance:** the summary reads

```
Migrate-names dry run complete: 0 migrated, N skipped (already migrated or absent), 0 failed, 0 legacy secrets deleted
```

and no line in the output starts with `WOULD`. `ORPHAN` lines may remain and don't
block acceptance. Only then is this hub done.

## 5. Reading the output

Job output goes to Cloud Logging, not your terminal. Get the execution name from the
last `execute` call's own output, or look it up:

```bash
EXECUTION=$(gcloud run jobs executions list --job "$JOB" --project "$PROJECT" --region "$REGION" \
  --sort-by=~metadata.creationTimestamp --limit=1 --format='value(metadata.name)')
```

Or list recent executions with the columns that actually carry data on a v1
Execution (`status.startTime`/`status.completionTime`, not top-level fields):

```bash
gcloud run jobs executions list --job "$JOB" --project "$PROJECT" --region "$REGION" \
  --format='table(metadata.name,status.succeededCount,status.failedCount,status.startTime,status.completionTime)'
```

Read that execution's logs. `logging read` needs an explicit `--freshness` — its
1-day default can silently miss an older execution:

```bash
gcloud logging read '
  resource.type="cloud_run_job"
  resource.labels.job_name="'"$JOB"'"
  resource.labels.location="'"$REGION"'"
  labels."run.googleapis.com/execution_name"="'"$EXECUTION"'"
' --project "$PROJECT" --order=asc --freshness=1d --format='value(textPayload)'
```

`succeededCount: 1` means the command exited 0. `failedCount: 1` means it exited
non-zero: either the summary shows `failed > 0` (look for `ERROR` / `CONFLICT`
lines), or there's no summary and the last log line is the fatal error (a config,
database, or hub-ID resolution error before the migration loop even starts, which has
no summary line of its own). `gcloud run jobs execute --wait` also exits non-zero in
that case.

The output vocabulary, taken from `cmd/hub_secret_migrate_names.go`:

| Line | When | Meaning |
|---|---|---|
| `Using hub ID: <id> (prefix: scion-<h12>-)` | every run | echo of the resolved ID and prefix — confirms the argument reached the command. With `--hub-id` passed explicitly, this just echoes the flag back; the real hub-ID guard is the env-var read in §2. |
| `WOULD MIGRATE` / `RESYNC` / `REPAIR REF` / `DELETE LEGACY` (joined with ` AND `) | dry runs | planned actions |
| `MIGRATED` / `RESYNCED` / `REPAIRED REF` | non-dry runs | copy / overwrite a stale prefixed copy / repoint the DB ref |
| `DELETED LEGACY` | non-dry with `--delete-legacy` | legacy copy deleted |
| `ORPHAN ... stored ref has no accessible value; nothing to migrate` | any | skipped, not a failure — there's nothing left to copy for that record |
| `CONFLICT ...` | non-dry runs only | counts as failed |
| `ERROR ...` | any | counts as failed |
| `Migrate-names complete: N migrated, N skipped (already migrated or absent), N failed, N legacy secrets deleted` | non-dry summary | |
| `Migrate-names dry run complete: ...` | dry-run summary (legacy-deleted is always 0) | |

**`CONFLICT`**: the command already wrote a version to the prefixed name and then
detected that a concurrent write had changed the record. The change may be a real ref
change (a true conflict) or only a version bump (a false positive). To diagnose: re-run
pass 2 (same args, no `--delete-legacy`). If it now prints `MIGRATED` / `RESYNCED` /
`REPAIRED REF` for that secret, the conflict is resolved. If it prints nothing for it,
re-set the secret to its intended value through the normal secret-set path. Don't go
on to pass 3 until this is resolved.

## 6. Cleanup

Confirm every pass above actually completed before deleting the job — once it's gone,
so is its execution history.

```bash
gcloud run jobs executions list --job "$JOB" --project "$PROJECT" --region "$REGION"
```

Then delete it (`--quiet` skips the interactive confirmation prompt):

```bash
gcloud run jobs delete "$JOB" --project "$PROJECT" --region "$REGION" --quiet
```

Confirm nothing is left:

```bash
gcloud run jobs list --project "$PROJECT" --region "$REGION" --filter="metadata.name=${JOB}"
```

This should return no rows. If it prints a `WARNING` about filter keys not being
present alongside "Listed 0 items", that's benign — it's `gcloud`'s way of saying the
filter matched nothing, not an error.

## 7. For Terraform-managed hubs: what can be removed afterward

Once the final dry run in §4 (pass 5) shows zero pending items for **every** hub
sharing a GCP project, the legacy IAM grant and the legacy pre-created secret that
only existed to bridge the old naming scheme can be removed, each as its own
per-resource acknowledgment rather than a single blanket change:

- the legacy conditioned `secretmanager.admin` (or equivalent) grant scoped to the
  hub's pre-migration secret-name prefix (`scion-hub-<hash>-*`) — see
  [Secrets: IAM Permissions and Secret Naming](https://scion-ai.dev/scion/hosted/user/secrets/#iam-permissions-and-secret-naming)
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
