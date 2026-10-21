# cr-ci permissions matrix

This page lists every setup and operations step for cr-ci with the minimum
permission it needs. GitHub steps are split by target:

- **Fork:** `ptone/scion`, a repository owned by a **personal account**.
- **Upstream:** `GoogleCloudPlatform/scion`, a repository owned by an **organization**.

GCP steps do not depend on which GitHub repository is served. They depend on
who controls the GCP project named by `GCP_PROJECT`.

The page is written so that someone who holds the rights can carry out a step
without reading the rest of cr-ci. A ready-to-send request for a
GoogleCloudPlatform org owner is at the end ([section 6](#6-request-to-a-googlecloudplatform-org-owner-future)).

- **Status:** complete for P1. GitHub requirements were checked against
  docs.github.com on 2026-10-21. For GCP, the Cloud Run, Cloud Build and Cloud
  Run Jobs limits pages (GC1, GC2, GC7, GC11) were read on the same day. The
  other GCP role names (GC3-GC6, GC8-GC10) are standard predefined roles that
  were not re-fetched; the P4 dry-run transcripts check them. The
  sources are cited inline as `[GHn]` (GitHub) and `[GCn]` (GCP) and listed in [section 7](#7-sources).
  Anything still unverified is marked **UNVERIFIED**, with the check that would
  settle it.
- **Names:** resource names are the defaults in `config.example.env`, written as
  the variable name with its default in brackets, for example
  `MEDIATOR_SA_NAME` (`cr-runner-mediator-sa`).
- **Source design:** [cr-runners design](https://storage.cloud.google.com/scion-xproject-exchange/cr-runners/design.md)
  (`gs://scion-xproject-exchange/cr-runners/design.md`), in particular sections
  3 and 7. Where this page and the design disagree, this page is the verified
  one. [Section 5](#5-design-corrections-and-open-items) lists the differences.

## 0. Summary for the fork-vs-upstream decision

> **Decision (ptone, 2026-10-21): cr-ci is installed on `ptone/scion` only.**
> Upstream (`GoogleCloudPlatform/scion`) is out of scope for now, because the
> org permissions it needs are likely not available. The upstream columns and
> sections below are kept for the future; they require a GoogleCloudPlatform
> org owner. [Section 4.3](#43-upstream-alternative-not-implemented-repo-webhook-plus-a-pat-from-a-repo-admin)
> describes the PAT-based alternative that would avoid the org owner, and why
> it is not implemented.

| Question | Fork (`ptone/scion`) | Upstream (`GoogleCloudPlatform/scion`) |
| :--- | :--- | :--- |
| Can ptone do every GitHub step alone? | **Yes.** ptone owns the account, so ptone can create the App, install it, set the variable and change the Actions settings. | **No.** Installing an App that requests *Repository administration* needs an **org owner** [GH3]. A repo admin cannot do it. Org-level Actions policy (whether repo-level runners are allowed, plus fork approval overrides) is also org-owner territory [GH9]. |
| Installation scope (`GITHUB_APP_INSTALLATION_SCOPE`) | `repo` only. Personal accounts have no org-level runners or runner groups [GH8]. | `repo` (recommended) or `org`. Either way an org owner must install the App. `org` also needs a runner group that allows public repositories [GH8]. |
| Who owns the App (holds the private key, sets the webhook URL and secret)? | ptone. | Either ptone (an App owned by a personal account and made installable on "Any account" [GH2]) or the GoogleCloudPlatform org (created by an org owner or a GitHub App manager [GH2][GH4]). See [4.1](#41-who-should-own-the-github-app-for-upstream). |
| Switching the routing variable (rollout and fallback) | ptone (owner or collaborator) [GH7]. | Anyone with **write** on the repo [GH7][GH12]. Admin is not needed. |
| GCP steps | Same for both targets: the owner of the chosen GCP project, or the least-privilege set in [2.1](#21-deployer-roles-least-privilege-bundle). The org-policy caveat (item U6) depends on where `GCP_PROJECT` lives, not on the GitHub target. | Same as the fork |

## 1. GitHub steps

The **App permissions** column is what the App registration must request. The
**human role** columns are what the person performing the step needs.

App permission set, depending on `GITHUB_APP_INSTALLATION_SCOPE`:

- **`repo`:** Repository *Actions*: read; Repository *Administration*: read and
  write; Repository *Metadata*: read (mandatory). No organization permissions.
  Subscribed event: `workflow_job`.
- **`org`:** Repository *Actions*: read; Repository *Metadata*: read;
  Organization *Self-hosted runners*: read and write. No Repository
  *Administration*. Subscribed event: `workflow_job`.

Why each one is needed:

- `generate-jitconfig`, delete runner and list runners need *Administration*
  (repo) or *Self-hosted runners* (org) [GH1][GH5].
- Reading runs and jobs for the fork guard and reconcile needs *Actions*: read
  [GH1][GH6].
- Subscribing to `workflow_job` also needs *Actions*: read [GH10].

| # | Step | Fork (`ptone/scion`): who can do it | Upstream (`GoogleCloudPlatform/scion`): who can do it | API / App permission | Sources |
| :-- | :--- | :--- | :--- | :--- | :--- |
| G1 | **Create (register) the GitHub App** with the permission set above | ptone, under the personal account. Anyone can register an App on their own account. | **Either** an org owner or GitHub App manager, for an org-owned App, **or** ptone, for a personal-account App with "Any account" installability ([4.1](#41-who-should-own-the-github-app-for-upstream)) | n/a (UI, or the manifest flow in P5) | [GH2][GH4] |
| G2 | **Generate and download the App private key**, and store it in Secret Manager (step C5) | App owner (ptone) | Owner of the App: an org owner or App manager if org-owned, ptone if personal | n/a | [GH2][GH4] |
| G3 | **Set the App webhook URL and webhook secret** (mediator URL + `/webhook`; the secret goes to Secret Manager, step C5) | App owner (ptone) | App owner or App manager. **Not** the repo or org admin of the installation target, unless they also own the App. | n/a. App settings, not repo settings. | [GH10][GH11] |
| G4 | **Install the App on the repository** (`repo` scope; select only the target repo) | ptone. "Anyone can install GitHub Apps on their personal account." | **Org owner only.** Repo admins can install only Apps that request neither org permissions nor *Repository administration*; this App requests the latter. Anyone else who tries sends the org owner an install request instead. | Installation grants the permission set above for the selected repos only | [GH3][GH13] |
| G5 | **Install the App on the organization** (`org` scope) | **Not possible.** Personal accounts have no org-level runners. | **Org owner only.** The App requests an organization permission. | Org *Self-hosted runners*: read and write | [GH3][GH5][GH8] |
| G6 | **Allow repo-level self-hosted runners** for the repo (`repo` scope) | Nothing to do. Personal accounts have no such policy. | **Org owner.** Org Settings, Actions, General, *Runners*: "All repositories" or "Selected repositories" including scion. When it is "Disabled", repo-level runners (JIT runners included) cannot be created. An enterprise policy can override this (enterprise owner). | n/a | [GH9] |
| G7 | **Runner group for org runners** (`org` scope): enable *Allow public repositories* on the group the JIT runners join, and allow the scion repo | **Not applicable** | **Org owner.** By default "only private repositories can access runners in a runner group"; the public scion repo needs the override. Every org has the Default group. Extra groups need the GitHub Team plan or above. | REST: org *Self-hosted runners*: write | [GH8] |
| G8 | **Fork PR approval setting**: set "Require approval for all external contributors" | ptone, as repo owner (Settings, Actions, General) | **Repo admin** (UNVERIFIED, see U4), unless an **org owner** (or enterprise owner) has set an overriding policy, in which case they must set it | n/a | [GH9][GH12][GH14] |
| G9 | **Approve an individual fork-PR workflow run** (day to day) | ptone or any collaborator with write | Anyone with **write** access ("maintainers with write access") | n/a | [GH15] |
| G10 | **Set the Actions variable** `ROUTING_VARIABLE_NAME` (`SCION_LARGE_RUNNER`) to `RUNNER_LABEL`, for example `gh variable set SCION_LARGE_RUNNER --repo OWNER/REPO --body scion-cr-8c32g` | ptone (personal repos: owner or collaborator) | Anyone with **write**. Admin is **not** required. | REST: Repository *Variables*: write | [GH7][GH12] |
| G11 | **Fallback toggle**: delete the variable, or set it to `ubuntu-latest` (or a larger-runner label) | Same as G10 | Same as G10 (write) | Same as G10 | [GH7] |
| G12 | **Org-level variable** instead of a repo variable (optional) | Not applicable | Org owner | Org *Variables*: write | [GH7] |
| G13 | **Repository webhook instead of an App webhook** (not recommended; the App is still needed for runner tokens) | ptone (repo owner or admin) | Repo admin (org webhook: org owner) | Repo *Webhooks*: write | [GH11] |
| G14 | **Rotate the webhook secret**: change it in the App settings, then add a new Secret Manager version | App owner + C9 | App owner or App manager + C9 | n/a | [GH10] |
| G15 | **Rotate the App private key**: generate a new key, add a Secret Manager version, then delete the old key in the App settings | App owner + C9 | App owner or App manager + C9 | n/a | [GH2] |
| G16 | **Remove stale runner registrations** by hand (the mediator normally does this) | ptone (repo owner) | Repo admin (`repo` scope) or org owner (`org` scope) | Repo *Administration*: write or org *Self-hosted runners*: write | [GH1][GH5][GH8] |
| G17 | **Uninstall or suspend the App** (emergency stop, stronger than G11) | ptone | Org owner | n/a | [GH3] |
| G18 | **(Optional) Larger GitHub-hosted runner** as the fallback target (`ubuntu-latest-8-cores`) | **UNVERIFIED** whether a personal account can create one | Org owner. Always billed per minute, even on public repos. | n/a | [GH16] |

Runtime calls made by the mediator (generate-jitconfig, delete runner, list
runs and jobs) use the App's installation access token. That token expires
after one hour [GH17] and is minted from a JWT valid for at most 10 minutes
[GH18]. No human role is involved at runtime.

## 2. GCP steps

All GCP steps are the same for both GitHub targets. Every principal below is a
human or CI identity running the P4 scripts, called **the deployer**, unless the
step says otherwise. Resource names come from `config.example.env`.

| # | Step | Minimum role (resource) | Notes | Sources |
| :-- | :--- | :--- | :--- | :--- |
| C1 | Enable APIs: Cloud Run, Artifact Registry, Secret Manager, Cloud Scheduler, Cloud Build, IAM, Cloud Storage | Listing: `roles/serviceusage.serviceUsageConsumer` or `roles/serviceusage.serviceUsageViewer` (project; `serviceusage.services.list`). Enabling: `roles/serviceusage.serviceUsageAdmin` (project; `serviceusage.services.enable`), **only if an API is missing** | `bootstrap-gcp.sh` lists the enabled APIs first and enables only the missing ones; when all are enabled it makes no enable call, so a deployer on a shared project where an admin already enabled them needs no `serviceusage.services.enable`. A list error stops the script. | [GC6] |
| C2 | Create the four service accounts (`BUILD_SA_NAME`, `MEDIATOR_SA_NAME`, `SCHEDULER_SA_NAME`, `EXEC_SA_NAME`) | `roles/iam.serviceAccountAdmin` (project) | `EXEC_SA_NAME` must stay at **zero** roles | [GC5] |
| C3 | Create the Artifact Registry Docker repo `ARTIFACT_REPO` in `REGION` | `roles/artifactregistry.admin` (project) | | [GC4] |
| C4 | Grant `BUILD_SA_NAME` `roles/artifactregistry.writer` on `ARTIFACT_REPO` | `roles/artifactregistry.admin` (repo) | Repository-level binding, not project-level | [GC4] |
| C5 | Create the secrets `GITHUB_APP_PRIVATE_KEY_SECRET_NAME` and `GITHUB_WEBHOOK_SECRET_NAME`, and add their first versions | `roles/secretmanager.admin` (project) | Values come from G2 and G3. Pipe them from a file or stdin, never pass them as an argument. | [GC3] |
| C6 | Grant `MEDIATOR_SA_NAME` `roles/secretmanager.secretAccessor` on **each of the two secrets** | `roles/secretmanager.admin` (secret) | Secret-level bindings, not project-level | [GC3] |
| C7 | Build and push the runner image with Cloud Build as `BUILD_SA_NAME` | `roles/cloudbuild.builds.editor` (project) + `roles/iam.serviceAccountUser` on `BUILD_SA_NAME` | A user-specified build SA requires `logging: CLOUD_LOGGING_ONLY` (or a logs bucket) [GC7]. `BUILD_SA_NAME` then also needs `roles/logging.logWriter`, which the design omits (item X7). | [GC7] |
| C8 | Create or update the Cloud Run Job `RUNNER_JOB_NAME` (image, CPU, memory, `--max-retries=0`, `--task-timeout`, ephemeral-disk volume) running as `EXEC_SA_NAME` | `roles/run.developer` (project or job) + `roles/iam.serviceAccountUser` on `EXEC_SA_NAME` | Deploying with a service identity requires `iam.serviceAccounts.actAs` on it | [GC1][GC2] |
| C9 | **Secret rotation**: add a new version to a secret, then disable the old one | `roles/secretmanager.secretVersionAdder` (secret) to add; `roles/secretmanager.secretVersionManager` (secret) to disable or destroy | The mediator reads `latest`, so a restart or new revision picks up the new version. That restart needs `roles/run.developer` on the service (as in C12). | [GC3][GC1] |
| C10 | Grant `MEDIATOR_SA_NAME` `roles/run.developer` on the job `RUNNER_JOB_NAME` only | `roles/run.admin` (job), because setting IAM on a job is not in run.developer | `roles/run.developer` is what the docs require for executing with overrides and cancelling executions [GC2]. `roles/run.jobsExecutorWithOverrides` is a narrower candidate (item U3). | [GC1][GC2] |
| C11 | Grant `MEDIATOR_SA_NAME` `roles/iam.serviceAccountUser` on `EXEC_SA_NAME` | `roles/iam.serviceAccountAdmin` (on `EXEC_SA_NAME`) | The design requires this. Whether *executing* (rather than deploying) a job needs actAs is UNVERIFIED (item U3). The grant is harmless because `EXEC_SA_NAME` has no roles. | [GC1][GC2] |
| C12 | Deploy the mediator service `MEDIATOR_SERVICE_NAME` as `MEDIATOR_SA_NAME`, reachable by GitHub without Google auth | `roles/run.developer` + `roles/iam.serviceAccountUser` on `MEDIATOR_SA_NAME`; **plus** `roles/run.admin` (service) to bind `allUsers` as invoker | Public ingress is required because GitHub cannot present Google credentials. HMAC is the authentication. An org policy restricting allowed member domains blocks `allUsers`; then deploy with `--no-invoker-iam-check` instead (item U6). | [GC1] |
| C13 | Grant `SCHEDULER_SA_NAME` `roles/run.invoker` on `MEDIATOR_SERVICE_NAME` only | `roles/run.admin` (service) | `/reconcile` additionally checks the OIDC token in code (P2), because the service is public | [GC1] |
| C14 | Create the Cloud Scheduler job `RECONCILER_JOB_NAME` with an OIDC token for `SCHEDULER_SA_NAME` | `roles/cloudscheduler.admin` (project) + `roles/iam.serviceAccountUser` on `SCHEDULER_SA_NAME` | | [GC8] |
| C15 | **Quota increase**: Cloud Run regional ephemeral-disk (default 100 GiB, auto-granted) to cover `RUNNER_DISK_GI` x `MAX_CONCURRENT_RUNNERS`, plus regional CPU and memory if needed | `roles/cloudquotas.admin` (project), or the legacy `roles/servicemanagement.quotaAdmin`; Google approves the request | Only needed when raising `MAX_CONCURRENT_RUNNERS` above the default 5 (at 20 GiB each) | [GC9] |
| C16 | Read logs, executions and metrics (operations) | `roles/logging.viewer` + `roles/run.viewer` (project) | `roles/run.viewer` can see execution env overrides. That is why the JIT config is passed by a claim token, never as an env override (design 3.2), and why the claim also requires the runner identity's ID token (claim contract v2). Grant it narrowly. | [GC1] |
| C17 | Create alerting policies (launch failures, queue starvation) | `roles/monitoring.alertPolicyEditor` (project) | P4 or operations | [GC10] |
| C18 | Teardown (delete everything above) | The admin role for each resource above (C2-C14, C19) | `scripts/teardown.sh` (P4) | Same sources as C2-C14 |
| C19 | Create the Cloud Build source staging bucket `BUILD_STAGING_BUCKET` (default `GCP_PROJECT-cr-ci-staging`; uniform access, public access prevention, objects deleted after 7 days) and grant `BUILD_SA_NAME` `roles/storage.objectViewer` on that bucket only; builds pass `--gcs-source-staging-dir` | `roles/storage.admin` (project), or bucket create plus `roles/storage.admin` on the bucket | A user-specified build SA reads the uploaded source itself, so it needs object read on the staging location. A dedicated bucket keeps that grant bucket-scoped instead of on the shared default `GCP_PROJECT_cloudbuild` bucket (resolves U10). The deployer uploads the source, so it needs object create there (covered by `roles/storage.admin`). | [GC7][GC12][GC13] |

### 2.1 Deployer roles (least-privilege bundle)

On a **dedicated** project, `roles/owner` covers everything and is the simplest
choice. For a shared project such as `deploy-demo-test`, the deployer needs
these roles on the project:

- `roles/serviceusage.serviceUsageConsumer` (to list the enabled APIs), plus
  `roles/serviceusage.serviceUsageAdmin` only if an API in C1 is not yet
  enabled (otherwise ask a project admin to enable the missing ones)
- `roles/iam.serviceAccountAdmin`
- `roles/iam.serviceAccountUser` (or the same role granted only on the four cr-ci service accounts)
- `roles/artifactregistry.admin`
- `roles/secretmanager.admin`
- `roles/run.admin`
- `roles/cloudbuild.builds.editor`
- `roles/cloudscheduler.admin`
- `roles/monitoring.alertPolicyEditor` (only for the alerting policies, C17)
- `roles/cloudquotas.admin` (only when requesting the quota increase)
- `roles/storage.admin` (the build staging bucket, C19; it can be granted on that bucket only once it exists)
- `roles/iam.serviceAccountTokenCreator` on `BUILD_SA_NAME` (only for
  `build-runner-image.sh --local` / `deploy-mediator.sh --local`, which push
  with a token of the build SA)

### 2.2 Runtime identities (what the deployed system holds)

| Identity | Holds | Must not hold |
| :--- | :--- | :--- |
| `BUILD_SA_NAME` | `roles/artifactregistry.writer` on `ARTIFACT_REPO`; `roles/logging.logWriter` (C7); `roles/storage.objectViewer` on `BUILD_STAGING_BUCKET` only (C19) | Anything else |
| `MEDIATOR_SA_NAME` | `roles/secretmanager.secretAccessor` on the two secrets only; `roles/run.developer` on `RUNNER_JOB_NAME` only; `roles/iam.serviceAccountUser` on `EXEC_SA_NAME` only | Project-level roles |
| `SCHEDULER_SA_NAME` | `roles/run.invoker` on `MEDIATOR_SERVICE_NAME` only | Anything else |
| `EXEC_SA_NAME` | **Nothing.** Workflow code can read its metadata-server token, so the token must be worthless. | Any role, including `roles/logging.logWriter` (Cloud Run captures stdout and stderr without it) |
| GitHub App installation | The App permission set in [section 1](#1-github-steps), on the selected repos only | Contents, Pull requests, Secrets, Workflows, any write beyond runners |

## 3. Operations at a glance

| Operation | Fork | Upstream | GCP |
| :--- | :--- | :--- | :--- |
| Roll out: set the variable (G10) | ptone | write access | n/a |
| Fall back: clear the variable (G11) | ptone | write access | n/a |
| Emergency stop: uninstall or suspend the App (G17) | ptone | org owner | Or delete the runner job `RUNNER_JOB_NAME` (`roles/run.developer`, as for C8) |
| Rotate the webhook secret (G14 + C9) | ptone | App owner | `secretVersionAdder` (add), `secretVersionManager` (disable the old version), `run.developer` (restart the mediator) |
| Rotate the App key (G15 + C9) | ptone | App owner | `secretVersionAdder` (add), `secretVersionManager` (disable the old version), `run.developer` (restart the mediator) |
| Approve a fork run (G9) | ptone | write access | n/a |
| Raise concurrency (C15, then redeploy) | n/a | n/a | quota admin + `run.admin` |
| Bump the runner image (C7, C8) | n/a | n/a | `cloudbuild.builds.editor` + `run.developer` |

## 4. Notes on the target split

### 4.1 Who should own the GitHub App for upstream

GitHub allows two ownership models [GH2][GH3]:

- **App owned by a personal account (ptone), installable on "Any account".**
  ptone keeps the private key and webhook settings. The GoogleCloudPlatform org
  owner only has to install it (G4 or G5). One App can serve both the fork and
  upstream, with one deployment and `TARGET_REPOS=ptone/scion,GoogleCloudPlatform/scion`
  in `repo` scope. The cost: a public App can be installed by anyone. The
  mediator rejects webhooks for repositories outside `TARGET_REPOS`, so foreign
  installations cannot start runners, but they can send webhook traffic.
- **App owned by the GoogleCloudPlatform org.** An org owner, or a GitHub App
  manager [GH4], creates it and controls the key. This needs more org
  involvement, but may be what the org's governance expects. **UNVERIFIED:**
  whether GoogleCloudPlatform has an internal policy that requires org-owned
  Apps, or that forbids Apps owned by personal accounts.

Either way the **installation** on GoogleCloudPlatform/scion needs an org
owner.

### 4.2 `repo` vs `org` installation scope for upstream

| | `repo` scope | `org` scope |
| :--- | :--- | :--- |
| App permission | Repo *Administration*: read and write. Broad: it also allows changing repo settings. | Org *Self-hosted runners*: read and write. Narrower in kind, but covers runners for the **whole org**. |
| Org-owner steps | G4, G6 | G5, G7 |
| Runner group / public-repo override | Not needed | Required (G7) |
| Fits the personal fork too | Yes (one model for both targets) | No (the fork stays on `repo`) |

Recommendation: `repo` scope for both targets. It is the only scope that works
for the fork, it keeps runners scoped to the one repository, and it avoids the
public-repo override on an org runner group. The trade-off is the
Administration permission. Mitigations: the installation is limited to the
target repo, the key lives only in Secret Manager, and only `MEDIATOR_SA_NAME`
can read it.

### 4.3 Upstream alternative (NOT implemented): repo webhook plus a PAT from a repo admin

An org owner is needed only because an App that requests *Repository
administration* cannot be installed by a repo admin [GH3]. The alternative
avoids the App: a repo admin of `GoogleCloudPlatform/scion` creates a
repository webhook and a personal access token (PAT), and the mediator uses
that token instead of App installation tokens. **cr-ci does not implement
this.** The mediator has no PAT auth mode.

What it would need:

| Piece | Requirement | Who | Sources |
| :--- | :--- | :--- | :--- |
| Webhook | A repository webhook on `workflow_job`, pointing at `MEDIATOR_URL/webhook`, with the webhook secret | Repo admin ("Manage webhooks": Admin) | [GH11][GH12][GH10] |
| Runner registrations | `generate-jitconfig`, list and delete runners on the repo: a fine-grained PAT with repository **Administration: read and write** (plus **Actions: read** for runs and jobs), or a classic PAT with the `repo` scope. The token's user must have admin access to the repo | Repo admin creates the token under their own account | [GH1] ("The authenticated user must have admin access to the repository"; fine-grained token permissions) |
| Org PAT policy | By default an org **requires owner approval** for fine-grained PATs that access its resources, and owners can **block classic PATs** from org resources entirely. Owners can also cap fine-grained PAT lifetime (default: must expire within 366 days) | An org owner must approve the token, or have chosen not to require approval | [GH20] |
| SAML SSO | If the org enforces SAML single sign-on, the token must also be authorized for SSO by its user. **UNVERIFIED** whether GoogleCloudPlatform enforces SAML SSO | Token's user | [GH21] |
| Mediator | A PAT auth mode: read the token from Secret Manager instead of minting installation tokens, skip the installation lookup, and alert before the token expires | cr-ci code change (not done) | `mediator/internal/githubapp` |

Why it is weaker than an App:

- **Tied to a person.** The token acts as the repo admin who created it. It
  stops working when that person loses admin access or leaves, and their
  name is on every runner registration.
- **Long-lived and broad.** A PAT lives up to its expiry (months), not one
  hour like an installation token [GH17], and a classic `repo` token reaches
  every repository its user can. A leaked App key can be revoked by the App
  owner, and its tokens are limited to one installation.
- **Manual rotation** before every expiry, through the person who owns the
  token.
- **Likely still needs an org owner anyway.** Under the default policy a
  fine-grained PAT needs owner approval [GH20]. So the alternative removes the
  org owner only where the org has relaxed its policy. **UNVERIFIED** for
  GoogleCloudPlatform.

Verdict: if upstream comes into scope, ask the org owner to install the App
([section 6](#6-request-to-a-googlecloudplatform-org-owner-future)). Consider
the PAT path only if the org owner refuses the App but approves a
fine-grained PAT, and accept the costs above.

## 5. Design corrections and open items

Corrections to the cr-runners design (verified):

- **X1. Repo admins cannot install this App on an org repo.** Design section 7,
  step 7, says installation "requires repo admin / GoogleCloudPlatform org
  approval". GitHub: repo admins can install an App only if it requests no org
  permissions and not *Repository administration* [GH3]. This App requests
  *Repository administration* (repo scope) or an org permission (org scope), so
  **only an org owner can install it on GoogleCloudPlatform/scion**.
- **X2. Setting `SCION_LARGE_RUNNER` needs write access, not repo admin.**
  Design 2.3 and section 7, step 5, say repo admin. GitHub: write access (org
  repos) or collaborator (personal repos) [GH7][GH12]. This makes the fallback
  toggle available to every maintainer, which helps operations, but anyone with
  write can also *route* jobs to the runner. The mediator's fork guard and
  allowlist remain the security boundary.
- **X3. `workflow_job` delivery needs Actions: read.** The design lists *Actions*
  as read-only for API reads. It is also the permission that allows subscribing
  to `workflow_job` [GH10], so it cannot be dropped even if reconcile stopped
  reading runs.
- **X4. Org scope does not remove the need for repository permissions.** Design
  3.1 says org *Self-hosted runners* "replaces" repo *Administration*. That is
  correct for runner calls [GH5], but the App still needs repository *Actions*:
  read (for runs, jobs and the webhook) on the installed repos.
- **X5. Org runners on a public repo need the runner-group override.** The
  design does not mention it. Runner groups are private-repos-only by default
  [GH8].
- **X6. Cloud Run Jobs memory vs CPU.** Design 1.2 says "> 16 GiB requires at
  least 4 vCPUs". Cloud Run Jobs actually allow 4 vCPU up to 16 GiB, 6 vCPU up
  to 24 GiB and 8 vCPU up to 32 GiB [GC11]. The default 8 vCPU / 32 GiB is valid.
  The mediator loader enforces the real table.
- **X7. The Cloud Build SA needs logging.** A user-specified build SA requires
  `CLOUD_LOGGING_ONLY` or a logs bucket [GC7], so `BUILD_SA_NAME` needs
  `roles/logging.logWriter` (or bucket write) in addition to the design's
  `artifactregistry.writer`.

Resolved during P1 review:

- **U1 (resolved). `runner_group_id` on the repo-level JIT endpoint.** It is a
  required body field even for repo-level runners [GH1], and `1` (the default
  group) is the value to send there. cr-runners-arch confirmed this in the P1
  review. cr-ci sends `GITHUB_RUNNER_GROUP_ID` (default `1`). With org scope,
  set it to the group configured in G7. The P6 PoC will exercise it against
  the live API.

Still UNVERIFIED (to settle in P2, P4 or P6). Each item's runtime check is in
[poc-checklist.md](poc-checklist.md):

- **U2. JIT config lifetime before first use.** GitHub documents that an unused
  JIT runner is removed automatically, and that ephemeral runners are removed
  after one day offline [GH19]. It does not document how long the encoded config
  stays valid. The claim token's `CLAIM_TTL` (default 300 s) bounds the cr-ci
  side regardless.
- **U3. Narrower mediator role.** Whether `roles/run.jobsExecutorWithOverrides`,
  plus a role for listing and cancelling executions, can replace
  `roles/run.developer` on the job; and whether executing (not deploying) a job
  requires actAs on its service identity (C11). Check in P6 with a policy
  troubleshooter dry run.
- **U4. Fork-approval setting role.** docs.github.com places it under the
  repository's Settings, which require admin ("Change a repository's settings":
  Admin only [GH12]). The page itself does not state a role [GH14]. At org level
  it is under org Settings, so org owner [GH9].
- **U5. Larger runners for personal accounts** (G18): the plan requirements were
  not found in the docs that were fetched [GH16].
- **U6. GCP org policy.** If `GCP_PROJECT` lives in a Google-managed GCP org, the
  domain-restricted-sharing constraint (`iam.allowedPolicyMemberDomains`) may
  forbid the `allUsers` invoker binding in C12. The fallback is to deploy
  with `--no-invoker-iam-check` (Cloud Run skips the invoker IAM check, so no
  `allUsers` binding is needed); cr-runners-arch agreed in the P1 review.
  `deploy-mediator.sh` (P4) supports both. The webhook stays protected by
  HMAC, and `/reconcile` by the in-code OIDC check (audience `MEDIATOR_URL`).
  **UNVERIFIED** for the actual project. Changing org policy needs
  `roles/orgpolicy.policyAdmin` at org level, which is outside cr-ci's reach.
- **U7. Default labels on JIT runners.** cr-ci calls `generate-jitconfig` with
  `labels: [RUNNER_LABEL]`, assuming GitHub adds no default labels. GitHub
  describes `labels` as custom labels "to add", and its example JIT response
  shows `self-hosted` and `X64`, so JIT runners may carry the default labels
  too. If they do, a job with `runs-on: self-hosted` could also be assigned to
  a cr-ci runner. **UNVERIFIED**; check in the P6 PoC (list the runner's
  labels after a mint). Either way the mediator's hijack guard keys on
  `runner_name`, not labels, so cr-ci does not rely on the label choice as a
  defence.
- **U8. gcloud version floor.** The scripts require gcloud >=
  `CRCI_MIN_GCLOUD` (`scripts/lib.sh`, 530.0.0) for `--apply`. The oldest
  release that has every flag used (`--no-invoker-iam-check`, ephemeral-disk
  volumes on jobs, `run jobs deploy/update` volume flags) was not looked up;
  the floor is a recent release. **UNVERIFIED**; lower or raise it once
  checked against the gcloud release notes.
- **U9. Job volume update in one call.** `deploy-job.sh` creates the job with
  `--add-volume=name=scratch,type=ephemeral-disk,size=<n>Gi` plus
  `--add-volume-mount`, and updates it with `--clear-volumes
  --clear-volume-mounts` followed by the same flags. The GA `gcloud run jobs
  create` and `update` references list `type=ephemeral-disk` with an optional
  `size` key and the `--clear-volumes` / `--clear-volume-mounts` flags
  (checked by the P4 reviewer). **UNVERIFIED** only that clear and add combine
  in one `update` call; check in the P6 PoC on a second `deploy-job.sh --apply`.
- **U10 (resolved by design). Cloud Build staging access.** `gcloud builds
  submit` uploads the source to a bucket the build SA must read. Instead of
  granting access to the shared default `gs://GCP_PROJECT_cloudbuild`,
  `bootstrap-gcp.sh` creates a dedicated staging bucket and grants
  `BUILD_SA_NAME` `roles/storage.objectViewer` on it only, and the builds pass
  `--gcs-source-staging-dir` (row C19). Confirm on the first P6 build.

## 6. Request to a GoogleCloudPlatform org owner (future)

Not needed for the current fork-only scope. Kept ready for the day upstream
is in scope.

Only needed if upstream is chosen. Ready to send as written; fill in the
bracketed parts.

> **Subject: GitHub App install + Actions settings for GoogleCloudPlatform/scion self-hosted CI runners**
>
> We are moving the `golangci-lint` CI job of GoogleCloudPlatform/scion off
> 16 GB GitHub-hosted runners, where it is OOM-killed, onto ephemeral
> 8 vCPU / 32 GiB runners on Cloud Run. Each runner is single-use: a
> just-in-time registration, one job, then deleted. Code and docs: `cr-ci/`
> in the scion repo. Background: the cr-runners design doc.
>
> As a GoogleCloudPlatform **organization owner**, please:
>
> 1. **Install the GitHub App** `[APP NAME / URL]` on GoogleCloudPlatform
>    with **"Only select repositories" → `scion`**. It requests: Repository
>    *Actions*: read, Repository *Administration*: read & write (needed to
>    mint and delete just-in-time runner registrations), Repository
>    *Metadata*: read, and the `workflow_job` webhook event. No access to
>    contents, pull requests, secrets or workflows. (Repo admins cannot install
>    an App that requests Administration; GitHub routes it to org owners.)
> 2. **Allow repository-level self-hosted runners for `scion`**: Organization
>    settings → Actions → General → *Runners* = "All repositories" or
>    "Selected repositories" including `scion`. Please tell us if an
>    enterprise policy overrides this.
> 3. **Confirm the fork PR approval policy** for `scion` is *"Require approval
>    for all external contributors"* (org setting, or allow the repo admin to
>    set it). The workflow also refuses to route fork PRs to these runners, and
>    the runner service rejects them independently.
> 4. Tell us whether GoogleCloudPlatform requires **org-owned GitHub Apps**. If
>    it does, please create the App under the org from our manifest
>    `[link, P5]` (or make `[NAME]` a GitHub App manager), and we will hand you
>    the webhook settings.
>
> Not needed from you: setting the `SCION_LARGE_RUNNER` repository variable at
> cutover (any maintainer with write access can do this, and clearing it is
> the instant fallback), and any GCP access.
>
> Only if we choose org-level runners instead (not our recommendation): in
> place of 1 and 2, install the App org-wide on selected repos with
> Organization *Self-hosted runners*: read & write, and enable *Allow public
> repositories* on the runner group used (Default group or a dedicated one)
> restricted to `scion`.

## 7. Sources

GitHub (docs.github.com, read 2026-10-21):

- [GH1] Permissions required for GitHub Apps (Administration; Actions; Variables; Webhooks; org Self-hosted runners): https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps , and REST self-hosted runners: https://docs.github.com/en/rest/actions/self-hosted-runners
- [GH2] Registering a GitHub App ("under your personal account or under any organization you own"; "Only on this account" / "Any account"): https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app
- [GH3] Installing a GitHub App from a third party ("Anyone can install GitHub Apps on their personal account"; "Organization owners can install…"; "Repository admins can install … if the app does not request any organization permissions or the 'Repository administration' permission"): https://docs.github.com/en/apps/using-github-apps/installing-a-github-app-from-a-third-party
- [GH4] GitHub App managers: https://docs.github.com/en/organizations/managing-programmatic-access-to-your-organization/adding-and-removing-github-app-managers-in-your-organization , and roles in an organization: https://docs.github.com/en/organizations/managing-peoples-access-to-your-organization-with-roles/roles-in-an-organization
- [GH5] Autoscaling with self-hosted runners ("For repository-level runners, a GitHub App requires the `administration` permission… organization-level… `organization_self_hosted_runners`"): https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/autoscaling-with-self-hosted-runners
- [GH6] REST workflow runs: https://docs.github.com/en/rest/actions/workflow-runs
- [GH7] Store information in variables (organization repo: `write` access; personal repo: collaborator; org level: org owners): https://docs.github.com/en/actions/writing-workflows/choosing-what-your-workflow-does/store-information-in-variables
- [GH8] Adding self-hosted runners (repo: owner, or for org repos org owner or repo admin; org: org owner): https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/adding-self-hosted-runners , and managing access with runner groups ("only private repositories can access runners in a runner group, but you can override this"; additional groups need Team): https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/managing-access-to-self-hosted-runners-using-groups
- [GH9] Disabling or limiting GitHub Actions for your organization (Runners policy; fork approval at org level): https://docs.github.com/en/organizations/managing-organization-settings/disabling-or-limiting-github-actions-for-your-organization
- [GH10] Webhook events and payloads, `workflow_job` ("must have at least read-level access for the 'Actions' repository permission"): https://docs.github.com/en/webhooks/webhook-events-and-payloads
- [GH11] Creating webhooks (repo: owner or admin; org: org owner; App owners and managers configure App webhooks): https://docs.github.com/en/webhooks/using-webhooks/creating-webhooks
- [GH12] Repository roles for an organization (variables: Write, Maintain, Admin; "Change a repository's settings": Admin; "Manage webhooks": Admin): https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-repository-roles/repository-roles-for-an-organization
- [GH13] Requesting a GitHub App from your organization owner: https://docs.github.com/en/apps/using-github-apps/requesting-a-github-app-from-your-organization-owner , and limiting App access requests: https://docs.github.com/en/organizations/managing-programmatic-access-to-your-organization/limiting-oauth-app-and-github-app-access-requests
- [GH14] Managing GitHub Actions settings for a repository (fork approval options; org and enterprise policies can override; self-hosted runner warning): https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository
- [GH15] Approving workflow runs from public forks ("Maintainers with write access"): https://docs.github.com/en/actions/managing-workflow-runs-and-deployments/managing-workflow-runs/approving-workflow-runs-from-public-forks
- [GH16] Managing larger runners (org owners add them) and about larger runners (always billed per minute): https://docs.github.com/en/actions/using-github-hosted-runners/using-larger-runners/managing-larger-runners , https://docs.github.com/en/actions/using-github-hosted-runners/using-larger-runners/about-larger-runners
- [GH17] Generating an installation access token (expires after 1 hour): https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app
- [GH18] Generating a JWT for a GitHub App (`exp` at most 10 minutes ahead): https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-json-web-token-jwt-for-a-github-app
- [GH20] Setting a personal access token policy for your organization (fine-grained PATs: "Require administrator approval" by default; classic PATs can be restricted; fine-grained default maximum lifetime 366 days): https://docs.github.com/en/organizations/managing-programmatic-access-to-your-organization/setting-a-personal-access-token-policy-for-your-organization
- [GH21] Authorizing a personal access token for use with SAML single sign-on: https://docs.github.com/en/authentication/authenticating-with-saml-single-sign-on/authorizing-a-personal-access-token-for-use-with-saml-single-sign-on
- [GH19] Removing self-hosted runners (unused JIT runners auto-removed; ephemeral removed after 1 day offline): https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/removing-self-hosted-runners
- Security hardening for GitHub Actions ("Self-hosted runners should almost never be used for public repositories…"; JIT runners "perform at most one job"): https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions

GCP (docs.cloud.google.com, read 2026-10-21):

- [GC1] Cloud Run IAM roles (run.admin, run.developer, run.invoker, run.jobsExecutorWithOverrides; actAs for service identity): https://docs.cloud.google.com/run/docs/reference/iam/roles
- [GC2] Execute jobs ("Cloud Run Invoker" to execute; "Cloud Run Developer … to override job configurations, or to cancel job executions"): https://docs.cloud.google.com/run/docs/execute/jobs
- [GC3] Secret Manager access control: https://docs.cloud.google.com/secret-manager/docs/access-control
- [GC4] Artifact Registry access control: https://docs.cloud.google.com/artifact-registry/docs/access-control
- [GC5] Service account roles: https://docs.cloud.google.com/iam/docs/service-account-permissions
- [GC6] Enabling services: https://docs.cloud.google.com/service-usage/docs/enable-disable
- [GC7] Cloud Build user-specified service accounts (logging requirement): https://docs.cloud.google.com/build/docs/securing-builds/configure-user-specified-service-accounts
- [GC8] Cloud Scheduler HTTP targets with authentication: https://docs.cloud.google.com/scheduler/docs/http-target-auth
- [GC9] Cloud Quotas permissions: https://docs.cloud.google.com/docs/quotas/permissions
- [GC10] Cloud Monitoring access control: https://docs.cloud.google.com/monitoring/access-control
- [GC11] Cloud Run Jobs CPU limits (allowed 1, 2, 4, 6, 8; memory range per CPU): https://docs.cloud.google.com/run/docs/configuring/jobs/cpu
- [GC12] `gcloud builds submit` reference (`--gcs-source-staging-dir`, `--service-account`): https://cloud.google.com/sdk/gcloud/reference/builds/submit
- [GC13] Cloud Storage IAM roles (`roles/storage.objectViewer`, `roles/storage.admin`): https://docs.cloud.google.com/storage/docs/access-control/iam-roles

The role names in GC3-GC6 and GC8-GC10 are the standard predefined roles. They were
not each re-fetched for this page; P4's dry-run transcripts are the check.
