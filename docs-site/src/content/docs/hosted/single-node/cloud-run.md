---
title: Deploy on Cloud Run (Single-Node)
description: Deploy a single-node Scion Hub on a Cloud Run Instance with sandbox-based agent execution and IAP protection.
---

This guide deploys a single-node Scion Hub on a **Cloud Run Instance**. Agents run
as Cloud Run Sandboxes inside the same Instance — one container image, one deploy
command, no external database or storage to provision.

:::danger[Workspaces are ephemeral]
Agent workspaces live on the Instance's ephemeral disk. **A redeploy destroys all
workspaces and the SQLite control plane.** There is no persistence layer. Push work
to a git remote before redeploying. If you need durable workspaces, use the
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
| `gcloud` (recent version, with `beta` component) | `gcloud beta run instances deploy --help` |
| `scion` CLI (built from this branch) | `scion deploy-instance --help` |

The `deploy-instance` subcommand is part of the single-node Cloud Run tier and may
not be in earlier `scion` releases. Build from source if your installed binary does
not include it:

```bash
go build -tags no_embed_web -o ./scion ./cmd/scion/
```

:::caution[gcloud version]
`gcloud beta run instances deploy` requires a recent gcloud SDK. If `beta run
instances` returns "Invalid choice: 'instances'", update your SDK:
`sudo apt-get update && sudo apt-get --only-upgrade install google-cloud-cli`.
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
and all supported harnesses. There is no default public image; you must specify one
explicitly.

For this guide, use the image built from commit `f99a818`:

```
# Tag (readable, but tags can be moved):
us-docker.pkg.dev/ptone-misc/scion-alt/scion-omni:f99a818

# Digest (immutable — this identifies the exact artifact):
us-docker.pkg.dev/ptone-misc/scion-alt/scion-omni@sha256:e3eab113675848be634513b1e35bb40a03c0ba109b4ce771eac4b8905beafaaa
```

A tag is a pointer that can be reassigned; only the `@sha256:` digest guarantees
you are running the same build. Use the tag for readability and the digest when
pinning a known-good version.

To build your own image, see the [Image Build README](https://github.com/GoogleCloudPlatform/scion/blob/main/image-build/README.md)
and target `omni`:

```bash
image-build/scripts/build-images.sh --target omni --registry $YOUR_REGISTRY --push
```

---

## 1. Deploy

A single command creates the Instance, enables IAP, and verifies the perimeter:

```bash
scion deploy-instance \
  --name my-scion-hub \
  --project $PROJECT_ID \
  --region us-east4 \
  --image us-docker.pkg.dev/ptone-misc/scion-alt/scion-omni:f99a818
```

Or use the wrapper script:

```bash
./scripts/single-node/deploy.sh \
  --name my-scion-hub \
  --project $PROJECT_ID \
  --region us-east4 \
  --image us-docker.pkg.dev/ptone-misc/scion-alt/scion-omni:f99a818
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

---

## 2. First login

Open the Instance URL printed by the deploy command in your browser:

```
https://my-scion-hub-PROJECT_NUMBER.us-east4.run.app
```

1. **IAP challenge** — Google sign-in. Use the email that was bound as the IAP user
   during deploy (your gcloud account, or the `--admin-email` value).
2. **Hub login** — After IAP, the Hub presents its own login. The deployer is
   automatically seeded as the first admin.

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

For the API, specify `template`, `harnessConfig`, and `projectId` explicitly.
The access token authenticates through IAP because the caller has
`roles/iap.httpsResourceAccessor` (granted by the deploy command). Both the
`Authorization` and `Proxy-Authorization` headers work.

```bash
# Replace PROJECT_UUID with the project ID from the create-project response
curl -s -X POST "$HUB_URL/api/v1/agents" \
  -H "Authorization: Bearer $(gcloud auth print-access-token)" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-agent",
    "projectId": "PROJECT_UUID",
    "template": "default",
    "harnessConfig": "claude"
  }'
```

:::note[Identity tokens as an alternative]
For stricter or scripted environments, you can use an OIDC identity token instead
of an access token. The audience **must** be the IAP OAuth client ID (not the
resource path):

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

:::caution[Always specify template and harnessConfig]
An agent create that omits both `template` and `harnessConfig` will fail with a
runtime error. Always pass them explicitly. This is a known issue
([#37](https://github.com/GoogleCloudPlatform/scion/issues/37) /
[#48](https://github.com/GoogleCloudPlatform/scion/issues/48)).
:::

### Attach to the agent's terminal

Once the agent reaches a running state, click **Attach** in the web UI to open a
live tmux session in your browser. You can watch the agent work, send it messages,
and inspect its output in real time.

---

## 4. Sizing

<!-- PLACEHOLDER: sizing guidance pending -->
<!-- ======================================================================= -->
<!-- Sizing data is not yet available. Stress testing is running in parallel  -->
<!-- and will provide concrete numbers. DO NOT add estimates, ranges, or any  -->
<!-- numbers here. This section will be filled in when the stress test agents -->
<!-- (sn-stress-def, sn-stress-max) report their results.                    -->
<!-- ======================================================================= -->

Sizing guidance for this tier is pending and will be added when stress-test
results are available.

**What is known today:** the default allocation is `--cpu 4 --memory 8Gi`. There
are **no per-agent resource limits** — all agents share the Instance's CPU and
memory budget. A single compute-heavy agent can starve its neighbours. Plan
accordingly until per-agent limits are available.

---

## 5. Durability

This tier is **Tier 0: pure ephemeral**.

- **Workspaces** live on the Instance's ephemeral filesystem. A redeploy loses
  them.
- **The SQLite database** (projects, agent metadata) lives on the same ephemeral
  filesystem. A redeploy loses it.
- **The admin seed** (your email) is set by an environment variable in the deploy
  command, so it is re-established on every deploy.

This is a deliberate design trade for fast, cheap, disposable deployments — not an
oversight. Treat the Instance as a workspace, not as infrastructure.

**Before redeploying:** ensure every agent has pushed its work to a git remote.
Anything only on the Instance's local disk is gone after redeploy.

:::note[Shallow clones]
Agent workspaces are depth-1 shallow clones and can only push to `origin`. Pushes
to other remotes will fail. This is a known limitation
([#1274](https://github.com/GoogleCloudPlatform/scion/issues/1274)).
:::

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
([#1291](https://github.com/GoogleCloudPlatform/scion/issues/1291)). Verify:

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

### Agent create fails with runtime error

If creating an agent without `template` or `harnessConfig` returns an error (e.g.
"failed to find harness-config" or a 500), specify both explicitly. See the note
in [Section 3](#3-create-a-project-and-start-an-agent).
