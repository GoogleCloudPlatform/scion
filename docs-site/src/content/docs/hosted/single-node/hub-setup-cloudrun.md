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
| `gcloud` (recent version, with `beta` component) | `gcloud beta run instances deploy --help` |
| `scion` CLI that includes `deploy-instance` (see below) | `scion deploy-instance --help` |
| `git` and `go` — only needed to build `scion` from source (see below) | `git --version`, `go version` |

The `deploy-instance` subcommand is not yet in any published `scion` release. Until
one ships it, build the CLI from a clone of the repository at `main`:

:::caution[Temporary workaround — check before you build]
Building from source is a stopgap, accurate as of 2026-08-27. It stops being
necessary the moment a published `scion` release ships `deploy-instance`. Run
`scion deploy-instance --help` against your installed binary first — if it
succeeds, skip the build. This section should be deleted once a release includes
the subcommand — tracked by
[ptone/scion#1314](https://github.com/ptone/scion/issues/1314).
:::

```bash
git clone https://github.com/GoogleCloudPlatform/scion.git
cd scion
go build -tags no_embed_web -o ./scion ./cmd/scion/
mkdir -p "$(go env GOPATH)/bin" && mv ./scion "$(go env GOPATH)/bin/scion"
```

The last line moves the binary into your Go bin directory (`~/go/bin` unless you
have overridden `GOPATH`) — the conventional location for Go-built commands, and
one that needs no `sudo`. Every `scion` command in this guide is then invoked
bare, exactly as it will be once a release ships `deploy-instance`.

Verify before continuing — this is the `scion` row of the [CLI tools](#cli-tools)
table:

```bash
scion deploy-instance --help
```

If it prints the help text, you are done with this section. Two failures are
possible, and both are fixed the same way:

- `scion: command not found` — your Go bin directory is not on your `PATH`.
- `unknown command "deploy-instance"` — an older `scion` **earlier** on your
  `PATH` (a Homebrew or release install) is shadowing the binary you just built.

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

Prepending, not appending, is what makes the freshly built binary win over an
older install. Re-run the verify command.

:::caution[Scope this `PATH` change to your deploy shell]
This build uses `-tags no_embed_web`, so it has **no web UI assets** — `scion
server start` from it would serve a blank UI. If you already have a full `scion`
install, export the `PATH` line in the shell you deploy from rather than in your
shell profile, so the web-less build does not shadow it everywhere.
:::

**Stay in the clone directory for the rest of this guide** — the optional wrapper
scripts in [Section 1](#1-deploy) and [Section 6](#6-teardown) are
repository-relative paths.

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

The deploy derives the agent image registry from `--image` automatically; if
derivation fails, the error names `--image-registry` as the explicit override.
When this value is wrong, agent creation fails — not the deploy itself.

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
| `--image-registry` | derived from `--image` | Override the image registry the broker uses to pull agent images |

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

:::caution[Always specify harnessConfig]
An agent create that omits `harnessConfig` will fail with a 502 and an error
naming a harness the operator never chose:

```
failed to find harness-config "antigravity": harness-config "antigravity" not found
```

This is the product-wide default harness resolving to a name that is not registered
on the running hub. The error gives no indication that specifying `harnessConfig`
is the fix. Always pass `template` and `harnessConfig` explicitly.
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
scion deploy-instance \
  --name my-scion-hub \
  --project $PROJECT_ID \
  --cpu 8 --memory 32Gi \
  --image us-docker.pkg.dev/ptone-misc/scion-alt/scion-omni:f99a818
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
