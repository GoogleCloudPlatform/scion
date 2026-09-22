# Agent Runbook: Single-Node VM Deployment

Deploy a Scion Hub on a single GCE VM with IAP authentication. This runbook is
written for an AI agent to read and execute end-to-end. Follow the sections in
order.

For architecture details, access patterns, and configuration reference, see
[docs/deploy/single-node-vm.md](single-node-vm.md).

---

## 1. Prerequisites

Verify each tool is available before proceeding. If any check fails, stop and
tell the user what is missing.

| Tool | Check command | Expected output |
|------|--------------|-----------------|
| gcloud CLI | `gcloud --version` | Version string (any version) |
| bash | `bash --version` | Version string (any version) |
| python3 | `python3 --version` | Version string (3.6+) |
| PyYAML | `python3 -c "import yaml; print(yaml.__version__)"` | Version string (any version) |
| curl | `curl --version` | Version string (any version) |

If PyYAML is missing, install it one of these ways:

| Option | Command | Notes |
|--------|---------|-------|
| System package (Debian/Ubuntu) | `apt-get install python3-yaml` | Preferred; avoids PEP 668 entirely. |
| Virtualenv | `python3 -m venv ~/.venv && ~/.venv/bin/pip install pyyaml && PYTHON=~/.venv/bin/python3 bash deploy.sh ...` | Use when you cannot install system packages; pass `PYTHON=` when invoking `deploy.sh`. |
| Per-user install (where allowed) | `pip install --user pyyaml` | Fails under PEP 668 ("externally-managed-environment") on Debian >= 12, Ubuntu >= 23.04, and Homebrew Python — prefer one of the options above on those systems. |

`deploy.sh` itself never creates a virtualenv; it only reads `PYTHON` from
the environment (default `python3`) to locate the interpreter with PyYAML
installed.

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

Check each API. If any is not enabled, enable it.

| API | Check | Enable |
|-----|-------|--------|
| `compute.googleapis.com` | `gcloud services list --enabled --filter="name:compute.googleapis.com" --format="value(name)" --project=PROJECT_ID` | `gcloud services enable compute.googleapis.com --project=PROJECT_ID` |
| `run.googleapis.com` | `gcloud services list --enabled --filter="name:run.googleapis.com" --format="value(name)" --project=PROJECT_ID` | `gcloud services enable run.googleapis.com --project=PROJECT_ID` |
| `iap.googleapis.com` | `gcloud services list --enabled --filter="name:iap.googleapis.com" --format="value(name)" --project=PROJECT_ID` | `gcloud services enable iap.googleapis.com --project=PROJECT_ID` |
| `iam.googleapis.com` | `gcloud services list --enabled --filter="name:iam.googleapis.com" --format="value(name)" --project=PROJECT_ID` | `gcloud services enable iam.googleapis.com --project=PROJECT_ID` |

**Expected:** Each check returns the API name. If empty, run the enable command.

### 2.4 Billing

```bash
gcloud billing projects describe PROJECT_ID --format='value(billingEnabled)'
```

**Expected:** `True`

**If `False` or empty:** Tell the user that billing must be enabled on the
project before deployment can proceed. Direct them to the GCP Console billing
page.

### 2.5 Compute quota (optional)

```bash
gcloud compute regions describe REGION --project=PROJECT_ID \
  --format='value(quotas[name=CPUS].limit,quotas[name=CPUS].usage)'
```

Verify that the available CPU quota (limit minus usage) is sufficient for the
chosen machine type (4 CPUs for small, 16 for medium). If quota is tight, warn
the user and suggest a different region.

---

## 3. Gather Deployment Details

Ask the user each question below in natural conversation. Use the defaults when
the user does not have a preference. Validate each answer before moving on.

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
| 9 | Release channel | auto-detect from version | Must be `stable`, `preview`, or `nightly`. Usually auto-detected — only ask if the user wants to override. **stable** = GA releases. **preview** = pre-releases (rc, alpha, beta). **nightly** = nightly builds. | `release_channel` |
| 10 | Chat plugins | none (empty list) | Each must be one of: `telegram`, `discord`, `slack`, `teams`. Multiple allowed. | `chat_plugins` |

---

## 4. Generate Config File

After gathering all answers, write a YAML config file. Use the schema from
`scripts/single-node-vm/deploy-config.example.yaml`.

Write the file to `/tmp/scion-deploy-config.yaml`.

### Template

```yaml
# Scion single-node VM deployment configuration
hub_name: "HUB_NAME"
project_id: "PROJECT_ID"
region: "REGION"
machine_size: "MACHINE_SIZE"
disk_size_gb: DISK_SIZE_GB
chat_plugins: [CHAT_PLUGINS]
container_images:
  source: "SOURCE"
  registry: "REGISTRY"
admin_email: "ADMIN_EMAIL"
update_policy: "UPDATE_POLICY"
# Only include if the user explicitly chose a channel (usually auto-detected)
# release_channel: "stable"
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

Write the file:

```bash
cat > /tmp/scion-deploy-config.yaml << 'EOF'
# (insert populated YAML here)
EOF
```

Verify the file parses:

```bash
python3 -c "import yaml; yaml.safe_load(open('/tmp/scion-deploy-config.yaml'))" && echo "Config valid"
```

**If validation fails:** Fix the YAML syntax and retry.

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
bash scripts/single-node-vm/deploy.sh --config /tmp/scion-deploy-config.yaml
```

**What to expect:**

- The script runs 5 phases: Prerequisites, GCP Resources, VM Setup, IAP Proxy,
  Finalize.
- Total time: 10-20 minutes for a registry-based deployment.
- If building images locally: add 30-45 minutes for the image build phase.
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

Provide the access URL (from section 5.3) to the user and ask them to open it
in their browser.

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
bash scripts/single-node-vm/deploy.sh --delete --config /tmp/scion-deploy-config.yaml
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
