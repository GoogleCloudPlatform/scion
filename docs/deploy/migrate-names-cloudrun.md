# Running migrate-names on Cloud Run hubs deployed with the terraform-ha modules

Run `scion hub secret migrate-names` against a Cloud Run hub deployed with the
hub-cloudrun Terraform module, using a one-off Cloud Run job.

`scion hub secret migrate-names` renames a hub's GCP Secret Manager secrets from the
legacy naming scheme to the hub-prefixed scheme (see
[Secrets: IAM Permissions and Secret Naming](https://scion-ai.dev/scion/hosted/user/secrets/#iam-permissions-and-secret-naming)).
The command opens the hub's own database directly from a DSN.

**Scope:** this runbook is for Cloud Run hubs deployed with the hub-cloudrun
Terraform module (one of the terraform-ha modules). Private-IP Cloud SQL, Direct VPC
egress, and an explicit `hub_id` are prerequisites this module provides
unconditionally: the terraform-ha Cloud SQL module (`cloudsql-instance`) hard-codes
`ipv4_enabled = false`, so there is no public-IP variant of this module to support.
The DSN is delivered as the `SCION_SERVER_DATABASE_URL` secret env var, and
`settings.yaml` is mounted from a secret volume. No hub in this scope is ever
operated from a workstation for this: this job is the only supported way to run
`migrate-names` against it. Cloud Run hubs not deployed by this module — including
ones wired up by hand — are not covered by this runbook; see
[ptone/scion#2395](https://github.com/ptone/scion/issues/2395) for the tracked gap.

Hubs deployed with the manual
[Deploy on GCP](https://scion-ai.dev/scion/hosted/ha/setup-gcp/#7b-secret-name-migration)
guide are **not** covered by this runbook. That guide's hub uses a public-IP Cloud
SQL instance with no Direct VPC egress, and keeps its DSN inside `settings.yaml`
rather than a separate secret env var — none of which this runbook's discovery
steps assume.

This runbook avoids handing the database credential to a human, and works around
the lack of any network path from a workstation to the private-IP Cloud SQL
instance, with a **one-off Cloud Run job**: an ephemeral job that borrows the
running service's own identity, VPC wiring and Cloud SQL connection to reach the
database, runs the migration, and is deleted afterward. It is **not**
Terraform-managed — there is no
`google_cloud_run_v2_job` resource for this. The job is created ad hoc with `gcloud`,
used for the five migration passes below, and deleted. A permanent resource isn't
worth it for a one-time migration that `--delete-legacy` (and a later removal of the
legacy IAM grant) retires anyway.

No human ever sees the database credential: the DSN stays a Secret Manager reference
passed to the job with `--set-secrets`, the same way it reaches the real service.

Every command below uses placeholders — `PROJECT`, `REGION`, `HUB` —
and every real value is *discovered* from the live service with `gcloud`, not typed in
by hand. Do not fill in a real project ID or hub name in this page.

## What the job mounts, and why

The job must mirror enough of the hub-cloudrun Terraform module's service definition
to reach the same database as the same identity — but not more than that. Checked
against `cmd/hub_secret_migrate_names.go`:

| Service has it for... | migrate-names needs it? | Included in the job? |
| :--- | :--- | :--- |
| The service's runtime service account | Yes — it's the identity used for both Cloud SQL and Secret Manager access; no new IAM is created for the job. | Yes, `--service-account` |
| Direct VPC egress (network/subnet, `PRIVATE_RANGES_ONLY`) | Yes — every hub in this runbook's scope has a private-IP-only Cloud SQL instance, reachable only from inside the VPC. | Yes, `--network`/`--subnet`/`--vpc-egress` |
| `/cloudsql` Cloud SQL volume | Yes — the DSN embeds `?host=/cloudsql/<connection name>`; without the volume the socket path doesn't exist and the connection fails. | Yes, `--set-cloudsql-instances` |
| `SCION_SERVER_DATABASE_URL` secret env (the DSN) | Yes — this is the only way the command opens the database. | Yes, `--set-secrets` |
| `settings.yaml` secret, mounted as a file | Yes, but only for one field: **`server.database.driver: postgres`**. Without the settings file (or `SCION_SERVER_DATABASE_DRIVER`), the driver resolves to sqlite — `openMigrateNamesStore`'s `switch` treats an empty/default driver as `"sqlite"` (the legacy loader's own default), so the job would silently try to open the Postgres DSN as a sqlite file. `--config` pointed at the mounted file is what makes the command see `driver: postgres`. Its `server.hub.hub_id` is not read when `--hub-id` is passed. | Yes, `--set-secrets`, mounted at `/run/secrets/settings.yaml` |
| `SCION_SERVER_HUB_HUBID` env var | Not required once `--hub-id` is passed explicitly (see below) — harmless; not read when `--hub-id` is passed. | Yes, `--set-env-vars` |
| `SCION_SERVER_SESSION_SECRET` (session secret) | No — migrate-names never touches sessions, and `secret.NewGCPBackend` doesn't consume it. | **No** |
| Kubeconfig secret / `KUBECONFIG` env / `SCION_K8S_NAMESPACE` | No — migrate-names never talks to Kubernetes. | **No** |
| NFS volume | No — migrate-names does no workspace I/O. | **No** |
| `HOME`, `SCION_REQUIRE_STABLE_SIGNING_KEY` | No, but `HOME` still matters: `LoadGlobalConfig` reads `$HOME/.scion/settings.yaml` **before** `--config`'s directory, and if `$HOME` can't be resolved it skips settings.yaml entirely. Leaving `HOME` out is safe only because the image sets `ENV HOME=/home/scion` and the job bypasses `entrypoint.sh`, so nothing gets copied into `~/.scion` that could shadow `--config`. | **No** |

Mounting only what's needed keeps the job's blast radius to exactly what the CLI reads:
the DSN and the driver.

## 1. Prerequisites

### Tools

`gcloud` (authenticated as the operator) and `jq` — 1.7 or newer is recommended
(Debian 12 and Ubuntu 22.04 still ship 1.6). The §2 guard and §4 gate below are
written to fail closed on either version; the version bump alone isn't a substitute
for those guards.

Run every command in this runbook with **bash**, not `zsh` — zsh's `echo` can mangle
embedded JSON (its handling of backslashes and quoting differs from bash's), which
breaks the `jq` pipelines in §2. If you're in an interactive zsh shell, start a bash
subshell first (`bash`) before pasting any of the commands below.

### Operator IAM

The operator needs, on the project (or a custom role with just these permissions):

- `run.jobs.create`, `run.jobs.get`, `run.jobs.update`, `run.jobs.run`,
  `run.jobs.delete`, `run.jobs.list`, `run.executions.get`, `run.executions.list`,
  `run.revisions.get`, `run.services.get`, `run.services.update` — to create,
  configure, execute, read the results of, and clean up the job (the
  executions/revisions permissions cover `execute --wait`, §5's executions list,
  §6's cleanup, and §2's revision lookup); `run.services.get` for §2's discovery
  and the §4 gate; `run.services.update` for `update-traffic` (tag removal in §4,
  and §8's roll-back and roll-forward). `roles/run.developer` covers all of these.
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
SVC_STATUS=$?
```

`$SVC_STATUS` is checked below, at the guard — a failed `describe` (token expiry,
network, a typo in `$HUB`) must not be swallowed by the command substitution.

**Service account:**

```bash
SA=$(echo "$SVC" | jq -r '.spec.template.spec.serviceAccountName')
```

**Network, subnet, VPC egress** (Direct VPC egress is expressed as annotations on the
revision template — every hub in scope has them, since the module's Cloud SQL
instance is always private-IP):

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
pinned, non-latest traffic is handled correctly too. This is the same guard §4 repeats
before passes 2, 3 and 4 — defined once here, as `ROLLOUT_OK_JQ`, and reused verbatim
there so the two checks can't drift apart:

```bash
REVISION=$(echo "$SVC" | jq -r '.status.traffic[] | select(.percent==100) | .revisionName')
ROLLOUT_OK_JQ='
    (.status.traffic | length == 1)
    and .status.traffic[0].percent == 100
    and .status.traffic[0].revisionName == $rev
    and ((.status.traffic[0].tag // "") == "")
    and .status.latestReadyRevisionName == $rev
    and .status.latestCreatedRevisionName == $rev
    and .metadata.generation == .status.observedGeneration'
[ "$SVC_STATUS" -eq 0 ] && [ -n "$SVC" ] && [ -n "$REVISION" ] && [ -n "$ROLLOUT_OK_JQ" ] \
  && echo "$SVC" | jq -e --arg rev "$REVISION" "$ROLLOUT_OK_JQ" >/dev/null \
  || echo "STOP: services describe failed or returned nothing, no revision has 100% of traffic, a deploy is in progress or failed, or a traffic tag exists; remove any tag (§4) or see §8 step 3, then restart §2"
REV_JSON=$(gcloud run revisions describe "$REVISION" --project "$PROJECT" --region "$REGION" --format=json)
IMG=$(echo "$REV_JSON" | jq -r '.status.imageDigest')
```

Every other discovered value above (`SA`, network/subnet/egress, `CONN`, `HUB_ID`, the
DSN version, the settings version) comes from `$SVC.spec.template` — the newest
revision's template — while `REVISION`/`IMG` come from whichever revision has 100% of
traffic. Those are normally the same revision, but two things can split them: a
traffic-only rollback to an older revision — including §8 step 2's, which is a
temporary mitigation, not a completed revert, and stops here until §8 step 3 makes the
latest revision serve 100% again — and a deploy whose new revision fails to become
ready or hasn't finished rolling out yet (`latestCreatedRevisionName` differs from
`latestReadyRevisionName`, or `metadata.generation` is ahead of
`status.observedGeneration`). In both cases, `spec.template` belongs to a revision
that isn't the one serving traffic, so the job would run one revision's binary
against another revision's settings and DSN versions. Separately, the check also
stops on any traffic tag, on any revision — not because a tag splits `spec.template`
from the traffic revision, but because a tag on an older revision keeps a
legacy-name writer live at that tag's own URL (see §4). The check above stops all of
these cases before it reaches job creation; if it prints `STOP`, remove any traffic
tag (§4) or see §8 step 3 to resolve the rollout, then restart from the top of this
section before continuing.

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
to read a secret value, which this whole procedure is designed to avoid.

## 3. Create the job

`--network`/`--subnet`/`--vpc-egress` are always required in this runbook's scope,
since the module's Cloud SQL instance is private-IP only.

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
Secret Manager methods. Passes 1 and 5 need no gate. Pass 3 is a dry run too, but it's
gated anyway, because its plan is meaningful only under the same precondition that
governs pass 4.

```bash
JOB="${HUB}-migrate-names"
BASE_ARGS="hub,secret,migrate-names,--gcp-project=${PROJECT},--hub-id=${HUB_ID},--config=/run/secrets/settings.yaml,--global"
```

`--global` is required: the root command's `PersistentPreRunE` demands a scion
project for `hub secret ...` subcommands unless `--global` is passed, and this job's
container has none. `migrate-names` never uses a project path (it resolves everything
through `--config`/`LoadGlobalConfig`), so `--global` is safe here. (Whether
`hub secret` subcommands should be exempted from that requirement outright is
tracked separately in
[ptone/scion#2396](https://github.com/ptone/scion/issues/2396).)

The **rollout check** below re-appears before passes 2, 3, and 4 — it always fetches
live state, so it can't be fooled by a stale `$SVC` captured back in §2, and it reuses
`$ROLLOUT_OK_JQ` from §2 verbatim, so this gate and that guard can't disagree. It also
fetches the job's own image and compares it against the serving revision's digest, so
the check doesn't depend on the shell's `$REVISION` still being the current one:

```bash
FRESH_SVC=$(gcloud run services describe "$HUB" --project "$PROJECT" --region "$REGION" --format=json) \
  && JOB_IMG=$(gcloud run jobs describe "$JOB" --project "$PROJECT" --region "$REGION" \
       --format='value(spec.template.spec.template.spec.containers[0].image)') \
  && [ -n "$FRESH_SVC" ] && [ -n "${REVISION:-}" ] && [ -n "${ROLLOUT_OK_JQ:-}" ] && [ -n "$JOB_IMG" ] \
  && echo "$FRESH_SVC" | jq -e --arg rev "$REVISION" "$ROLLOUT_OK_JQ" >/dev/null \
  && [ "$JOB_IMG" = "$(gcloud run revisions describe "$REVISION" --project "$PROJECT" --region "$REGION" \
       --format='value(status.imageDigest)')" ] \
  && echo "OK: 100% on $REVISION, job image matches" \
  || echo "STOP: services/jobs describe failed or returned nothing, REVISION/ROLLOUT_OK_JQ/JOB_IMG are empty, rollout not complete, revision changed, traffic is pinned to a non-latest revision, a traffic tag exists, or the job's image no longer matches the serving revision's digest; remove any traffic tag (see below), otherwise see §8 step 3 — delete the job and restart from §2, don't hand-patch the image"
```

The `[ -n "$FRESH_SVC" ]`, `[ -n "$REVISION" ]`, and `[ -n "$ROLLOUT_OK_JQ" ]` checks
keep this gate fail-closed on every `jq` version: jq 1.6 treats a missing or empty
program as `.` and exits 0 under `-e` even with no input, so without these checks a
failed `describe` or an empty `$REVISION`/`$ROLLOUT_OK_JQ` would print `OK` instead of
`STOP`. The `[ -n "$JOB_IMG" ]` check instead guards the string comparison below it: an
empty value must never compare as a match, so without it a failed `jobs describe` (empty
`$JOB_IMG`) alongside a failed `revisions describe` (empty `status.imageDigest`) would
compare `"" = ""` and print `OK`. That comparison makes the gate check the job's image
instead of trusting the shell's `$REVISION`: the job resource outlives the shell, so a
lost-and-resumed session (a new terminal, a Cloud Shell timeout, or leaving the `bash`
subshell §1 asks for) can otherwise re-derive `REVISION` from live traffic while the job
stays pinned to an older digest — the gate would then check the wrong revision against
the right one and print a false `OK`. The gate checks only the job's image, though, not
its secret versions, Cloud SQL connection or network, so it can't detect a config-only
revision with the same image digest — a DSN secret-version bump or a settings change
through Terraform, with no new build — which is why, if you lose §2's shell, you must
not re-derive its variables by hand in a new shell: go straight to §8 step 3, delete the
job, and restart from §2. If `REVISION`, `ROLLOUT_OK_JQ`, or `JOB_IMG` come back empty,
or the job's image doesn't match the serving revision's digest — see §8 step 3: delete
the job and restart from §2. Don't hand-patch the job's image to make it match; the
whole point of the digest pin (§2) is that the job runs the exact binary the service
does, and hand-patching only hides a mismatch that means something else already went
wrong.

You want exactly one traffic entry, at 100%, on the revision whose digest you pinned
the job to in §2, and that revision must also be both `latestReadyRevisionName` and
`latestCreatedRevisionName`, with `metadata.generation` equal to
`status.observedGeneration` — otherwise the job's config (read from the service
template) and its image (read from the traffic revision) could belong to two different
revisions: a rollback with traffic pinned to an older revision, or a new deploy that
failed to become ready or hasn't finished rolling out yet.

The check also stops while **any** traffic tag is present on **any** revision, even
the one serving 100% — the `length == 1` and empty-`tag` terms both require it. A tag
on an older revision keeps it serving at its own tag URL — a live legacy-name writer,
exactly what `--delete-legacy` must rule out. The check doesn't distinguish by
revision, so a tag on the serving revision also stops it; remove either kind. Find a
tag with `echo "$FRESH_SVC" | jq '.status.traffic'`, and remove it with
`gcloud run services update-traffic "$HUB" --project "$PROJECT" --region "$REGION"
--remove-tags=<tag>`, then re-run the check.

For any other `STOP`, see §8 step 3 to resolve the rollout and re-pin the job; it
deletes any leftover job and restarts from §2 once traffic agrees again. Never run a
non-dry pass from a job whose digest isn't the 100%-traffic revision's — retrying
later from the same job won't help, since it's still pinned to the old digest.

### Pass 1 — dry run

```bash
gcloud run jobs update "$JOB" --project "$PROJECT" --region "$REGION" \
  --args="${BASE_ARGS},--dry-run"
gcloud run jobs execute "$JOB" --project "$PROJECT" --region "$REGION" --wait
```

Read the output (§5). Expect `WOULD MIGRATE` / `WOULD RESYNC` / `WOULD REPAIR REF`
lines for anything not yet on the hub-prefixed name, and a summary:

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

`--delete-legacy` is only safe once **every** replica of this hub is running a
binary that includes the hub-prefixed naming scheme — i.e. the rollout is 100%
complete and no older revision is still serving traffic. GCP Secret Manager has no
conditional delete:
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

and no `WOULD` lines appear. `ORPHAN` lines may remain and don't
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

Read that execution's logs. `gcloud logging read` only looks back 1 day by default; if
the execution is older, raise `--freshness` (e.g. `7d`):

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
lines), or look for the `Error: <message>` line near the end (wrapped in terminal
colour codes in the log, so a literal search for a line starting `Error:` misses it).
The command's usage text follows that line only for an argument or flag error (for
example a missing `--gcp-project`); a runtime failure prints just the error
(`cmd/root.go`, ptone/scion#2859). `gcloud run jobs execute --wait` also exits non-zero
in that case.

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
| `Error: migrate-names finished with N failure(s); re-run to retry (idempotent)` | non-dry or dry run with `failed > 0` | follows the summary; see the `ERROR`/`CONFLICT` lines above it |

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

The `terraform-ha` modules no longer carry the legacy conditioned
`secretmanager.admin` grant or the legacy pre-created OIDC signing key secret —
both were scoped to the hub's pre-migration secret-name prefix,
`scion-hub-<h>-*`, where `<h>` is the first 12 hex characters of
`sha256("<hub_id>:<hub_id>")` (`legacyGCPSecretName`, `pkg/secret/gcpbackend.go`).
See
[Secrets: IAM Permissions and Secret Naming](https://scion-ai.dev/scion/hosted/user/secrets/#iam-permissions-and-secret-naming)
for how the hub-prefixed replacement is computed. A fresh hub now generates and
stores its own OIDC signing key on first boot under the hub-prefixed name
instead of reading a Terraform-pre-created one.

**Run this runbook through pass 4 (`--delete-legacy`) and confirm pass 5's
"zero pending" result for every hub sharing a GCP project *before* upgrading
that project's Terraform to a module version without the legacy grant.** Pass
4 needs the legacy grant to delete the legacy-named secrets; once the grant is
gone, `migrate-names` can no longer reach them at all. Applying this version
after passes 1-3 but before pass 4 is not destructive: the remaining
legacy-named secrets just can no longer be deleted by `migrate-names` (which
runs as the hub SA) and must be removed by hand with `gcloud secrets delete`.
Applying it before passes 1-3 is not safe: Terraform deletes the legacy OIDC
secret on apply, and on an unmigrated hub that secret is the only copy, so any
record whose `SecretRef` still points at the legacy name starts failing with
`PermissionDenied`.

**Expected plan delta after `--delete-legacy` (pass 4) has already run:** for
each existing hub, applying this version should show the legacy
`secretmanager.admin` grant and `tls_private_key.oidc_signing_key` destroyed;
the OIDC secret and its version dropped from state on refresh (already
deleted out-of-band by pass 4, so they show as "changed outside of Terraform",
not as a destroy); the Cloud Run service updated in place (the legacy env var
removed); and `time_sleep.iam_propagation` replaced. This version also
replaces `module.agent_runtime_k8s.kubernetes_job_v1.nfs_init` once, on each
existing hub's first apply of it: the Job moves from a fixed name to a
generated name, and both are ForceNew, so Terraform create-before-destroys
the Job; the new Job re-runs the same idempotent mkdir/chown as the one it
replaces. That same apply updates
`module.hub_cloudrun.terraform_data.boot_prerequisites` in place, because its
`nfs_init_job` input is only known after the replacement Job is created. The
only add in this plan is that replacement Job (a replace counts as one add
and one destroy); nothing else is added. If
the OIDC secret/version show up as a destroy instead, pass 4 has not actually
deleted them yet — stop and confirm pass 5 first.

## 8. Break-glass

No human ever fetches the DSN or the settings secret outside this job — that
includes when something goes wrong. There is no bastion or workstation fallback that
reads either of them. If the Cloud Run job approach is unavailable (for example,
`gcloud` job creation is blocked by an org policy) or a pass fails in a way this
runbook doesn't cover:

1. **Stop.** Do not improvise a workaround that reads the DSN or the settings
   secret's contents from a workstation, a bastion, or anywhere outside this job.
2. **Roll traffic back** to the prior revision if the currently-serving revision is
   implicated: `gcloud run services update-traffic "$HUB" --project "$PROJECT"
   --region "$REGION" --to-revisions=<prior-revision>=100`. This pins traffic to a
   non-latest revision on purpose, so §2's rollout guard will reject it by design
   (the job's config and image would otherwise come from two different revisions) —
   do not restart from §2 yet.
3. **Resolve the rollout, then re-run from §2.** First check whether a job is left
   over from the interrupted attempt — `gcloud run jobs describe
   "${HUB}-migrate-names" --project "$PROJECT" --region "$REGION"` — and only if it
   exists, delete it and confirm it's gone (§6: `gcloud run jobs delete
   "${HUB}-migrate-names" --project "$PROJECT" --region "$REGION" --quiet`, then
   `gcloud run jobs list --project "$PROJECT" --region "$REGION"
   --filter="metadata.name=${HUB}-migrate-names"` should return no rows). Then clear
   whatever caused the STOP:
   - A **traffic pin from step 2**, or an unfinished rollout of an otherwise healthy
     revision: roll forward — `gcloud run services update-traffic "$HUB" --project
     "$PROJECT" --region "$REGION" --to-latest`.
   - A **deploy still in progress** (`metadata.generation` ahead of
     `status.observedGeneration`): wait for it to finish, then re-run §2's check and
     handle whichever case applies, if any — it may now pass with nothing to do, or
     turn out to be the roll-forward case above or the failed-deploy case below.
   - A **failed deploy** (`latestCreatedRevisionName` != `latestReadyRevisionName`):
     `--to-latest` won't clear this, since traffic already sits on the latest
     *ready* revision. Redeploy a known-good image as a new revision first, through
     the module's normal Terraform apply, then roll forward as above.
   - A **traffic tag**: remove it — `gcloud run services update-traffic "$HUB"
     --project "$PROJECT" --region "$REGION" --remove-tags=<tag>`.
   - A **job-image mismatch** or lost shell state with traffic otherwise converged:
     nothing more to clear; deleting the job above is the fix.

   Resolve the underlying blocker (org policy, IAM, quota) first if that's what's
   stopping the redeploy or roll-forward. The module declares no `traffic` block, so a
   Terraform apply alone leaves step 2's pin in place — it doesn't reset traffic to
   the new revision on its own, which is why `--to-latest` still follows it. Then
   restart from §2 once its check prints no STOP (no traffic tags, latest ready
   revision at 100%, generation == observedGeneration); §2 creates a fresh job
   re-pinned to whatever revision is now serving.
4. **Escalate** to the project owner if the job still cannot be created or run. No
   command in this procedure, and no ad hoc substitute for it, may read the DSN or
   the settings secret outside this job.
