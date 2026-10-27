# GitHub setup

This page takes you from nothing to a working cr-ci GitHub App. At the end the
App is installed and verified, and routing is ready but off until you set the
Actions variable (step 8).

**Current scope: the fork, `ptone/scion` (a personal account), only**
(ptone's decision, 2026-10-21). ptone can do every step on this page alone.
Upstream (`GoogleCloudPlatform/scion`, an organization) is out of scope for
now, because it needs a GoogleCloudPlatform org owner. Its notes are kept,
marked *future*, and the PAT-based alternative that is not implemented is in
[matrix 4.3](permissions-matrix.md#43-upstream-alternative-not-implemented-repo-webhook-plus-a-pat-from-a-repo-admin).

The roles each step needs, and where they differ between the two targets,
are in [permissions-matrix.md](permissions-matrix.md#1-github-steps). Step
numbers in brackets, such as (G4), refer to its rows. The GCP side
(bootstrap, image, job, mediator, scheduler) is in
[gcp-setup.md](gcp-setup.md). The order of both is in the
[README quick start](../README.md#quick-start).

**Secret handling, throughout this page.** The App private key, the webhook
secret and the one-hour manifest code are credentials. They only ever live:

- in a private temporary directory (`mktemp -d`, mode 0700, files 0600) that
  you delete at the end of the step; or
- in Secret Manager, written by `scripts/set-secrets.sh`, which reads them
  from a file or stdin and passes them to `gcloud` on stdin.

Never paste them into a chat, ticket, shell argument or commit.

## 0. Before you start

**Decisions.** Record them in `cr-ci/config.env`:

| Decision | Options | Guidance |
| :--- | :--- | :--- |
| Target (`TARGET_REPOS`) | **`ptone/scion`** | The current scope. Upstream (future) needs a GoogleCloudPlatform org owner for the install ([matrix 0](permissions-matrix.md#0-summary-for-the-fork-vs-upstream-decision)). |
| Installation scope (`GITHUB_APP_INSTALLATION_SCOPE`) | **`repo`** | The only scope a personal account has. `org` would be upstream only (future) and needs a runner group (G7). |
| App owner | **ptone's account**, private (`PUBLIC=false`) | To add upstream later, the same App can be made public, or the org can own its own App ([matrix 4.1](permissions-matrix.md#41-who-should-own-the-github-app-for-upstream)). |

**Tools.** A browser logged in as the App owner, plus `bash`, `jq`, `curl`
and `openssl`. `gh` (authenticated, with write access to each target repo) is
needed only from step 7 on. GCP access for the `scripts/` steps is described
in [gcp-setup.md](gcp-setup.md).

**Already done** (README quick start): `bootstrap-gcp.sh`,
`build-runner-image.sh` and `deploy-job.sh`. `cr-ci/config.env` holds
`TARGET_REPOS`, `GCP_PROJECT` and `REGION` (`GITHUB_APP_INSTALLATION_SCOPE`
defaults to `repo`, right for the fork).
Every script validates the whole config, and `GITHUB_APP_ID` is required, so
until the App exists `config.env` carries the placeholder `GITHUB_APP_ID=1`.
The GCP-side scripts do not use the value. Step 2.3 replaces it before
`deploy-mediator.sh`, which does use it.

## 1. Fix the mediator URL

The App needs the webhook URL before the mediator is first deployed, and the
mediator needs the App ID. Break the cycle with Cloud Run's deterministic
service URL,
`https://<MEDIATOR_SERVICE_NAME>-<PROJECT_NUMBER>.<REGION>.run.app`. It is
known before the first deploy, as long as that first DNS label is at most 63
characters ([Cloud Run URLs](https://docs.cloud.google.com/run/docs/triggering/https-request)):

```bash
cd cr-ci
project_number=$(gcloud projects describe "$GCP_PROJECT" --format='value(projectNumber)')
echo "MEDIATOR_URL=https://cr-runner-mediator-${project_number}.${REGION}.run.app"
```

Use your `MEDIATOR_SERVICE_NAME` in place of `cr-runner-mediator` if you
changed it. Put the printed line into `config.env` and check it with
`scripts/check-config.sh`. **The App's webhook URL is `MEDIATOR_URL` +
`/webhook`**, never the bare `MEDIATOR_URL`: the mediator answers deliveries to
`/` with 404 (seen in the P6 deploy, when a readiness note gave the bare URL).

`MEDIATOR_URL` is also the audience of every ID token the mediator accepts
(claims and `/reconcile`). The runners and the scheduler use whatever is in
`config.env`, so it only has to be *a* URL of the service, used
consistently. If `deploy-mediator.sh` later prints a different URL, either
keep this one (both URLs reach the service) or switch everything: update
`MEDIATOR_URL`, re-run `deploy-mediator.sh` and `deploy-scheduler.sh`, and
change the App's webhook URL (App settings, **General**, **Webhook URL**).

## 2. Create the GitHub App from the manifest (recommended)

[`scripts/github-app-manifest.json`](../scripts/github-app-manifest.json) is
the App definition:

- Repository **Administration**: read and write. Needed for
  `generate-jitconfig` and for deleting runner registrations.
- Repository **Actions**: read. Needed to read runs and jobs, and to receive
  `workflow_job`.
- Repository **Metadata**: read (mandatory).
- The `workflow_job` event, and nothing else.

These are exactly the matrix's `repo`-scope set
([section 1](permissions-matrix.md#1-github-steps)). A test
(`workflows/templates_test.go`) fails if the manifest gains or loses a
permission or event. The placeholders (`REPLACE-...`) are filled in below.
GitHub's reference: [Registering a GitHub App from a manifest](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest).

### 2.1 Prepare the form

Run this from `cr-ci/`, in a shell that holds `MEDIATOR_URL` (from step 1):

```bash
umask 077
work=$(mktemp -d)                      # private: 0700, deleted in 2.4
APP_NAME=scion-cr-ci-ptone             # must be unique across GitHub
HOMEPAGE=https://github.com/ptone/scion/tree/main/cr-ci
PUBLIC=false                           # true only if this one App must also be installed on another account (both targets)
# Personal account:
ACTION=https://github.com/settings/apps/new
# Org-owned App instead (future, upstream): ACTION=https://github.com/organizations/GoogleCloudPlatform/settings/apps/new

jq --arg name "$APP_NAME" --arg home "$HOMEPAGE" \
   --arg hook "${MEDIATOR_URL}/webhook" --argjson public "$PUBLIC" \
   '.name = $name | .url = $home | .hook_attributes.url = $hook | .public = $public' \
   scripts/github-app-manifest.json > "$work/manifest.json"

state=$(openssl rand -hex 16)
jq -r --arg action "$ACTION" --arg state "$state" \
   '@html "<!doctype html><title>cr-ci GitHub App</title><form action=\"\($action)?state=\($state)\" method=\"post\"><input type=\"hidden\" name=\"manifest\" value=\"\(tojson)\"><button type=\"submit\">Create the cr-ci GitHub App</button></form>"' \
   "$work/manifest.json" > "$work/create-app.html"
echo "open file://$work/create-app.html ; expected state: $state"
```

For `org` scope (future, upstream only, not recommended), change the permissions
before creating the form. Org *Self-hosted runners* replaces repo
*Administration*; *Actions* and *Metadata* stay
([matrix 1](permissions-matrix.md#1-github-steps)):

```bash
jq '.default_permissions |= (del(.administration) + {organization_self_hosted_runners: "write"})' \
   "$work/manifest.json" > "$work/m.json" && mv "$work/m.json" "$work/manifest.json"
```

Then re-run the `jq -r ... create-app.html` command.

### 2.2 Create the App in the browser

1. Open `create-app.html` in the browser where you are logged in as the App
   owner, and click **Create the cr-ci GitHub App**.
2. GitHub shows the new App's settings, pre-filled from the manifest. Check
   the name, the webhook URL and the permissions, then click **Create GitHub
   App for ...**.
3. GitHub redirects to
   `http://127.0.0.1:9/cr-ci-app-created?code=...&state=...`. Nothing listens
   there, so the browser shows a connection error. That is expected: the
   redirect target is deliberately local, so the code is never sent to any
   server. Check that `state` equals the value printed in 2.1, then copy the
   `code` value.

The code works for **one hour** and only once. Until it is used, anyone
holding it can fetch the App's private key, so treat it as a secret.

### 2.3 Exchange the code for the App ID, key and webhook secret

The conversion call needs no token
([REST: create a GitHub App from a manifest](https://docs.github.com/en/rest/apps/apps#create-a-github-app-from-a-manifest)).
The code is read without echo, and it reaches `curl` on stdin rather than in
its arguments:

```bash
read -rs -p 'manifest code: ' CODE; echo
printf 'url = "https://api.github.com/app-manifests/%s/conversions"\n' "$CODE" |
  curl -q -fsS -X POST -H 'Accept: application/vnd.github+json' \
       -H 'X-GitHub-Api-Version: 2022-11-28' -K - -o "$work/conversion.json"
unset CODE

jq -r '"App ID: \(.id)   slug: \(.slug)   owner: \(.owner.login)   \(.html_url)"' "$work/conversion.json"
jq -e '(.pem | type == "string") and (.webhook_secret | type == "string" and length > 0)' "$work/conversion.json" >/dev/null \
  || echo "conversion response lacks pem or webhook_secret: use the manual steps in section 3 for the missing part"
jq -j '.pem' "$work/conversion.json" > "$work/app-key.pem"
jq -j '.webhook_secret' "$work/conversion.json" > "$work/webhook-secret"   # -j: no trailing newline
rm -f "$work/conversion.json"          # it also holds an unused OAuth client secret
ls -l "$work"                          # both files must be -rw-------
```

- Put the App ID into `config.env` as `GITHUB_APP_ID=<id>`, replacing the
  placeholder `1`.
- The webhook secret must not end in a newline. The mediator uses the stored
  bytes as-is for the HMAC, and GitHub signs with the value exactly as
  generated. `jq -j` writes it without one.
- `client_id` and `client_secret` (OAuth) are not used by cr-ci.

### 2.4 Store the key and the webhook secret in Secret Manager

`scripts/set-secrets.sh` (P4) adds both as new versions of
`GITHUB_APP_PRIVATE_KEY_SECRET_NAME` and `GITHUB_WEBHOOK_SECRET_NAME`. It
refuses any file readable by group or others, and passes the values to
`gcloud` on stdin only. Like every script it defaults to a dry run; `--apply`
executes:

```bash
scripts/set-secrets.sh --app-key-file "$work/app-key.pem" --webhook-secret-file "$work/webhook-secret"          # dry run
scripts/set-secrets.sh --app-key-file "$work/app-key.pem" --webhook-secret-file "$work/webhook-secret" --apply
rm -rf "$work"
```

This needs `roles/secretmanager.admin`, or `roles/secretmanager.secretVersionAdder`
on the two secrets (C5, C9). The App does not need its webhook to answer yet:
deliveries before the mediator exists simply fail, and nothing depends on
them.

Now deploy the mediator and the scheduler ([gcp-setup.md](gcp-setup.md),
README quick start). `deploy-mediator.sh` reads `GITHUB_APP_ID` and
`MEDIATOR_URL` from `config.env`. Then continue with step 4.

## 3. Manual fallback: create the App in the UI

Use this if the manifest flow is not available, for example under an org
policy. The result must match the manifest exactly. Reference:
[Registering a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).

1. **Open the form.**
   - Personal account: profile picture, **Settings**, **Developer settings**,
     **GitHub Apps**, **New GitHub App**.
   - Org-owned App: org **Settings**, **Developer settings**, **GitHub Apps**,
     **New GitHub App**.
2. **GitHub App name:** unique across GitHub, for example
   `scion-cr-ci-ptone`. **Homepage URL:** the repo's `cr-ci/` folder.
3. **Identifying and authorizing users:** leave the callback URL empty.
   Leave **Request user authorization (OAuth) during installation**
   unchecked.
4. **Webhook:** **Active** checked. **Webhook URL:** `MEDIATOR_URL/webhook`.
   For **Webhook secret**, generate it with `set-secrets.sh`, which writes it
   once to a new 0600 file (no trailing newline) and stores the same bytes in
   Secret Manager:

   ```bash
   umask 077; work=$(mktemp -d)
   scripts/set-secrets.sh --generate-webhook-secret "$work/webhook-secret" --apply
   # Copy it to the clipboard without printing it, paste it into the form:
   pbcopy < "$work/webhook-secret" 2>/dev/null || xclip -selection clipboard < "$work/webhook-secret"
   # (No clipboard tool? cat the file, paste, then clear the terminal scrollback.)
   rm -f "$work/webhook-secret"
   ```

5. **Permissions, Repository permissions:** **Actions**: Read-only;
   **Administration**: Read and write; **Metadata**: Read-only (set
   automatically). Everything else: No access. For `org` scope instead:
   Repository **Actions** Read-only, **Metadata** Read-only, and
   **Organization permissions**, **Self-hosted runners**: Read and write.
6. **Subscribe to events:** **Workflow job** only.
7. **Where can this GitHub App be installed?** **Only on this account**,
   unless this one App must also be installed on another account (both
   targets): then **Any account**.
8. Click **Create GitHub App**. Note the **App ID** on the **General** page
   and put it into `config.env` as `GITHUB_APP_ID`.
9. **Private key:** on the same page, **Private keys**, **Generate a private
   key**. The browser downloads a `.pem` file, usually with mode 0644. Move
   it into the private directory, restrict it, store it, and delete it:

   ```bash
   mv ~/Downloads/<app-name>.*.private-key.pem "$work/app-key.pem"
   chmod 600 "$work/app-key.pem"
   scripts/set-secrets.sh --app-key-file "$work/app-key.pem" --apply
   rm -rf "$work"
   ```

   Also delete any other copy the browser kept (downloads history, trash).

## 4. Install the App

The mediator looks up the App's installation for every repository in
`TARGET_REPOS` (`/repos/{owner}/{repo}/installation`, or
`/orgs/{org}/installation` with `org` scope). Each target must therefore be
covered by an installation.

### 4.1 Fork: `ptone/scion` (repo scope) (G4)

ptone, as account owner:

1. Open the App's page (`https://github.com/apps/<slug>`, or App settings,
   **Install App**) and click **Install** next to the account.
2. Choose **Only select repositories**, then select `scion`.
3. Review the permissions (Actions read, Administration read and write,
   Metadata read) and click **Install**.

### 4.2 Future: `GoogleCloudPlatform/scion` (requires a GoogleCloudPlatform org owner)

Out of the current scope (fork only). Kept for when upstream is in scope. A
token-based alternative without an App, not implemented, is described in
[matrix 4.3](permissions-matrix.md#43-upstream-alternative-not-implemented-repo-webhook-plus-a-pat-from-a-repo-admin).

Only a GoogleCloudPlatform **organization owner** can install this App. A
repo admin cannot install an App that requests *Repository administration*
or any org permission; GitHub turns their attempt into an install request to
the org owners ([matrix X1, G4, G5](permissions-matrix.md#5-design-corrections-and-open-items)).

- If the App is owned by ptone's account, it must be public (`PUBLIC=true` in
  2.1, or **Any account** in section 3) so that it can be installed on the org.
- Send the org owner the ready-made request in
  [matrix section 6](permissions-matrix.md#6-request-to-a-googlecloudplatform-org-owner-future).
  It asks them to install the App on **Only select repositories**, `scion`,
  and to allow repository-level self-hosted runners for `scion` (G6).
- **`org` scope only:** the org owner installs the App on the org (G5), and
  configures a runner group with **Allow public repositories** limited to
  `scion` (G7). Its ID goes into `GITHUB_RUNNER_GROUP_ID`.
- After the installation, add `GoogleCloudPlatform/scion` to `TARGET_REPOS`
  and re-run `deploy-mediator.sh`.

## 5. Check the webhook

GitHub sends a `ping` event when the App is created and lets you redeliver
any delivery. The mediator checks the signature **before** it looks at the
event type, so a redelivered `ping` tests the URL and the webhook secret
end to end. It never starts a runner.

1. App settings, **Advanced**, **Recent Deliveries**. Open the `ping`
   delivery and click **Redeliver**. Later, when the ping is buried (after a
   webhook URL change), redeliver a `workflow_job` delivery whose action is
   `completed` or `in_progress`, never `queued`
   ([operations](operations.md#webhook-url-change)).
2. Read the response code:

   | Response | Meaning | Fix |
   | :--- | :--- | :--- |
   | `200`, body `ignored` | URL and secret are correct | none |
   | `401`, `bad signature` | Secret Manager and App settings hold different secrets (or a trailing newline) | Generate a new secret with `set-secrets.sh --generate-webhook-secret` and paste it into the App settings ([rotation](operations.md#secret-rotation)) |
   | `503`, `unavailable` | The mediator cannot read the webhook secret | Check that the secret has a version and that `MEDIATOR_SA_NAME` can access it (C6) |
   | `404` or connection error | Wrong webhook URL, or the mediator is not deployed | Compare the App's webhook URL with `MEDIATOR_URL/webhook` |

3. The mediator logs each delivery. For the ping:

   ```bash
   gcloud logging read --project "$GCP_PROJECT" --freshness=1h --limit=20 \
     'resource.type="cloud_run_revision" AND resource.labels.service_name="cr-runner-mediator" AND jsonPayload.event="webhook_ignored"' \
     --format='table(timestamp, jsonPayload.github_event, jsonPayload.reason, jsonPayload.delivery)'
   ```

   Expect `ping` / `event_type`. `jsonPayload.event="webhook_rejected"` with
   `reason=signature` is the 401 case. The full list of events is in
   [operations.md](operations.md#monitoring-and-alerts).

## 6. Deploy precondition: fork PR approval (G8)

This is **required** before step 8 on every target repository
([architecture: deploy preconditions](architecture.md#deploy-preconditions)).
It is what bounds the residual risk of a fork job reaching a cr-ci runner
([residual risk](architecture.md#residual-risk-label-matched-job-assignment)).

- **Repository** (repo admin; ptone on the fork):
  1. **Settings**, then in the sidebar **Actions**, **General**.
  2. Under **Approval for running fork pull request workflows from
     contributors**, select **Require approval for all external
     contributors**.
  3. Click **Save**.

  ([GitHub docs](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository))
- **Organization** (future, upstream only; org owner): org **Settings**, **Actions**,
  **General**, the same section and option. An org (or enterprise) policy
  overrides the repository setting
  ([GitHub docs](https://docs.github.com/en/organizations/managing-organization-settings/disabling-or-limiting-github-actions-for-your-organization)).
- The first two options ("first-time contributors ...") are **not** enough:
  anyone with one merged commit would bypass approval.
- **Check it in the UI.** The REST endpoint for this setting
  (`GET /repos/OWNER/REPO/actions/permissions/fork-pr-contributor-approval`)
  needs admin access: a personal access token without it gets 403 (seen in P6).
  The repo owner or an admin reads it on the settings page above.

The other preconditions are on the GCP and workflow side: the routing
expression with the fork guard (step 8), `max-instances=1` (set by
`deploy-mediator.sh`), and an `EXEC_SA_NAME` with no roles (set by
`bootstrap-gcp.sh`).

## 7. PoC run (before any routing)

The PoC exercises the whole path (webhook, fork guard, JIT runner, Cloud Run
execution, claim, job) before any CI job is routed. The template is
[`workflows/cr-runner-poc.yml`](../workflows/cr-runner-poc.yml). It runs from a
**push to a throwaway `cr-ci-poc-*` branch** that adds it under
`.github/workflows/`: `workflow_dispatch` would need the file on the default
branch, and the fork's main must mirror upstream. This is a P6 step and needs
ptone's go-ahead. The exact commands and the cleanup (branch and caches) are in
[workflows/README.md](../workflows/README.md#running-the-poc-a-push-triggered-throwaway-branch):

```bash
git push origin cr-ci-poc-1             # a branch whose only change is .github/workflows/cr-ci-poc.yml
gh run watch --repo ptone/scion "$(gh run list --repo ptone/scion --branch cr-ci-poc-1 --limit 1 --json databaseId -q '.[0].databaseId')"
```

What to check is listed in [step 9](#9-verify-a-routed-job). The complete P6
list, with every open runtime checkpoint and the PoC criteria, is
[poc-checklist.md](poc-checklist.md).

## 8. Route jobs: the Actions variable `SCION_LARGE_RUNNER` (G10)

Each routed job chooses its runner with this expression (in the workflows since
the primary-runner PRs; see [workflows/README.md](../workflows/README.md#routing-in-githubworkflows)):

```yaml
runs-on: ${{ (github.event_name == 'push' || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository)) && vars.SCION_LARGE_RUNNER || 'ubuntu-latest' }}
```

While `SCION_LARGE_RUNNER` (`ROUTING_VARIABLE_NAME`) is unset, every job runs
on `ubuntu-latest`. Setting it to `RUNNER_LABEL` sends pushes and same-repo
pull requests to cr-ci. Fork PRs and every other event stay on
`ubuntu-latest`. Setting it needs **write** access to the repository, not
admin ([matrix X2](permissions-matrix.md#5-design-corrections-and-open-items)).

Only after steps 4 to 7 hold for that repository:

```bash
scripts/set-routing-variable.sh --repo ptone/scion            # dry run: prints the gh command
scripts/set-routing-variable.sh --repo ptone/scion --apply    # sets SCION_LARGE_RUNNER=RUNNER_LABEL
```

This is equivalent to
`gh variable set SCION_LARGE_RUNNER --repo ptone/scion --body scion-cr-8c32g`.
In the UI (that page needs admin; `gh` and the script need only write):
**Settings**, **Secrets and variables**, **Actions**, the **Variables** tab,
**New repository variable**, then **Add variable**.
Without `--repo`, the script acts on every repository in `TARGET_REPOS`.

### Fallback toggle (G11)

To send every newly queued job back to `ubuntu-latest`, with no commit:

```bash
scripts/set-routing-variable.sh --repo ptone/scion --clear --apply
# same as: gh variable delete SCION_LARGE_RUNNER --repo ptone/scion
```

To use a larger GitHub-hosted runner instead (if the org has one, G18), set
the variable to that runner's label:
`gh variable set SCION_LARGE_RUNNER --repo <repo> --body <label>`.

The change applies to jobs queued **after** it. A job already queued on the
cr-ci label keeps waiting for a cr-ci runner, so cancel and re-run it
(`gh run rerun <run-id> --failed`, or **Re-run jobs** in the UI). For an
emergency stop that also cuts the mediator off from GitHub, suspend or
uninstall the App (G17): the App's installation page, **Suspend** or
**Uninstall**.

## 9. Verify a routed job

After the PoC dispatch, or after setting the variable and pushing to a
branch with an open same-repo pull request:

1. **GitHub:** the job leaves "Waiting for a runner" (P6b in us-west2: 17-18 s warm;
   P6 in us-central1: 40-67 s, mostly Cloud Run's start). **Settings**, **Actions**, **Runners** briefly shows
   `cr-job-<job id>`, and the runner disappears by itself after the job. Use the
   UI (repo admin) for this: the runners API
   (`gh api repos/OWNER/REPO/actions/runners`) needs repository Administration
   read, and a personal access token without it gets 403 (seen in P6). The
   mediator's `reconcile` lines (`runners_reaped`) show its own view.
2. **Mediator log:** a `dispatch` line for the job, then `claim_consumed`:

   ```bash
   gcloud logging read --project "$GCP_PROJECT" --freshness=1h --limit=50 \
     'resource.type="cloud_run_revision" AND resource.labels.service_name="cr-runner-mediator" AND jsonPayload.event=("dispatch" OR "claim_consumed" OR "claim_rejected" OR "capacity_full" OR "SECURITY_REJECT_FORK_PR" OR "webhook_ignored" OR "error")' \
     --format='table(timestamp, jsonPayload.event, jsonPayload.job_id, jsonPayload.runner, jsonPayload.reason)'
   ```

3. **Runner execution log:** the entrypoint logs the `/scratch` mount check
   and `claim succeeded on attempt 1` (the [runner README](../runner/README.md#runtime-contract)
   lists both as P6 checkpoints):

   ```bash
   gcloud logging read --project "$GCP_PROJECT" --freshness=1h --limit=50 \
     'resource.type="cloud_run_job" AND resource.labels.job_name="cr-gh-runner-8c32g"' \
     --format='value(timestamp, textPayload)'
   ```

4. **Not dispatched?** Use [operations.md, troubleshooting](operations.md#troubleshooting).
   The usual causes:
   - `labels_unservable`: the job's `runs-on` has labels beyond
     `RUNNER_LABEL`.
   - `webhook_rejected` with reason `repo_not_allowed`: the repository is not in
     `TARGET_REPOS`.
   - `capacity_full`: `MAX_CONCURRENT_RUNNERS` is reached.
   - No line at all: check the App's **Recent Deliveries** (step 5).

## 10. Removing cr-ci from a repository

1. Clear the routing variable (fallback toggle above).
2. Remove the repository from `TARGET_REPOS` and re-run `deploy-mediator.sh`.
3. Remove the repository from the App's installation (**Configure**,
   repository access), or uninstall the App.
4. When cr-ci is gone everywhere: delete the App (App settings, **Advanced**,
   **Delete GitHub App**), then run `scripts/teardown.sh` for the GCP side.
