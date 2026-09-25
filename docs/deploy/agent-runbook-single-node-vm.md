# Agent Runbook: Single-Node VM Deployment

Deploy a Scion Hub on a single GCE VM with IAP authentication. This runbook is
written for an AI agent to read and execute end-to-end. Follow the sections in
order.

For architecture details, access patterns, and configuration reference, see
[docs/deploy/single-node-vm.md](single-node-vm.md).

---

## 0. Obtain the Repository

If you are reading this runbook from a GitHub URL or a standalone file (not from
within a local clone of the Scion repository), clone the repo first. The deploy
script and its supporting files must be available locally.

```bash
git clone --depth 1 https://github.com/GoogleCloudPlatform/scion.git /tmp/scion-repo
cd /tmp/scion-repo
```

> **Important for AI agents:** Always clone the repository locally rather than
> reading the runbook via URL-fetching tools. Web-reading tools may silently
> truncate long documents — this runbook is 500+ lines and critical deployment
> steps in later sections will be missed if truncated.

A shallow clone (`--depth 1`) is sufficient — the deploy script does not need
git history. The `/tmp/scion-repo` path is consistent with Section 5.1; if you
already have the repo cloned or checked out elsewhere, skip this step and
adjust the paths in Section 5 accordingly.

---

## 1. Prerequisites

Verify each tool is available before proceeding. If any check fails, stop and
tell the user what is missing.

| Tool | Check command | Expected output |
|------|--------------|-----------------|
| gcloud CLI | `gcloud --version` | Version string (any version) |
| bash | `bash --version` | Version string (any version) |
| python3 | `python3 --version` | Version string (3.6+) |
| curl | `curl --version` | Version string (any version) |

The config file is JSON, parsed with Python's built-in `json` module — no
extra install is required. If `python3` is not on `PATH`, or you need a
different interpreter, set `PYTHON=/path/to/python3` when invoking
`deploy.sh`.

**VM operating system:** the deployed GCE VM's image is pinned to Ubuntu
22.04 LTS (`ubuntu-2204-lts` / `ubuntu-os-cloud`) and is not currently
configurable — `cloud-init.yaml`'s Docker apt-repo setup is written for this
specific image. This is not something the operator needs to prepare locally;
it's noted here because it affects what the deployed VM looks like.

---

## 2. GCP Preflight

Run these checks before asking the user any deployment questions. If the
environment is not ready, fix it first — do not waste the user's time gathering
config details that cannot be used.

### 2.1 Authentication

```bash
gcloud auth list --filter=status:ACTIVE --format='value(account)'
```

**Expected:** An email address (the active account).

**If empty:** Tell the user to run `gcloud auth login` and try again.

### 2.2 Project access

Ask the user which GCP project to use, or detect the current default:

```bash
gcloud config get-value project 2>/dev/null
```

Then verify access:

```bash
gcloud projects describe PROJECT_ID --format='value(projectId)'
```

**Expected:** The project ID echoed back.

**If it fails:** The user does not have access to the project, or the project
does not exist. Ask them to verify the project ID and their permissions.

### 2.3 Required APIs

`deploy.sh` enables every API it needs itself (Phase 2) — this preflight
check exists only to fail fast on permissions before gathering deployment
details from the user, not because the operator needs to enable anything
manually.

Check each API. If any is missing, run the consolidated enable command below
(`gcloud services enable` is idempotent and accepts multiple services, so
there's no need to enable them one at a time).

| API | Check |
|-----|-------|
| `compute.googleapis.com` | `gcloud services list --enabled --filter="name:compute.googleapis.com" --format="value(name)" --project=PROJECT_ID` |
| `run.googleapis.com` | `gcloud services list --enabled --filter="name:run.googleapis.com" --format="value(name)" --project=PROJECT_ID` |
| `iap.googleapis.com` | `gcloud services list --enabled --filter="name:iap.googleapis.com" --format="value(name)" --project=PROJECT_ID` |
| `cloudbuild.googleapis.com` | `gcloud services list --enabled --filter="name:cloudbuild.googleapis.com" --format="value(name)" --project=PROJECT_ID` |
| `artifactregistry.googleapis.com` | `gcloud services list --enabled --filter="name:artifactregistry.googleapis.com" --format="value(name)" --project=PROJECT_ID` |
| `aiplatform.googleapis.com` | `gcloud services list --enabled --filter="name:aiplatform.googleapis.com" --format="value(name)" --project=PROJECT_ID` |
| `iam.googleapis.com` | `gcloud services list --enabled --filter="name:iam.googleapis.com" --format="value(name)" --project=PROJECT_ID` |

**Expected:** Each check returns the API name. If any is empty, enable all of them at once:

```bash
gcloud services enable \
  compute.googleapis.com \
  run.googleapis.com \
  iap.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  aiplatform.googleapis.com \
  iam.googleapis.com \
  --project=PROJECT_ID
```

**Timing note:** first-time enablement of an API can take 1-2 minutes per
API. This is normal `gcloud`/GCP behavior, not something to retry — `gcloud
services enable` already blocks until the enable operation completes, so do
not add retry loops around it. If you are executing these commands from an
agent harness with a short default tool-call timeout (commonly ~2 minutes),
run the enable/preflight commands with an extended timeout, or run them in
the background and poll for completion, so the harness timeout doesn't get
mistaken for a `gcloud` failure.

### 2.4 Billing

```bash
gcloud billing projects describe PROJECT_ID --format='value(billingEnabled)'
```

**Expected:** `True`

**If `False` or empty:** Tell the user that billing must be enabled on the
project before deployment can proceed. Direct them to the GCP Console billing
page.

### 2.5 Compute quota (optional)

The `regions.get` API's quota entries are shaped `{metric, limit, usage,
owner}` — there is no `name` field, so a `quotas[name=CPUS]` projection
silently matches nothing (exits 0 with empty output). `[key=value]` bracket
filtering also isn't valid projection syntax here, and `describe` commands
don't accept a top-level `--filter` flag (that's `list`-only) — so pick the
row out with `--flatten` plus a client-side filter instead:

```bash
gcloud compute regions describe REGION --project=PROJECT_ID \
  --flatten='quotas[]' --format='value(quotas.metric,quotas.limit,quotas.usage)' \
  | awk '$1=="CPUS"{print "limit="$2" usage="$3}'
```

Verify that the available CPU quota (limit minus usage) is sufficient for the
chosen machine type (4 CPUs for small, 16 for medium). If quota is tight, warn
the user and suggest a different region.

### 2.6 Organization & IAP Compatibility

Check whether your account's domain matches the project's organization:

```bash
ANCESTORS="$(gcloud projects get-ancestors PROJECT_ID --format='value(id,type)')"
ORG_ID="$(echo "$ANCESTORS" | awk '$2=="organization"{print $1; exit}')"
if [[ -z "$ORG_ID" ]]; then
  echo "WARNING: Project has no organization. IAP requires a custom OAuth client."
else
  ORG_DOMAIN="$(gcloud organizations describe "$ORG_ID" --format='value(displayName)')"
  echo "Organization domain: $ORG_DOMAIN"
fi
```

**If the project has no organization, or if the deployer's email domain does not
match the organization domain:** IAP will require a custom OAuth client before
the deployer can log in. See Section 7 (Troubleshooting) for setup instructions.
Inform the user immediately — do not wait until after deployment.

---

## 3. Gather Deployment Details

Ask the user each question below in natural conversation. Use the defaults when
the user does not have a preference. Validate each answer before moving on.

> **Agent optimization:** Rather than prompting the user for each question
> individually (10 round trips), detect defaults from the ambient GCP environment
> first, then present the full candidate configuration as a single table and ask
> for confirmation or targeted overrides in one prompt:
>
> ```bash
> # Detect defaults
> gcloud config get-value project        # -> project_id
> gcloud config get-value compute/region # -> region
> gcloud config get-value account        # -> admin_email
> ```

| # | Question | Default | Validation | Config field |
|---|----------|---------|------------|--------------|
| 1 | GCP project ID | Current gcloud project | Must be a valid, accessible project (verified in preflight) | `project_id` |
| 2 | Hub name | `dev` (or suggest based on project) | Must be <= 20 chars, start with a lowercase letter, contain only lowercase letters, numbers, and hyphens. Regex: `^[a-z][a-z0-9-]*$` | `hub_name` |
| 3 | GCP region | `us-central1` | Must be a valid GCP region. Offer: `us-central1`, `us-east1`, `europe-west1`, `asia-east1` | `region` |
| 4 | Machine size | `small` | Must be `small` or `medium`. Explain: **small** = e2-standard-4 (4 vCPU, 16 GB, up to ~10 agents). **medium** = n2-standard-16 (16 vCPU, 64 GB, up to ~50 agents). | `machine_size` |
| 5 | Disk size in GB | `200` | Must be a positive integer | `disk_size_gb` |
| 6 | Container images: build on VM or use a registry? | `build` (build on VM) | Must be `build` or `registry`. If `registry`, ask for the registry path (e.g., `us-docker.pkg.dev/my-project/scion`). | `container_images.source`, `container_images.registry` |
| 7 | Admin email | Active gcloud account | Must be a valid email address | `admin_email` |
| 8 | Update policy | `auto` | Must be `auto`, `notify`, or `disabled`. Explain: **auto** = install updates automatically (recommended). **notify** = check for updates, show banner in admin UI. **disabled** = no automatic checking. | `update_policy` |
| 9 | Release channel | `nightly` | Must be `stable`, `preview`, or `nightly`. Defaults to nightly — only ask if the user wants to override. **stable** = GA releases. **preview** = pre-releases (rc, alpha, beta). **nightly** = nightly builds. | `release_channel` |
| 10 | Chat plugins | none (empty list) | Each must be one of: `telegram`, `discord`, `slack`, `teams`. Multiple allowed. | `chat_plugins` |

---

## 4. Generate Config File

After gathering all answers, write a JSON config file. Use the schema from
`scripts/single-node-vm/deploy-config.example.json`.

Write the file to `/tmp/scion-deploy-config.json`.

### Template

```json
{
  "hub_name": "HUB_NAME",
  "project_id": "PROJECT_ID",
  "region": "REGION",
  "machine_size": "MACHINE_SIZE",
  "disk_size_gb": DISK_SIZE_GB,
  "chat_plugins": [CHAT_PLUGINS],
  "container_images": {
    "source": "SOURCE",
    "registry": "REGISTRY"
  },
  "admin_email": "ADMIN_EMAIL",
  "update_policy": "UPDATE_POLICY",
  "release_channel": "RELEASE_CHANNEL"
}
```

Replace each placeholder with the gathered value:

| Placeholder | Source |
|-------------|--------|
| `HUB_NAME` | Question 2 answer |
| `PROJECT_ID` | Question 1 answer |
| `REGION` | Question 3 answer |
| `MACHINE_SIZE` | Question 4 answer (`small` or `medium`) |
| `DISK_SIZE_GB` | Question 5 answer (integer, no quotes) |
| `CHAT_PLUGINS` | Question 9 answers as quoted, comma-separated strings, e.g., `"telegram", "slack"`. Use `[]` for none. |
| `SOURCE` | Question 6 answer (`build` or `registry`) |
| `REGISTRY` | Question 6 registry path if source is `registry`, otherwise `""` |
| `ADMIN_EMAIL` | Question 7 answer |
| `UPDATE_POLICY` | Question 8 answer |
| `RELEASE_CHANNEL` | Question 9 answer if the user explicitly chose a channel, otherwise `""` (defaults to nightly) |

Write the file (substituting the gathered values into the template above,
in place of `# (insert populated JSON here)`):

```bash
cat > /tmp/scion-deploy-config.json << 'EOF'
# (insert populated JSON here)
EOF
```

Verify the file parses:

```bash
python3 -c "import json; json.load(open('/tmp/scion-deploy-config.json'))" && echo "Config valid"
```

**If validation fails:** Fix the JSON syntax and retry.

Show the user the generated config and ask them to confirm before proceeding.

---

## 5. Run Deployment

### 5.1 Get the repository

If not already in a clone of the Scion repository, clone it:

```bash
git clone https://github.com/GoogleCloudPlatform/scion.git /tmp/scion-repo
cd /tmp/scion-repo
```

If the repo is already available, use it directly.

### 5.2 Run the deploy script

```bash
bash scripts/single-node-vm/deploy.sh --config /tmp/scion-deploy-config.json
```

**What to expect:**

- The script runs 5 phases: Prerequisites, GCP Resources, VM Setup, IAP Proxy,
  Finalize.
- Total time: 10-20 minutes for a registry-based deployment.
- If building images locally: add 10-15 minutes for the image build phase.
- The script outputs progress to stdout. Watch for phase transitions
  (`--- Phase N: ... ---`).

**Do not interrupt the script.** If it fails, read the error output and consult
section 7 (Troubleshooting) before retrying.

### 5.3 Capture outputs

When the script completes successfully, it prints a summary block. Capture these
values from the output:

| Value | Where to find it |
|-------|-----------------|
| Hub name | `Hub name:` line |
| Instance name | `Instance:` line |
| Zone | `Zone:` line |
| Proxy service name | `Proxy:` line |
| Access URL | `Access URL:` line |

Save these for verification in section 6.

---

## 6. Verify Deployment

Run each check after the deploy script completes.

### 6.1 VM is running

```bash
gcloud compute instances describe scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --format='value(status)'
```

**Expected:** `RUNNING`

**If not `RUNNING`:** Check the VM serial port output:
```bash
gcloud compute instances get-serial-port-output scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID | tail -50
```

### 6.2 Cloud Run proxy is deployed

```bash
gcloud run services describe scion-hub-HUB_NAME-iap-proxy \
  --region=REGION --project=PROJECT_ID \
  --format='value(status.url)'
```

**Expected:** A URL like `https://scion-hub-HUB_NAME-iap-proxy-HASH-REGION.a.run.app`

**Do not** treat a successful `curl .../healthz` on this `*.run.app` URL as
proof the proxy or hub is reachable. On Cloud Run, `/healthz` is answered
directly by the Google Front End (GFE), before the request ever reaches the
proxy container — it proves the Cloud Run service exists, nothing more. The
proxy binary deliberately exposes `/proxy-healthz` instead (see
`extras/cloudrun-iap-proxy/main.go`) for anyone who needs an uptime check
that actually reaches the proxy. The cheap way to check that IAP is
enforcing on this URL is in §6.5 below.

### 6.3 Hub health check

SSH to the VM and check the health endpoint:

```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='curl -s http://localhost:8080/healthz'
```

**Expected:** A successful response (HTTP 200).

**If it fails:** Check service logs:
```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='sudo journalctl -u scion-hub.service --no-pager -n 50'
```

### 6.4 Container images (if built locally)

If `container_images.source` was `build`, verify images exist on the VM:

```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='docker images | grep scion'
```

**Expected:** Rows for `localhost/scion/core-base`, `localhost/scion/scion-base`,
and `localhost/scion/scion-antigravity`, all tagged `latest`.

### 6.5 IAP access

A cheap way to confirm IAP is actually enforcing on the proxy URL, without
involving a browser, is a plain unauthenticated request to the site root
(not `/healthz` — see the note in §6.2, GFE answers that one, not IAP):

```bash
curl -sI "https://scion-hub-HUB_NAME-iap-proxy-HASH-REGION.a.run.app/"
```

**Expected:** Either a `302` redirect (to `accounts.google.com`, the
browser-oriented login flow) or a `401` with an `Invalid IAP credentials`
body. Either response is proof IAP is enforcing on the service. Do not use a
service-account identity-token `curl` recipe for this check — it requires
the IAP OAuth client ID, which is out of scope for a first deploy.

For end-to-end verification (does auth actually complete and does the UI
load), provide the access URL (from section 5.3) to the user and ask them to
open it in their browser.

**Expected:** The user is prompted to authenticate with their Google account,
then sees the Scion Hub UI.

**If the user gets a 403:** They may not have the IAP access binding. Run:
```bash
gcloud iap web add-iam-policy-binding \
  --resource-type=cloud-run \
  --service=scion-hub-HUB_NAME-iap-proxy \
  --region=REGION \
  --project=PROJECT_ID \
  --member=user:USER_EMAIL \
  --role=roles/iap.httpsResourceAccessor
```

**If the user gets a generic error page:** IAP may still be propagating. Wait
60 seconds and retry.

---

## 7. Troubleshooting

### Quick reference

| Symptom | Cause | Fix |
|---------|-------|-----|
| `Permission denied` on instance create | Insufficient project permissions | User needs `roles/compute.admin` or `roles/editor` on the project |
| `Quota exceeded` on instance create | Not enough compute CPU quota in the region | Request a quota increase via GCP Console, or try a different region |
| Cloud-init timeout (script waits > 5 min) | VM is slow to provision | SSH in and check: `gcloud compute ssh scion-hub-HUB_NAME --zone=ZONE --project=PROJECT_ID --command='sudo cloud-init status --long'` |
| `image_registry is not configured` | `settings.yaml` missing `image_registry` field | SSH to VM, verify `/home/scion/.scion/settings.yaml` has `image_registry: "localhost/scion"` for local builds or the registry path for registry builds |
| IAP proxy deploy fails | IAP API not enabled or missing OAuth consent screen | Run `gcloud services enable iap.googleapis.com --project=PROJECT_ID`. Check the OAuth consent screen is configured in GCP Console > APIs & Services > OAuth consent screen. |
| Hub health check fails after deploy | Binary crashed or settings invalid | SSH to VM, check logs: `gcloud compute ssh scion-hub-HUB_NAME --zone=ZONE --project=PROJECT_ID --command='sudo journalctl -u scion-hub.service --no-pager -n 50'` |
| Hub health check fails after restart | Settings or IAP audience mismatch | SSH to VM, verify settings: `gcloud compute ssh scion-hub-HUB_NAME --zone=ZONE --project=PROJECT_ID --command='cat /home/scion/.scion/settings.yaml'`. Confirm `auth.mode` is `proxy` and the `audience` string is correct. |
| `iam.serviceAccounts.create` denied | User lacks IAM admin role | User needs `roles/iam.serviceAccountAdmin` on the project |
| Image build fails with `muse-code` error | Build script tried to build all images including unsupported ones | Verify the deploy script builds only `core-base`, `scion-base`, and `scion-antigravity`. If running manually, use `--target` to select individual images. |
| SSH connection fails to VM | IAP tunnel access not granted or firewall rule missing | Verify IAP tunnel role: `gcloud projects get-iam-policy PROJECT_ID --flatten="bindings[].members" --filter="bindings.role:roles/iap.tunnelResourceAccessor" --format="value(bindings.members)"`. Verify firewall rule exists: `gcloud compute firewall-rules describe scion-hub-HUB_NAME-allow-iap-ssh --project=PROJECT_ID`. |
| `403 Forbidden` accessing the hub URL | User missing IAP access binding | Grant access: `gcloud iap web add-iam-policy-binding --resource-type=cloud-run --service=scion-hub-HUB_NAME-iap-proxy --region=REGION --project=PROJECT_ID --member=user:USER_EMAIL --role=roles/iap.httpsResourceAccessor` |
| VM has no outbound internet | Cloud NAT not created or misconfigured | Verify router and NAT exist: `gcloud compute routers nats describe scion-hub-HUB_NAME-nat --router=scion-hub-HUB_NAME-router --region=REGION --project=PROJECT_ID` |
| IAP auth fails outright, or shows an unexpected consent screen | Deployer account is in a different GCP organization than the target project, or the project is not in a GCP organization at all | See "Cross-org IAP" below. `deploy.sh` prints a warning during Phase 2 for both cases it can detect (no-org is a certain warning; cross-domain is a heuristic) — either check is skipped silently if ancestry/org metadata can't be read, so trust the symptom over the absence of the warning. |

### Cross-org IAP: custom OAuth client required

**Symptom:** authentication through the Cloud Run IAP proxy fails outright,
or shows an "access blocked" / unexpected consent screen instead of the
expected sign-in flow.

**Why it happens:** IAP's default OAuth client is Google-managed and only
covers same-organization use. Per [Google's IAP custom OAuth
documentation](https://cloud.google.com/iap/docs/custom-oauth-configuration),
a custom OAuth client is required when:

- the deployer's account is outside the project's GCP organization, or
- **the project is not in a GCP organization at all** — this is the common
  first-deploy shape (a personal account on an OSS/sandbox project), and
  unlike the cross-org case it is a certain failure, not a heuristic.

This is unrelated to any IAM role or IAP binding —
`roles/iap.httpsResourceAccessor` can be correctly granted and auth will
still fail.

`deploy.sh` detects both cases ahead of time (Phase 2):

- **No organization:** if the project has no GCP organization, it always
  warns (unless the project's ancestry can't be read) — the Google-managed
  client can never work here, so there's nothing to guess.
- **Cross-org:** if the project does have an organization, it compares the
  deployer's email domain against the organization's domain (via `gcloud
  projects get-ancestors` and `gcloud organizations describe`) and warns on
  a mismatch. For service-account deployers, this domain comparison is
  skipped — comparing a service account's domain to an org isn't a
  meaningful check — but the no-org warning above still applies to them
  like anyone else. This part is a heuristic, not a guarantee, with two known
  failure modes:
  - **False negative:** if either gcloud call fails (a permissions gap is
    common — reading organization metadata needs a role the deployer may
    not have even when they can deploy fine otherwise), the check silently
    skips rather than guessing.
  - **False positive:** the organization domain compared against is its
    *primary* Workspace domain. A deployer on a secondary or alias domain of
    the same Workspace organization (or a subdomain) is legitimately in-org
    but will still trigger the warning.

Treat the actual symptom above as authoritative over the presence or absence
of either warning.

**Fix:** the default OAuth consent screen cannot be made to accept
cross-org or no-org accounts. A custom OAuth client (with its own consent
screen configuration) must be created for the project and IAP must be
configured to use it, instead of the default. This is a manual, one-time GCP
Console operation. The simplest path is directly from the Cloud Run service:

1. Navigate to **Cloud Run** in the GCP Console.
2. Click on the IAP proxy service (`scion-hub-HUB_NAME-iap-proxy`).
3. Go to the **Security** tab > **Identity-Aware Proxy** section.
4. Use the inline OAuth consent screen and client configuration presented
   there — redirect URIs are pre-populated for this service.

If that inline flow isn't available in your Console version, or you need to
configure the consent screen or client independently of a specific service,
use the longer manual path instead:

1. In the target project's GCP Console, go to **APIs & Services > OAuth
   consent screen** and configure a consent screen that includes the
   deployer's account (e.g. an "External" user type with the deployer added
   as a test user, or a configuration appropriate for your organization's
   policy).
2. Go to **APIs & Services > Credentials** and create an OAuth 2.0 Client ID
   of the type IAP expects for the resource (web application).
3. Under **Security > Identity-Aware Proxy**, associate the custom OAuth
   client with the Cloud Run service (`scion-hub-HUB_NAME-iap-proxy`)
   instead of the project's default-generated client.

See [Google's IAP custom OAuth
documentation](https://cloud.google.com/iap/docs/custom-oauth-configuration)
for the exact consent-screen and OAuth-client steps for your GCP Console
version — the menu paths above can shift between Console releases.
`deploy.sh` does not and will not automate this step; it is
detection-and-warning only.

### Diagnostic commands

Check VM status:
```bash
gcloud compute instances describe scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --format='value(status)'
```

Check Hub service status:
```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='sudo systemctl status scion-hub.service'
```

View Hub logs:
```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='sudo journalctl -u scion-hub.service --no-pager -n 100'
```

Check cloud-init status:
```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='sudo cloud-init status --long'
```

Check settings.yaml:
```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='cat /home/scion/.scion/settings.yaml'
```

Check Docker images:
```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='docker images'
```

---

## 8. Cleanup

To tear down all resources created by the deployment, run:

```bash
bash scripts/single-node-vm/deploy.sh --delete --config /tmp/scion-deploy-config.json
```

Or run without a config file (the script prompts for hub name and region):

```bash
bash scripts/single-node-vm/deploy.sh --delete
```

### What gets deleted

| Resource | Name pattern |
|----------|-------------|
| Cloud Run IAP proxy service | `scion-hub-HUB_NAME-iap-proxy` |
| GCE VM instance | `scion-hub-HUB_NAME` |
| Cloud NAT | `scion-hub-HUB_NAME-nat` |
| Cloud Router | `scion-hub-HUB_NAME-router` |
| Service account | `scion-hub-HUB_NAME@PROJECT_ID.iam.gserviceaccount.com` |
| IAP SSH firewall rule | `scion-hub-HUB_NAME-allow-iap-ssh` |

### What is intentionally NOT deleted

- **IAP tunnel role** (`roles/iap.tunnelResourceAccessor`) on the deployer
  account. This role may be used for SSH access to other VMs in the project.

To remove it manually:

```bash
gcloud projects remove-iam-policy-binding PROJECT_ID \
  --member=user:USER_EMAIL \
  --role=roles/iap.tunnelResourceAccessor
```
