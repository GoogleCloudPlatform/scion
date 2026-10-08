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
| jq | `jq --version` | Version string (any version) |

`jq` is required for the Cloud Router/Cloud NAT reuse check (see below) --
`deploy.sh` checks for it up front in Phase 1, before any prompts or API
calls, and fails immediately rather than guessing if it's missing.

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
manually. `deploy.sh` itself lists what's already enabled first and only
calls `services enable` for whatever's actually missing (never an
unconditional call on every run, except that with the tier off it falls
back to enabling the full API list if the enabled-API list cannot be
read); if the hybrid tier is on, it also requires `container.googleapis.com`.

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
| `iamcredentials.googleapis.com` | `gcloud services list --enabled --filter="name:iamcredentials.googleapis.com" --format="value(name)" --project=PROJECT_ID` |

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
  iamcredentials.googleapis.com \
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
> individually (up to 12 round trips), detect defaults from the ambient GCP environment
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
| 11 | Let the hub mint service accounts for agents? | `true` | Optional; only ask if the user raises it. Must be `true` or `false`. `true` grants the hub VM's service account `roles/iam.serviceAccountAdmin` on the project; `false` skips the grant, and the hub cannot mint service accounts. Explain the trade-off in §6.3c before deploying. | `hub_sa_minting` |
| 12 | Attach a GKE cluster (hybrid tier)? | No | Only ask if the user mentions running agents on Kubernetes. If yes: cluster name, location (zone or region), and project (default: same as `project_id`; a different project is not supported yet); the Kubernetes namespace (default `scion-hub-<hub_name>`) and PersistentVolumeClaim name (default `scion-hub-<hub_name>-shared`) for the shared tree. The cluster must already exist and be on the same VPC network as the hub VM (`default`, today) — this script never creates or deletes a cluster. **Also requires `container_images.source: registry`** (Question 6) — GKE nodes cannot pull from the VM's local Docker store that `source: build` uses, and the node service account needs `roles/artifactregistry.reader` (or equivalent read access) on that registry. | `gke_target.name`, `gke_target.location`, `gke_target.project`, `gke_target.namespace`, `gke_target.pvc_name` |

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
  "release_channel": "RELEASE_CHANNEL",
  "hub_sa_minting": HUB_SA_MINTING,
  "gke_target": {
    "name": "GKE_NAME",
    "location": "GKE_LOCATION",
    "project": "GKE_PROJECT"
  }
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
| `CHAT_PLUGINS` | Question 10 answers as quoted, comma-separated strings, e.g., `"telegram", "slack"`. Use `[]` for none. |
| `SOURCE` | Question 6 answer (`build` or `registry`) |
| `REGISTRY` | Question 6 registry path if source is `registry`, otherwise `""` |
| `ADMIN_EMAIL` | Question 7 answer |
| `UPDATE_POLICY` | Question 8 answer |
| `RELEASE_CHANNEL` | Question 9 answer if the user explicitly chose a channel, otherwise `""` (defaults to nightly) |
| `HUB_SA_MINTING` | Question 11 answer as a bare JSON boolean (`true` or `false`, no quotes); `true` if not asked |
| `GKE_NAME` | Question 12 answer, or `""` if the hybrid tier was declined (omit the whole `gke_target` block in that case) |
| `GKE_LOCATION` | Question 12 answer |
| `GKE_PROJECT` | Question 12 answer, or `""` to default to `PROJECT_ID` |

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

## Hybrid Tier (Optional): GKE Attach and NFS Firewall Rules

Most of this section applies only if the user answered yes to Question
11. Otherwise, read "Some behaviour applies whether or not the tier is on"
and "Teardown (`--delete`)" below, which also apply with the tier off.

### What it does

The hybrid tier attaches an **existing** GKE cluster as a second runtime
alongside the VM, so agents can run in either place while sharing project
scratchpads over NFS served from the hub VM. `deploy.sh` never creates or
deletes the cluster itself — it is always a manual prerequisite the user
sets up beforehand, in the same GCP project as the hub and on the hub VM's
network (`default`, today). The NFS export is reachable only at the hub
VM's internal IP inside that VPC, so any broker relying on this shared
volume must also run on it.

**Prerequisites:**
- An existing GKE cluster (Standard or Autopilot) in the hub's own GCP
  project, on the hub VM's network.
- `container_images.source: registry` with a registry path the cluster's
  node service account can read (`roles/artifactregistry.reader` or
  equivalent) — GKE nodes cannot pull from the VM's local Docker image
  store that `source: build` uses, so `build` is refused outright when the
  tier is on.
- The base required APIs, plus `container.googleapis.com`; see
  [2.3 Required APIs](#23-required-apis).
- `kubectl` and the `gke-gcloud-auth-plugin` (`gcloud components install
  gke-gcloud-auth-plugin`, or your package manager's equivalent) on the
  machine running `deploy.sh` — required for every Kubernetes object check
  this tier makes, both on create and on `--delete`; their absence is
  checked before the first `kubectl` call, with an actionable message
  naming whichever is missing.

Enabling the tier does six things, all additive:

1. **Discovery.** After enabling any missing APIs and the IAP service
   identity, and before creating anything else, the script confirms the
   cluster exists, checks that its network matches the hub VM's, and
   discovers the node tag GKE assigns, read from the cluster's GKE-managed firewall
   rules — the same source and the same check for both a Standard and an
   Autopilot cluster. It lists the firewall rules on the cluster's
   network and looks for the one rule matching `gke-<suffix>-all`, with
   direction INGRESS, whose source ranges include the cluster's own pod
   CIDR (pod CIDRs are unique within a VPC, which is what ties the rule
   to this cluster). That rule must carry exactly one target tag,
   matching `gke-<suffix>-node`, and the matching `gke-<suffix>-vms` rule
   must exist with the same single target tag. If no rule matches, more
   than one does, the tag shape doesn't match, or the two rules disagree,
   the script refuses to guess and fails, listing whatever it found and
   naming this as something to fix on the cluster's own firewall rules,
   not in `deploy.sh`. If the cluster can't be found or its network
   doesn't match, it also fails at this point. The same
   cluster description is also used to read the cluster's pod CIDR
   (`clusterIpv4Cidr`, cross-checked against
   `ipAllocationPolicy.clusterIpv4CidrBlock` — the script fails if the two
   disagree or if the range is missing, broader than `/8`, or not valid
   IPv4), which is what scopes the hub-deny firewall rule below to the
   cluster's own pod traffic.
2. **Firewall rules, VM tag, and a static internal IP.** Three firewall
   rules are created, all scoped to this hub by an exact
   `scion-deployment=<hub_name>` marker in their description and a
   `scion-hub-<hub_name>-nfs` target tag, which the hub VM also receives
   (at creation, or via an idempotent `add-tags` on an existing VM):
   - `scion-hub-<hub_name>-nfs-allow` — allows tcp:2049 (NFS) from the
     cluster's discovered node tag, priority 900.
   - `scion-hub-<hub_name>-nfs-deny` — denies tcp:2049 from everywhere
     else (`0.0.0.0/0`), priority 950.
   - `scion-hub-<hub_name>-hub-deny` — denies all protocols and ports
     from the cluster's discovered pod CIDR, priority 950, the same
     scheme as `nfs-deny` (so it beats a network's own
     default-allow-internal rule). GKE agent pods reach the hub through
     its existing public IAP URL, the same URL browser users use, not a
     private VPC path; this rule denies direct traffic from the cluster's
     pod range to the hub VM. NFS mounts are unaffected:
     the kubelet mounts the volume from the node's own primary address,
     which `nfs-allow` admits at priority 900, before either deny rule.
     The rule covers the cluster's default pod range (`clusterIpv4Cidr`)
     only. Pods on a node pool with its own pod range, or on an
     additional pod range added to the cluster, are not covered, and
     neither is pod traffic that leaves with the node's address
     (host-network pods, or traffic source-NATed to the node). For a
     cluster with more than one pod range, add an equivalent deny rule
     for each additional range (same target tag, priority 950).
     Deploy-time, before any of the three hybrid-tier rules is created, a
     fail-closed check reads the Cloud Run IAP proxy's own egress subnet
     and refuses to continue if that subnet's primary range would
     overlap the discovered pod CIDR, since that would also block the
     proxy's own traffic to the hub. See "Agent transport auth" below
     for how agent pods actually authenticate over that public URL.

   The two NFS rules are created deny first, then allow, so an
   interrupted run can never leave the allow rule in place without its
   paired deny; hub-deny is created alongside nfs-deny, since it has no
   allow to protect.

   If a firewall rule with one of these names already exists but doesn't
   carry the exact marker, the script refuses to touch it and fails rather
   than adopting a rule it doesn't recognize as its own. If it carries the
   marker, the script additionally verifies its full security-relevant
   spec (direction, action, every allow/deny entry, source tags, source
   ranges, source/target service accounts, destination ranges, disabled,
   target tags, priority, network) against what this tier expects, and
   fails — listing exactly what differs, plus the commands to fix it —
   rather than silently correcting a rule that has drifted from that spec
   (for example, after the cluster was recreated with a new node tag or
   pod CIDR). The fix-it commands include an in-place `update` only when
   running it would converge to exactly the expected rule; otherwise only
   a delete command is offered (deploy.sh recreates the rule correctly on
   the next run).

   The hub VM's internal IP is also reserved as a static address,
   `scion-hub-<hub_name>-internal-ip`, marked with an exact
   `scion-deployment=<hub_name>` description — the same token format the
   firewall rules, router, and service account use for their own
   markers. This marker is checked when an existing reservation is
   adopted and during teardown: an address with this name that lacks it
   is refused on create and blocks teardown, the same as an unmarked
   firewall rule. On a fresh VM, a free
   address is reserved first
   and the VM is created with `--private-network-ip` pinned to it; on an
   existing VM, its current internal IP is promoted into a reservation of
   the same name (`gcloud compute addresses create ... --addresses
   <current-ip>`), and the script re-reads the VM afterward to confirm the
   IP didn't change. Either way the address stays reserved for this hub,
   and a VM that `deploy.sh` creates later is pinned to it. A reserved
   address with this name that doesn't carry the marker, or whose
   reserved IP no longer matches the VM's actual IP, fails the run with
   the mismatch and the remediation, exactly like a drifted firewall rule
   — never auto-corrected. Reserving this address is what gives the
   shared NFS PV's server field a stable target: without it, a VM
   recreate could leave the PV pointing at a dead address.

3. **Agent transport auth.** Agents dispatched to the GKE cluster reach
   the hub through the same public IAP URL browser users use,
   authenticating with a Google OIDC ID token minted by impersonating a
   dedicated transport service account, marked with an exact
   `scion-deployment=<hub_name>` description like the firewall rules and
   the internal IP reservation (service accounts have no labels). Its id is
   `scion-tp-<prefix>-<hash>`: the first 12 characters of the hub name
   (trailing hyphens trimmed), then an 8-hex-digit checksum of the full
   hub name. That keeps the id within the 30-character service account
   limit for any hub name, distinct for hubs whose names share a long
   prefix, and never equal to a hub's own `scion-hub-*` service account.
   To print it for a hub:
   `bash -c 'source scripts/single-node-vm/hybrid-tier.sh; hybrid_transport_sa_name HUB_NAME'`. Setup: the project's IAP OAuth client ID is read
   (`gcloud iap settings get --resource-type=iap_web`) and used verbatim
   as the transport token's audience; the hub's own runtime service
   account is granted `roles/iam.serviceAccountOpenIdTokenCreator` on
   the transport SA specifically (not project-wide, and not the broader
   `serviceAccountTokenCreator`, which this never needs); once the Cloud
   Run proxy exists, the transport SA is granted
   `roles/iap.httpsResourceAccessor` on it, the same role and call the
   human operator's own access uses. `server.auth.transport` in
   `settings.yaml` records the audience and the transport SA email.
   IAM changes can take on the order of a minute to propagate; the very
   first agent dispatch right after a deploy may see a transient
   authentication failure that a retry resolves. A redeploy does not
   delete the transport SA or its grants, so turning the tier off on a
   later redeploy of the same hub leaves the transport SA and its grants in place (the
   settings no longer reference it); `--delete` removes them, tier on or
   off (see Teardown below).

4. **NFS server and export.** Discovery also reads the cluster's node
   subnet's primary IP range (never the pod CIDR), refusing anything
   `0.0.0.0/0` or broader than `/8`. Once the VM exists, a dedicated
   `scion-nfs` system account (`nologin` shell, primary group `scion`, a
   system-range uid distinct from both `scion`'s own uid and uid 0) is
   created without a home directory for NFS's `all_squash` identity --
   or, if it already existed from an earlier run, checked for that
   shell, group and uid, refusing to continue if a pre-existing account
   doesn't meet them.
   `/srv/scion-shared` (the export root) is the root of its own
   dedicated, size-capped ext4 filesystem (default 20G, configurable via
   `gke_target.shared_dir_image_size_gb`), loop-mounted from a single
   image file that itself lives on the VM's boot disk; the image's space
   is reserved with `fallocate` (not a sparse `truncate`), failing
   clearly and removing the partial file if the boot disk can't hold it,
   and formatted only the first time -- never re-created on a later run.
   A pre-existing `/etc/fstab` line for the export root
   (`/srv/scion-shared`) with different
   options is never silently trusted or replaced; the run fails with the
   options it expected. The export is only ever written or activated
   once the mount is confirmed, including a check that whatever is
   mounted at the export root is actually the loop device backing this
   image (via `findmnt`/`losetup`), not a stray leftover mount. A
   `scion-hub.service` drop-in (tier-on only) adds
   `RequiresMountsFor=/srv/scion-shared`, so the hub itself can never
   start against an unmounted export and write shared-dir paths to the
   boot disk's root filesystem instead. If a run is interrupted between
   creating the image and finishing this mount setup, remove the image
   file and re-run rather than trying to reuse a half-created one --
   the create step's own guard refuses to reformat an image that already
   exists, so a partial image is otherwise never retried automatically.
   Growing the image later is a manual, documented operation (grow the
   image file, then `resize2fs`) -- this script never shrinks it.
   `nfs-kernel-server` is installed, NFSv2/v3/4.0 and UDP are disabled
   and `rpcbind` is masked, with the mask verified rather
   than assumed; the server is restarted when needed after
   writing this config, since the package's own install already starts
   it with the stock config beforehand. A per-hub file under
   `/etc/exports.d/` exports the mounted root to just the node subnet,
   squashing every client to the dedicated identity. There's no separate
   teardown for the export or its backing image file: both are deleted
   along with the VM's boot disk.
5. **Kubernetes objects.** A cluster-scoped PersistentVolume
   (`scion-hub-<hub_name>-shared`), a namespace (default
   `scion-hub-<hub_name>`), and a PersistentVolumeClaim in that namespace
   (default `scion-hub-<hub_name>-shared`, bound to the PV) are labeled
   `scion-deployment=<hub_name>` when this script creates them. The
   ownership checks run in two parts: everything that doesn't depend on the VM's IP -- kubectl/plugin
   presence, credentials, whether an existing PV or PVC with the target
   name carries this deployment's marker, and every identity field except
   the PV's NFS server address -- runs right after discovery, before the
   VM, NFS export, or firewall rules are created, and creates the
   namespace at that point if it's missing (once the PV/PVC checks pass).
   The PV and PVC themselves are created once the VM's IP is known,
   later, since the PV's identity includes it. An existing PV or PVC with
   the target name but no marker refuses the run, as early as the PV/PVC
   marker check above can catch it; a marked one whose identity has
   drifted (the NFS server IP after a VM recreate, for example) also
   refuses, with the fields that differ and the remediation. An existing,
   unmarked namespace is used as-is and never adopted or deleted.

6. **settings.yaml.** Both writes (the initial dev-mode one and the later
   proxy-mode update) add a `server.shared_dir_storage` block (backend
   `nfs`, pointing at the VM's export and the PV the Kubernetes objects
   above create) using the schema already defined for it in the runtime's
   own settings package. The tier does not write the hub's `gke` runtime
   or profile settings; configure the `gke` runtime separately. With the
   hybrid tier enabled, the hub default GCP identity mode is `block`.
   Configure a project-level mode (`assign` with a dedicated service
   account) for projects that need GCP access. The hub-level
   `passthrough` default applies to hubs deployed without the hybrid
   tier. GKE agents reach the hub through its public IAP URL (item 3);
   the internal IP reserved in item 2 is the NFS server address in the
   PV and in `server.shared_dir_storage`, and the Cloud Run proxy's
   backend address.

   **Restricted user access.** With the tier on, both writes also set
   `server.auth.user_access_mode`: `invite_only` by default, or
   `domain_restricted` when the config file sets it, plus
   `server.auth.authorized_domains` when the config file lists any. The
   config keys are top-level and optional:

   ```json
   "user_access_mode": "domain_restricted",
   "authorized_domains": ["example.com", "*.example.org"]
   ```

   They apply only with the tier on; with the tier off they are ignored,
   with a warning, and the settings are unchanged. With the tier on,
   `deploy.sh` refuses, before creating anything: an empty
   `admin_email`; an `admin_email` that is a service account (ends in
   `gserviceaccount.com`); any other mode, including `open` and an empty
   value; `domain_restricted` with no domains; and any
   `authorized_domains` entry that is not a domain name or `*.domain`
   wildcard, or that matches service account addresses (a
   `gserviceaccount.com` domain, or a wildcard such as `*.com` that
   covers one). The `admin_email` account can sign in under either mode.
   It invites other users from the web UI's admin Users page,
   or with `scion hub invite create`.

Re-running the deploy script with the tier on against an existing hub
that predates the hybrid tier adds the hybrid firewall rules, the
internal IP reservation (promoted from the VM's current IP), the NFS
export, the Kubernetes objects, the `shared_dir_storage` settings and the
VM tag.

**Some behaviour applies whether or not the tier is on**, including:
- **Base resource markers.** When this script creates these base
  resources, it marks them `scion-deployment=<hub_name>`: a label on the
  VM; a description on the VM and Cloud Run proxy service accounts and
  the Cloud Router; and appended to the IAP SSH and proxy-to-VM rules'
  own descriptions. The Cloud Run proxy gets the label on a create when a
  lookup confirms the service is new. The Cloud NAT and the Artifact
  Registry repository are created without a marker. This marker is
  informational: it is not checked and does not affect adoption or
  teardown of those resources.
- **API enablement.** `deploy.sh` lists the APIs already enabled on the
  project and passes only the missing ones to `services enable`. With the
  tier off, if that list can't be read, it enables the full required set
  (see [2.3 Required APIs](#23-required-apis)).
- **Teardown.** `--delete` looks for the hybrid-tier firewall rules,
  internal IP reservation and agent transport service account, and
  removes the ones marked for this hub under the conditions described in
  "Teardown (`--delete`)" below, which also covers how base-resource
  deletes are reported.

**Known limits.** See `docs/deploy/hybrid-tier.md`'s own Known limits
section for `ptone/scion#1799` (image pinning: use `--image <digest>` at
agent start, not a profile's `harness_overrides`, since a template's own
image default wins over it for both Docker and GKE agents),
`ptone/scion#1800`, and `ptone/scion#1801`. For hardening an NFS tree that
was exported before a dedicated squash identity was in place, see that
same page's manual fix-up recipe under "E2 hardening: dedicated squash
identity + default ACL".

### Teardown (`--delete`)

Base-resource adoption is the same whether or not the tier is on.
`--delete` checks for the three hybrid firewall rules and the static
internal IP reservation, all by name, whether or not the current config
has the tier enabled — teardown has no other way to know whether the
tier was ever turned on for this hub. It also looks up the agent
transport service account by name (see below). These lookups add a
read-only `firewall-rules list` call, a read-only `addresses list` call
and a `service-accounts describe` call to a `--delete` run and, rarely,
can make it refuse to proceed (see below); they do not change which base
resources are deleted.

A base resource whose delete fails is reported on that step, as not
found only when that is positively confirmed, and is otherwise kept; a
kept base resource does not on its own make `--delete` exit non-zero.
A hybrid-tier firewall rule, internal IP reservation, transport service
account or its IAP access binding that is kept makes `--delete` exit
non-zero. The teardown summary reports outcomes such as deleted, not
found or kept.

Before deleting anything, `--delete` looks up the three hybrid firewall
rules and the internal IP reservation and prints a classification line
for each one found: `  found (marked): <name>` for a resource carrying
this hub's exact marker, or `  SKIPPED (unmarked): <name>` for a name
match that doesn't. A `SKIPPED (unmarked)` line from these checks fails
the whole teardown run before any resource is deleted (not just the
hybrid ones), since a naming collision on one of these names means the
hub name can no longer be trusted to identify only resources this
deployment owns. The same applies if either check itself can't
complete (a permissions error, for example): an unknown ownership state
is treated as a failure, never as "nothing to protect."

Marked firewall rules and the marked internal IP reservation are deleted
only once the hub VM is confirmed gone: deleted successfully, or
positively confirmed absent project-wide, not merely inferred from a
delete or describe call that happened to fail (which could just as
easily mean a wrong zone or a transient error) — never while the VM
might still exist or its fate is unknown, so the deny rule stays in
effect, and the reservation stays in place, for as long as the VM could
still be reachable (the reservation is also still attached to the VM's
network interface until the VM itself is gone, so deleting it earlier
would fail regardless). If the VM fails to delete, or its absence can't
be confirmed, all of the hybrid rules and the reservation are kept and
the run reports the failure, naming the reservation as kept rather than
omitting it silently. Firewall deletion order is the reverse of
creation: `nfs-allow`, then `hub-deny`, then `nfs-deny`. Deletion stops
at the first rule that isn't confirmed gone, so neither deny rule is
deleted after the allow rule's delete failed. If a marked `nfs-deny`
has drifted from its expected spec, the printed remediation deletes
`nfs-allow` first, then `nfs-deny`, then re-runs deploy.sh, so it does
not leave the allow rule in place without its deny. The GKE cluster
itself is never deleted by this script, under any circumstance.

If `gke_target.name` is set, `--delete` also tears down the Kubernetes
objects first, before any base resource: the PVC, then the PV, then the
namespace (only if it carries this deployment's marker -- an unmarked,
reused namespace is never deleted). Before deleting a marked PVC, the
teardown also refuses if any pod in its namespace still mounts it (the
GKE agent pods this tier exists to run are exactly the pods that would):
`kubectl delete pvc` blocks indefinitely under its own storage-protection
finalizer while a pod references the claim, so this is caught up front
with a "stop agents first" message rather than left to hang the whole
teardown. Every delete also carries a bounded `--timeout`, so a delete
that hangs for any other reason is treated as a failure rather than left
to block indefinitely. The same found/SKIPPED classification and
abort-before-any-delete rule applies to the PVC and PV; an unmarked
namespace is only skipped, not an abort, matching the create-side rule
that using an existing namespace is allowed. If the cluster itself is
confirmed gone (a positive NOT_FOUND), its objects are assumed to have
gone with it and nothing is checked; any other failure to reach the
cluster aborts the teardown before any delete.

A failure deleting any Kubernetes object stops the whole teardown right
there: Cloud Run, the VM, and every other base resource are left
untouched, and the run reports the failure and exits non-zero, since the
Kubernetes objects are deleted first specifically so a failure here can
still protect everything downstream. Running `--delete` interactively
(no `--config`) has no way to know whether a hybrid tier was ever
configured for the hub, since `gke_target.name` only exists in a config
file; that case prints a note naming the default namespace/PVC/PV names
and pointing at `--config` as the way to have them checked.

`--delete` also looks up the agent transport service account by name,
whether or not the current config has the tier on, after the Cloud Run
proxy, VM, NAT, router and base service account deletes; a Kubernetes
object delete failure stops teardown before this step. If it carries
this hub's marker, teardown first removes its IAP access binding on the
Cloud Run proxy (skipped once that service has been deleted, which
removes the binding with it), then deletes the service account; if the
binding removal fails, the service account is kept. A same-name service
account without the marker is never touched. Only a positive not-found
counts as absent: if the service account's state can't be read (a
permissions error, for example), or it is unmarked, or the binding
removal or the delete fails, it is reported as kept, with the reason,
and the run exits non-zero. When teardown reaches this step, the summary
prints `Deleted transport SA`, `Not found transport SA` or `Kept
transport SA (<reason>)`, plus, for a marked service account, a line for
its IAP access binding.

The NFS export itself has no separate teardown step: it's a dedicated image
file, an `/etc/fstab` entry loop-mounting it, and an `/etc/exports.d/` entry,
all on the hub VM's own boot disk, so they're deleted along with the VM.

### Testing this locally

`scripts/single-node-vm/tests/run.sh` runs the hybrid-tier logic against a
stubbed `gcloud` — no GCP project is contacted. Useful when validating a
change to `hybrid-tier.sh` before a real deployment. It needs bash 4 or
later, `python3`, and a Go toolchain: the `settings.yaml` parse tests run
`go run` on `tests/lib/settings-yaml-to-json.go` from the repository
root, which uses the repository's `github.com/knadh/koanf` YAML parser
module (downloaded into the Go module cache on first use if it is not
already there).

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

**Re-running on an existing deployment narrows the IAP SSH firewall rule.**
`scion-hub-HUB_NAME-allow-iap-ssh` is scoped to the hub VM's network tag.
Deployments from before this scoping existed had an unscoped rule that
allowed IAP-range SSH to *every* VM on network `default` in the project. A
re-run of `deploy.sh` (normally the first) narrows that rule in place to the
hub VM only — any other VM that was relying on it for IAP SSH loses that
access and needs its own firewall rule.

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

### 6.3a Hub-scoped agent env vars

Right after the Phase 3 health check, `deploy.sh` writes two hub-scoped env
vars into the hub database (`/home/scion/.scion/hub.db`) with `sqlite3`, run
as the `scion` user. Both use injection mode `always`, so every agent gets
them. Agents need them for Vertex AI inference:

| Key | Value |
|-----|-------|
| `GOOGLE_CLOUD_PROJECT` | `PROJECT_ID` |
| `GOOGLE_CLOUD_LOCATION` | `global` (intentional: the global Vertex AI endpoint) |

The rows are scoped to the hub instance ID, which is the `hub_id` field in
`/healthz`. They appear in the admin UI and under
`GET /api/v1/env?scope=hub`. The deploy only seeds the rows when they are
absent. Edits made in the admin UI are kept across redeploys, and a deleted row
is created again on the next deploy.
Verify:

```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command="sudo -u scion sqlite3 /home/scion/.scion/hub.db \"SELECT key, value, scope, scope_id, injection_mode FROM env_vars WHERE scope='hub' AND key LIKE 'GOOGLE_CLOUD_%'\""
```

**Expected:** Two rows, each with `scope` = `hub`, `scope_id` equal to the
`/healthz` `hub_id`, and `injection_mode` = `always`. The values are the ones
above unless an admin has edited them.

**If they are missing:** A warning in the deploy output includes the exact
command to run by hand. This step never fails the deploy.

### 6.3b Agent GCP identity default (passthrough)

With the hybrid tier enabled, `deploy.sh` writes `block` instead (see item 6
under "Hybrid Tier (Optional)" above); the rest of this section describes the
default single-node deployment.

Both `settings.yaml` heredocs `deploy.sh` writes (the Phase 3 dev-auth one and
the Phase 5 proxy/IAP one) set the top-level key
`default_gcp_identity_mode: passthrough`. Combined with the VM service
account's `roles/aiplatform.user` grant (§6.3a's prerequisite, added when the
VM is created) and the `GOOGLE_CLOUD_PROJECT` / `GOOGLE_CLOUD_LOCATION` hub
env vars from §6.3a, this means **agents created interactively, via the API,
or dispatched by a schedule all default to inheriting the VM's service
account and can call Vertex AI with no manual credential setup.**

`passthrough` is only honoured on the hub's own embedded (co-located) broker,
and only when the agent's resolved runtime profile is a local container
runtime that shares the broker host's metadata server (docker/podman-style)
— which is what a single-node VM's default runtime is. A kubernetes-type
runtime profile on the embedded broker, or any runtime profile the hub
cannot resolve, falls back to `block`, the same as an agent dispatched to
any other broker.

**Scheduled dispatches consult the hub default too.** Agents started by a
scheduled event follow the same fallback ladder as interactive/API creates:
an explicit project-level default GCP identity mode still wins, but a
project with no project-level mode set inherits this hub default exactly as
an interactively created agent would (GoogleCloudPlatform/scion#1927). No
extra per-project configuration is needed for scheduled agents on a
qualifying runtime (see above) to get Vertex access via this passthrough
default; a schedule dispatching to a kubernetes-type runtime profile needs
an explicit project-level default GCP identity mode instead, since the hub
default falls back to `block` for that runtime.

Verify:

```bash
gcloud compute ssh scion-hub-HUB_NAME \
  --zone=ZONE --project=PROJECT_ID \
  --command='cat /home/scion/.scion/settings.yaml | grep default_gcp_identity_mode'
```

**Expected:** `default_gcp_identity_mode: passthrough`.

**To change it:** Admin > Server Config > Agent Defaults > General in the web
UI, or `PUT /api/v1/admin/server-config` with
`{"default_gcp_identity_mode": "block"}` (or `"assign"`, which also requires
`default_gcp_identity_service_account_id`). The change applies to new agents
immediately, no hub restart needed.

**Redeploys revert manual changes.** `deploy.sh` writes the full
`settings.yaml` from its template on every deploy (Phase 3, and again in
Phase 5), the same way it does for every other key in that file — it does
not merge with the existing file. An admin edit to
`default_gcp_identity_mode` (or any other key not sourced from the deploy
config) is persisted by being written back into this same file, so it has no
separate store to survive a redeploy: running `deploy.sh` against an
existing VM resets the mode back to `passthrough`. Re-apply the change
afterward if you need something other than the default.

**A startup `WARN ... unrecognized keys ...` log line is expected and
harmless.** A legacy-format settings loader logs a warning listing top-level
keys it doesn't recognize, including `default_gcp_identity_mode` alongside
other keys that are obviously honoured (`server`, `schema_version`). This is
pre-existing noise unrelated to this setting — the key is still read and
applied; see the verification command above.

### 6.3c Hub-minted service accounts

Project owners can have the hub mint new GCP service accounts for agents
(the hub's GCP identity settings). The hub calls the IAM API with its own
credentials, which on this tier are the hub VM's service account,
`scion-hub-HUB_NAME@PROJECT_ID.iam.gserviceaccount.com`. Minting creates the
account, sets IAM policy on it, and deletes it again if a follow-up grant
fails, so `deploy.sh` grants that service account
`roles/iam.serviceAccountAdmin` on the project by default and enables
`iamcredentials.googleapis.com`, which the hub uses to issue tokens for
minted accounts. Set `hub_sa_minting: false` in the deploy config to skip
the role grant; doing so does not revoke a grant from an earlier deploy (the
deploy output prints the removal command).

Verify:

```bash
gcloud projects get-iam-policy PROJECT_ID \
  --flatten="bindings[].members" \
  --filter="bindings.members:serviceAccount:scion-hub-HUB_NAME@PROJECT_ID.iam.gserviceaccount.com" \
  --format="value(bindings.role)"
```

**Expected:** The list includes `roles/iam.serviceAccountAdmin`.

Things to know:

- **Passthrough is for getting started only.** `roles/iam.serviceAccountAdmin`
  applies to every service account in the project. Agents in `passthrough`
  mode (the default here, see §6.3b) use the VM's credentials, so they hold
  the same roles as the hub, this one included. Beyond getting started,
  switch the hub default to `assign` or `block` (§6.3b), and consider running
  the hub in a project that holds no other privileged service accounts.
- **Minted accounts start with no roles.** Grant a minted account whatever
  its agents need (for example `roles/aiplatform.user` for Vertex AI) before
  assigning it.
- **Teardown does not delete minted accounts.** `deploy.sh --delete` deletes
  the hub VM's service account, so its role bindings no longer apply, but
  leaves any service accounts the hub minted.

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

### 6.6 Hybrid tier: user access checks

Only when the hybrid tier is on.

1. **The admin can sign in.** Ask the user to open the access URL signed
   in as the `admin_email` account. **Expected:** the Scion Hub UI loads,
   and the admin Users page is available, from which other users are
   invited.

2. **A transport token alone is not a user session.** This access-control
   check sends a request through IAP that carries only an ID token for the
   agent transport service account, with no agent token. Minting that
   token needs `roles/iam.serviceAccountOpenIdTokenCreator` on the
   transport service account; if the operator lacks it, grant it for the
   check and remove it afterward.

   ```bash
   CLIENT_ID="$(gcloud iap settings get --project=PROJECT_ID \
     --resource-type=iap_web --format='value(accessSettings.oauthSettings.clientId)')"
   TOKEN="$(gcloud auth print-identity-token \
     --impersonate-service-account=TRANSPORT_SA_EMAIL \
     --audiences="$CLIENT_ID" --include-email)"
   curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $TOKEN" \
     "https://scion-hub-HUB_NAME-iap-proxy-HASH-REGION.a.run.app/api/v1/auth/me"
   ```

   **Expected:** the hub refuses the request with a `401` or `403` (for
   example `access denied: email not authorized`), not a `200` with user
   details. A `200` means the hub accepted the service account as a user:
   check `server.auth.user_access_mode` in `settings.yaml` on the VM.

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
| Agents lack `GOOGLE_CLOUD_PROJECT` / `GOOGLE_CLOUD_LOCATION` | The hub env var write after the Phase 3 health check failed (a warning in the deploy output) | Run the manual `sqlite3` command from that warning, or set both as hub-scoped env vars (injection mode `always`) in the admin UI. See §6.3a. |
| Admin's `default_gcp_identity_mode` change reverted to `passthrough` after a redeploy | `deploy.sh` rewrites the whole `settings.yaml`, not just the fields it manages | Expected — see §6.3b. Re-apply the change via the admin UI or API after redeploying. |
| Minting a service account in the hub fails with `Permission 'iam.serviceAccounts.create' denied` | The hub mints service accounts as the hub VM's service account (`scion-hub-HUB_NAME@PROJECT_ID.iam.gserviceaccount.com`), not as the signed-in user, and that service account lacks `roles/iam.serviceAccountAdmin`. Current `deploy.sh` grants it by default; deploys made before that, or with `hub_sa_minting: false`, do not have it. | Re-run `deploy.sh`, or grant the role by hand (no hub restart; allow a few minutes for IAM to propagate): `gcloud projects add-iam-policy-binding PROJECT_ID --member=serviceAccount:scion-hub-HUB_NAME@PROJECT_ID.iam.gserviceaccount.com --role=roles/iam.serviceAccountAdmin --condition=None` and `gcloud services enable iamcredentials.googleapis.com --project=PROJECT_ID`. See §6.3c. |
| Image build fails with `muse-code` error | Build script tried to build all images including unsupported ones | Verify the deploy script builds only `core-base`, `scion-base`, and `scion-antigravity`. If running manually, use `--target` to select individual images. |
| SSH connection fails to VM | IAP tunnel access not granted, firewall rule missing, or VM missing the network tag the rule targets | Verify IAP tunnel role: `gcloud projects get-iam-policy PROJECT_ID --flatten="bindings[].members" --filter="bindings.role:roles/iap.tunnelResourceAccessor" --format="value(bindings.members)"`. Verify firewall rule exists and its target tags: `gcloud compute firewall-rules describe scion-hub-HUB_NAME-allow-iap-ssh --project=PROJECT_ID --format="value(targetTags)"`. Verify the VM carries a matching tag: `gcloud compute instances describe scion-hub-HUB_NAME --zone=ZONE --project=PROJECT_ID --format="value(tags.items)"`. |
| `403 Forbidden` accessing the hub URL | User missing IAP access binding | Grant access: `gcloud iap web add-iam-policy-binding --resource-type=cloud-run --service=scion-hub-HUB_NAME-iap-proxy --region=REGION --project=PROJECT_ID --member=user:USER_EMAIL --role=roles/iap.httpsResourceAccessor` |
| VM has no outbound internet | Cloud NAT not created or misconfigured | Verify router and NAT exist: `gcloud compute routers nats describe scion-hub-HUB_NAME-nat --router=scion-hub-HUB_NAME-router --region=REGION --project=PROJECT_ID`. If deploy.sh logged "Reusing Cloud NAT" instead, check the *reused* router/NAT it named instead — see "Cloud NAT reuse" below. |
| `deploy.sh` fails immediately in Phase 1 with "jq is required" | `jq` is not installed | Install `jq` and re-run. Checked up front, before any prompts or API calls. |
| `deploy.sh` fails at "Checking for an existing Cloud NAT..." before creating any resources | The Cloud Router/NAT config in the region could not be listed or parsed | Check the deployer has `roles/compute.viewer` (or broader) on the project — the actual `gcloud`/`jq` error prints just above this message. This check intentionally fails closed rather than guessing — see "Cloud NAT reuse" below. |
| IAP auth fails outright, or shows an unexpected consent screen | Deployer account is in a different GCP organization than the target project, or the project is not in a GCP organization at all | See "Cross-org IAP" below. `deploy.sh` prints a warning during Phase 2 for both cases it can detect (no-org is a certain warning; cross-domain is a heuristic) — either check is skipped silently if ancestry/org metadata can't be read, so trust the symptom over the absence of the warning. |

### Cloud NAT reuse

A Cloud NAT gateway in an `ALL_SUBNETWORKS_*` mode (all subnets' full IP
ranges, or all subnets' primary ranges) cannot coexist with **any** other NAT
gateway on the same network+region — not just another all-subnets one. GCP
rejects creating one while another gateway already exists there, regardless
of what that other gateway covers. `deploy.sh` checks for this before
creating the service account, IAM bindings, or its own router/NAT (Phase 2,
after API enablement and before the service account is created), and
handles it one of two ways:

- **Some other router's NAT already covers our VM's subnet** (`default`) —
  either `ALL_SUBNETWORKS_*` mode, or a `LIST_OF_SUBNETWORKS` entry that
  explicitly forwards `default`'s primary range. `deploy.sh` reuses it
  instead of creating a duplicate, and logs which router and NAT provide
  egress:

  ```
  ==> Found an existing Cloud NAT that already covers this network/region; reusing it instead of creating our own.
    Reusing Cloud Router: some-other-router
    Reusing Cloud NAT:    some-other-nat
  ```

- **Some other router has a NAT that does *not* cover `default`** — an
  all-subnets NAT still can't be created alongside it, so `deploy.sh`
  creates its own NAT scoped to just subnet `default`
  (`--nat-custom-subnet-ip-ranges=default`), which coexists with the
  existing gateway:

  ```
  ==> A Cloud NAT gateway already exists on this network/region but doesn't provide internet egress for subnet 'default'; scoping our own NAT to that subnet only (an all-subnets NAT can't coexist with another gateway).
  ```

A Private NAT (used for NCC/hybrid connectivity, not internet egress) is
never reused, even if it technically covers `default` -- it can't provide
the VM's internet egress. It still counts as "some other router has a NAT"
above, though: GCP's exclusivity rule for an all-subnets NAT isn't
qualified by NAT type, so `deploy.sh` falls back to the scoped create in
this case too, rather than attempting (and having GCP reject) an
all-subnets NAT next to it.

A reused NAT/router is owned by whatever created it, not by this deploy: if
that owner later deletes or reconfigures it, the hub VM can silently lose
egress with no warning from `deploy.sh`. `--delete` never deletes a reused
NAT/router — it only ever targets the `scion-hub-HUB_NAME-router` /
`scion-hub-HUB_NAME-nat` names deploy.sh itself would have created, so a
reused resource owned by something else is left alone (its delete call
simply reports "not found").

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

Even with the hybrid tier off, `--delete` lists the tier's firewall rules
and looks up its internal IP reservation before deleting anything, and
stops if either finds an unmarked resource or cannot complete.

### What gets deleted

| Resource | Name pattern |
|----------|-------------|
| Cloud Run IAP proxy service | `scion-hub-HUB_NAME-iap-proxy` |
| GCE VM instance | `scion-hub-HUB_NAME` |
| Cloud NAT | `scion-hub-HUB_NAME-nat` |
| Cloud Router | `scion-hub-HUB_NAME-router` |
| Service account | `scion-hub-HUB_NAME@PROJECT_ID.iam.gserviceaccount.com` |
| Cloud Run proxy service account | `scion-hub-HUB_NAME-proxy@PROJECT_ID.iam.gserviceaccount.com` (name truncated and hashed for long HUB_NAME values) |
| IAP SSH firewall rule | `scion-hub-HUB_NAME-allow-iap-ssh` (scoped via `--target-tags` to instances tagged `scion-hub-HUB_NAME`; deleting the VM removes the tag along with it) |
| Proxy-to-VM firewall rule | `scion-hub-HUB_NAME-allow-proxy` (tcp:8080 only, scoped to the same `scion-hub-HUB_NAME` tag) |
| *If marked for this hub (tier on now or previously):* NFS allow/deny, hub-deny firewall rules | `scion-hub-HUB_NAME-nfs-allow`, `scion-hub-HUB_NAME-nfs-deny`, `scion-hub-HUB_NAME-hub-deny` |
| *If marked for this hub (tier on now or previously):* static internal IP reservation | `scion-hub-HUB_NAME-internal-ip` |
| *If marked for this hub (tier on now or previously):* agent transport service account | `scion-tp-PREFIX-HASH@PROJECT_ID.iam.gserviceaccount.com` (see "Agent transport auth" above for the id) |
| *If the hybrid tier is on:* PersistentVolumeClaim, PersistentVolume | `gke_target.pvc_name` (default `scion-hub-HUB_NAME-shared`), `scion-hub-HUB_NAME-shared` |
| *If the hybrid tier is on and this deployment created it:* Kubernetes namespace | `gke_target.namespace` (default `scion-hub-HUB_NAME`) |

### What is intentionally NOT deleted

- **IAP tunnel role** (`roles/iap.tunnelResourceAccessor`) on the deployer
  account. This role may be used for SSH access to other VMs in the project.
- **The GKE cluster itself**, under any circumstance — it's always an
  existing, attach-only prerequisite.
- **The Kubernetes namespace, if it existed before this deployment and
  never carried this deployment's marker** — it's used as-is on create and
  left alone on teardown, the same rule both ways.
- **The NFS export's own data**, since it has no separate teardown: it's
  deleted along with the VM's boot disk, not by an explicit step.
- **Service accounts the hub minted for agents** (§6.3c). The hub VM's own
  service account is deleted, so its project role bindings
  (`roles/iam.serviceAccountAdmin` included) no longer apply, but minted
  accounts stay.

To remove it manually:

```bash
gcloud projects remove-iam-policy-binding PROJECT_ID \
  --member=user:USER_EMAIL \
  --role=roles/iap.tunnelResourceAccessor
```
