---
title: Deploy on Cloud Run (Sandbox)
description: Deploy a single-node Scion Hub on a Cloud Run Instance with sandbox-based agent execution and IAP protection.
---

This guide deploys a single-node Scion Hub on a **Cloud Run Instance**. Agents run
as Cloud Run Sandboxes inside the same Instance — one container image, one deploy
command, no external database or storage to provision.

:::danger[All state is ephemeral]
Agent workspaces and the SQLite control plane live on the Instance's ephemeral
disk. **Any container restart — planned redeploy or unplanned crash — destroys all
of them.** There is no persistence layer. Push work to a git remote early and
often. If you need durable workspaces, use the
[VM (GCE) path](/scion/hosted/single-node/hub-setup-gce/) or the
[HA tier](/scion/hosted/ha/overview/).
:::

**What you will set up:**

| Component | Provided by | Purpose |
|-----------|-------------|---------|
| Hub + Broker | Cloud Run Instance | Control plane, API, web UI |
| Agent runtime | Cloud Run Sandboxes | Isolated agent containers |
| Database | Embedded SQLite | State (ephemeral) |
| Auth perimeter | Identity-Aware Proxy (IAP) | Zero-trust access control |

---

## 0. Prerequisites

### GCP project

You need a GCP project with billing enabled and the following APIs:

```bash
export PROJECT_ID="your-project-id"

gcloud services enable \
  run.googleapis.com \
  iap.googleapis.com \
  iam.googleapis.com \
  --project=$PROJECT_ID
```

### CLI tools

| Tool | Verify |
|------|--------|
| `gcloud` 582.0.0 or later (verified; 575.0.0 does not have `beta run instances`) | `gcloud version` and `gcloud beta run instances deploy --help` |
| `git` | `git --version` |

:::caution[gcloud version]
`gcloud beta run instances deploy --sandbox-launcher` requires Cloud SDK 582.0.0
or later. Version 575.0.0 does not have `beta run instances` at all. If the
command returns "Invalid choice: 'instances'", update your SDK:
`sudo apt-get update && sudo apt-get --only-upgrade install google-cloud-cli`.
:::

### Authentication

The deploy requires **two** credentials. `gcloud auth` and Application Default
Credentials (ADC) are separate credential stores — both must be configured:

```bash
gcloud auth login
gcloud auth application-default login
```

| Credential | Verify | Used by |
|------------|--------|---------|
| `gcloud auth login` | `gcloud auth list` | gcloud commands (instance create, IAM bindings) |
| `gcloud auth application-default login` | `gcloud auth application-default print-access-token` | REST API call that enables IAP (step 3b) |

:::caution[Two credential stores]
These are usually the same identity. When they differ, gcloud commands run as one
identity and the REST API call runs as another — which can cause a confusing
permission failure on step 3b after step 3a succeeds. The deploy script compares
the two identities and warns when they differ, **but only when the ADC token
carries an `email` claim. Service-account ADC (metadata server, Cloud Shell, CI)
does not, and the script says so and skips the comparison** — on those, verify by
hand. Either way the fix is to ensure both credentials are configured for the
same account.
:::

### Deployer permissions

The identity running the deploy needs these IAM roles on the target project:

| Role | Why |
|------|-----|
| `roles/run.admin` | Create and update Cloud Run Instances |
| `roles/iam.serviceAccountUser` | Attach a service account to the Instance (if using `--service-account`) |
| `roles/iap.admin` | Enable IAP and bind access policies |

:::note[Service account deployments]
If deploying via a service account (CI, automation), pass `--admin-email` to
set the Hub admin to a human email. The deployer SA is granted IAP access
automatically, but Hub admin is seeded from the deployer identity by default.
:::

### Container image

The deploy requires a pre-built **omni image** — a single image containing the Hub
and all supported harnesses. There is no public image; you must build your own.

**Build your own image with Cloud Build:**

The repository includes a Cloud Build config that builds the full omni image chain.
Before building, create an Artifact Registry repository to push to:

```bash
gcloud artifacts repositories create scion \
  --repository-format=docker \
  --location=us-central1 \
  --project=$PROJECT_ID
```

Then submit the build:

```bash
git clone https://github.com/GoogleCloudPlatform/scion.git
cd scion

gcloud builds submit \
  --project=$PROJECT_ID \
  --config=image-build/cloudbuild-omni.yaml \
  --substitutions="_TAG=$(git rev-parse --short HEAD),_SHORT_SHA=$(git rev-parse --short HEAD),_COMMIT_SHA=$(git rev-parse HEAD),_REGISTRY=us-central1-docker.pkg.dev/$PROJECT_ID/scion" \
  --ignore-file=image-build/gcloudignore-omni \
  .
```

The build runs eight images in a chain (thick-prep → scion-base → claude → codex →
opencode → antigravity → grok-build → omni). Each step builds in the Cloud Build
worker and feeds the next. Only the final `scion-omni` image is pushed to your
registry. Workers start fresh each run — there is no warm cache between builds.

Monitor progress in the
[Cloud Build console](https://console.cloud.google.com/cloud-build/builds) or with
`gcloud builds list --project=$PROJECT_ID`.

When it completes, your image is:

```
us-central1-docker.pkg.dev/$PROJECT_ID/scion/scion-omni:$(git rev-parse --short HEAD)
```

The deploy derives the agent image registry from `--image` automatically; if
derivation fails, the error names `--image-registry` as the explicit override.
When this value is wrong, agent creation fails — not the deploy itself.

**Stay in the clone directory for the rest of this guide** — the deploy script and
teardown script are repository-relative paths.

---

## 1. Deploy

A single command creates the Instance, enables IAP, and verifies the perimeter:

```bash
./scripts/single-node/deploy.sh \
  --name my-scion-hub \
  --project $PROJECT_ID \
  --region us-east4 \
  --image us-central1-docker.pkg.dev/YOUR_PROJECT/scion/scion-omni:YOUR_TAG
```

**What the command does, step by step:**

1. Resolves your gcloud identity and the project number.
2. Creates the Cloud Run Instance with `--sandbox-launcher` enabled (this is what
   allows agents to run as sandboxes inside the Instance).
3. Enables IAP via a REST API PATCH (`iapEnabled: true`, `invokerIamDisabled: true`).
4. Waits for IAP enforcement to activate (~30–75 seconds).
5. Binds your identity as an IAP-authorized user.
6. **Asserts the perimeter** — fetches the Instance URL with no credentials and
   **fails the deploy** if the app answers. This is the safety gate.
7. Prints the Instance URL.

:::caution[IAP is the only network guard]
The Cloud Run invoker IAM check is disabled on this tier (`invokerIamDisabled:
true`), because IAP's forwarded audience is incompatible with it. **IAP is
therefore the sole network perimeter** — there is no second gate behind it. A
deploy with IAP disabled is **refused, not warned about**, because without IAP the
Instance is open to the internet with only Hub session auth in front of it.
:::

### Optional flags

| Flag | Default | Description |
|------|---------|-------------|
| `--region` | `us-east4` | GCP region |
| `--cpu` | `4` | CPU allocation |
| `--memory` | `8Gi` | Memory allocation |
| `--admin-email` | deployer's gcloud account | Override the Hub admin email |
| `--service-account` | (default compute SA) | GCP service account for the Instance |
| `--image-registry` | derived from `--image` | Override the image registry the broker uses to pull agent images |

---

## 2. First login

Open the Instance URL printed by the deploy command in your browser:

```
https://my-scion-hub-PROJECT_NUMBER.us-east4.run.app
```

1. **IAP challenge** — Google sign-in. Use the email that was bound as the IAP user
   during deploy (your gcloud account, or the `--admin-email` value).
2. **Hub access** — After sign-in you land directly in the Hub. There is no
   second login. The deployer is automatically seeded as the first admin.

:::tip[Granting access to other users]
IAP access is region-scoped, not per-instance. To add another user:

```bash
gcloud iap web add-iam-policy-binding \
  --project=$PROJECT_ID \
  --region=us-east4 \
  --resource-type=cloud-run \
  --member=user:colleague@example.com \
  --role=roles/iap.httpsResourceAccessor
```

Then add them as a Hub user through the admin UI.
:::

---

## 3. Create a project and start an agent

### Create a project

From the Hub web UI, click **New Project**. Provide a name and a git remote URL
(e.g. a GitHub repository the agent will work in).

### Start an agent

Create an agent via the web UI or the API. The web UI is the simplest path — click
a project, then **New Agent**, pick a harness (e.g. Claude), and start it.

:::note[IAP authentication for API and scripted access]
If you access the Hub API programmatically (scripts, CI, or `curl`), authenticate
through IAP with an access token or an OIDC identity token. For identity tokens,
the audience **must** be the IAP OAuth client ID (not the resource path):

```bash
# Discover the auto-generated IAP OAuth client ID
export PROJECT_NUMBER=$(gcloud projects describe $PROJECT_ID --format="value(projectNumber)")
gcloud alpha iap oauth-clients list \
  "projects/$PROJECT_NUMBER/brands/$PROJECT_NUMBER" \
  --format="value(name)" | sed 's|.*/||'

# Use it as the audience
curl -s "$HUB_URL/health" \
  -H "Authorization: Bearer $(gcloud auth print-identity-token --audiences=CLIENT_ID)"
```

Using the resource path (`/projects/NUMBER/locations/REGION/services/NAME`) as the
audience will fail with "Invalid JWT audience".
:::

:::caution[Always specify harnessConfig when creating agents via the API]
An API agent create that omits `harnessConfig` will fail with a 502 and an error
naming a harness the operator never chose:

```
failed to find harness-config "antigravity": harness-config "antigravity" not found
```

This is the product-wide default harness resolving to a name that is not registered
on the running hub. The error gives no indication that specifying `harnessConfig`
is the fix. Always pass `template` and `harnessConfig` explicitly. The web UI
enforces harness selection and is not affected.
:::

### Attach to the agent's terminal

Once the agent reaches a running state, click **Attach** in the web UI to open a
live tmux session in your browser. You can watch the agent work, send it messages,
and inspect its output in real time.

---

## 4. Sizing

### Measured ceilings

| Instance size | Idle agents | Working agents |
|---------------|-------------|----------------|
| 4 CPU / 8 GiB (default) | 20 | 6 |
| 8 CPU / 32 GiB (maximum) | 51 | 14 |

8 CPU / 32 GiB is the largest size Cloud Run allows. Larger deploys are refused.

Each number is a **single observation** — one stress-test run per size. Repeatability
is unmeasured. These are the points past which the Instance was observed to fail,
not thresholds to design against.

**Do not extrapolate a per-CPU or per-GiB rule.** Four times the memory and twice
the CPU bought about three times the idle capacity and about twice the working
capacity. Two points cannot establish a curve, and the relationship is not linear
in either resource.

**The numbers are context. The operating signal is create latency.** Agent creates
under two seconds mean headroom. Creates at ten seconds or more mean the Instance
is near its ceiling — stop adding agents. That rule was measured at both sizes and,
unlike a headcount, adapts to what the agents are actually doing. See the
[overload warning](#overloading-the-instance-destroys-all-work) below for details.

**Sizing to the measured ceiling is not the safe choice.** Running fewer agents than
you could costs only capacity. Running more destroys every workspace on the Instance
with no warning and no recovery. The two errors are not the same size.

There are **no per-agent resource limits**. All agents share the Instance's CPU and
memory budget. A single compute-heavy agent can starve its neighbours.

To change the Instance size:

```bash
./scripts/single-node/deploy.sh \
  --name my-scion-hub \
  --project $PROJECT_ID \
  --cpu 8 --memory 32Gi \
  --image us-central1-docker.pkg.dev/YOUR_PROJECT/scion/scion-omni:YOUR_TAG
```

---

## 5. Durability

This tier is **Tier 0: pure ephemeral**.

- **Workspaces** live on the Instance's ephemeral filesystem.
- **The SQLite database** (projects, agent metadata) lives on the same ephemeral
  filesystem.
- **The admin seed** (your email) is set by an environment variable in the deploy
  command, so it is re-established automatically.

All state on the Instance is destroyed when the container restarts — whether by a
planned redeploy, an overload that crashes the container, or any other restart. A
redeploy you can plan for; a crash you cannot. **The only durable copy of any
agent's work is what it has pushed to a git remote.**

This is a deliberate design trade for fast, cheap, disposable deployments — not an
oversight. Treat the Instance as a workspace, not as infrastructure. If you need
durable workspaces, use the [VM (GCE) path](/scion/hosted/single-node/hub-setup-gce/)
or the [HA tier](/scion/hosted/ha/overview/).

:::danger[Overloading the Instance destroys all work]
If too many agents are started on one Instance, the container is terminated. The
Hub restarts into a clean state — **every agent, every project, and every workspace
is lost**.

The service recovers on its own within about 30 seconds. It comes back healthy,
responsive, and completely empty — a new Hub with no trace of what was running.
Nothing is visibly broken. An operator who checks after a crash sees a working
system and no error, which is more dangerous than an outage that announces itself.

**Under real load there is a warning signal.** When agents are actively working,
agent create times climb from under two seconds to tens of seconds as the Instance
approaches its limit. **Agent creates taking ten or more seconds mean the Instance
is near its ceiling — stop adding agents.** The final create before a crash
returned a 503 in both measured cases.

With idle agents there is no such warning. Creates return success at normal speed
right up to the point of failure, and the operator's last signal before losing
everything is a success message.

There are no per-agent resource limits on this tier and no direct memory or CPU
instrument visible to the operator; create latency under load is the practical
proxy. See [Section 4](#4-sizing) for measured ceilings by Instance size.
**Push work to a git remote often.** Anything not pushed is unrecoverable.
:::

:::note[Shallow clones]
Agent workspaces are depth-1 shallow clones and can only push to `origin`. Pushes
to other remotes will fail. This is a known limitation
([ptone/scion#1274](https://github.com/ptone/scion/issues/1274)).
:::

---

## Sandbox Runtime & Volumes

When running agents as Cloud Run Sandboxes, Scion enforces security hardening and standardized mounting conventions to guarantee compatibility with modern harnesses.

### Non-Root Execution (UID 1000)
To align with secure-by-default execution practices, the Cloud Run Sandbox runtime executes agents as **non-root users** (`UID 1000`, matching the built-in `scion` user inside the base images), even though the host-side launcher process executes as `root` (UID 0).

* **Harness Compatibility**: This hardening is critical for newer harnesses like Claude Code (≥ 2.1.246), which explicitly refuse to start with `root` or `sudo` privileges (even when configured with `--dangerously-skip-permissions`).
* **Environment Injection**: The runtime explicitly maps and configures:
  * `SCION_HOST_UID` & `SCION_HOST_GID` to `1000` so that `sciontool init` drops privileges.
  * `HOME` directed to the agent-specific sandbox-mounted directory (e.g. `~/.gemini`).
  * `USER` and `LOGNAME` environment variables forced to `scion`.
* **Writability**: The broker ensures all agent homes, workspaces, and cached directories are pre-chowned to UID 1000 so the non-root runner can write and modify code without permission blocks.

### Standardized Shared Volume Mounts
Shared directories within the Cloud Run Sandbox are mounted at standard paths matching the Kubernetes runtime conventions, ensuring configuration portability across Single-Node and HA environments.

* **Standard Mount Path**: All shared directories are mapped to `/scion-volumes/<name>` inside the sandbox container.
* **Workspace & Read-Only Support**: The sandbox runtime fully supports mounting shared volumes within the workspace directory tree (using `in_workspace: true` to mount at `/workspace/.scion-volumes/<name>`) and enforces read-only locks when specified.

---

## 6. Teardown

Delete the Instance:

```bash
./scripts/single-node/teardown.sh \
  --name my-scion-hub \
  --project $PROJECT_ID
```

Or directly:

```bash
gcloud beta run instances delete my-scion-hub \
  --region=us-east4 \
  --project=$PROJECT_ID \
  --quiet
```

### Cost of leaving an Instance running

A Cloud Run Instance is billed for CPU and memory for the entire time it exists,
regardless of whether it is handling requests. There is no scale-to-zero. Delete
the Instance when you are not using it.

### Cleaning up IAP bindings

IAP access bindings are region-scoped, not per-instance. If this was the only
instance in the region, review and clean up bindings:

```bash
gcloud iap web get-iam-policy \
  --project=$PROJECT_ID \
  --region=us-east4 \
  --resource-type=cloud-run
```

---

## 7. Troubleshooting

### Image pull failures on first deploy

If the Instance fails to start with a confusing image-pull error that names a cache
mirror rather than your image, this is a known platform behavior
([ptone/scion#1291](https://github.com/ptone/scion/issues/1291)). Verify:

1. The image coordinate is correct (check for typos in the digest or tag).
2. The image is accessible to the Instance's service account.
3. Re-run the deploy — transient pull failures sometimes resolve on retry.

### IAP not enforcing after deploy

The deploy command includes a perimeter assertion that fails the deploy if IAP is
not enforcing. If you see the Instance URL responding without an IAP challenge:

```bash
# Check IAP status
curl -s -o /dev/null -w "%{http_code}" "https://INSTANCE_URL"
# Expected: 302 (redirect to Google sign-in)
# Bad: 200 (app is answering directly — IAP is not enforcing)
```

Re-run the deploy command — it is idempotent and will re-enable IAP.

### Agent create returns 502: `harness-config "antigravity" not found`

If creating an agent without `harnessConfig` returns a 502 with `failed to find
harness-config "antigravity"`, the fix is to specify `harnessConfig` explicitly
(e.g. `"harnessConfig": "claude"`). See the note in
[Section 3](#3-create-a-project-and-start-an-agent).
