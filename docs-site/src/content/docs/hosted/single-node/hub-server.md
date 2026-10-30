---
title: Setting up the Scion Hub
description: Installation and configuration of the Scion Hub (State Server).
---

**What you will learn**: How to deploy, secure, and operate the Scion Hub infrastructure, including setting up persistence, configuring runtime brokers, and managing user access.

The **Scion Hub** is the central brain of a hosted Scion architecture. It maintains the state of all agents, projects, and runtime brokers, and provides the API used by the CLI and Web Dashboard.

## Core Responsibilities

- **Central Registry**: Maintains a record of all Projects (projects), Runtime Brokers, and Templates.
- **Identity Provider**: Manages user authentication (OAuth) and issues scoped JWTs for Agents and Brokers.
- **State Store**: Tracks the lifecycle, status, and metadata of all agents.
- **Task Dispatcher**: Routes agent commands from the CLI or Dashboard to the correct Runtime Broker via persistent WebSocket connections.

## Running the Hub

The Hub is part of the main `scion` binary. You can start it using the `server start` command. A full and complete production startup command will look something like this:

```bash
# Start the Hub, Web Dashboard, and a local Runtime Broker

SCION_SERVER_SESSION_SECRET=\${SCION_SERVER_SESSION_SECRET} scion --global server start --foreground --production --enable-hub --enable-runtime-broker --enable-web --runtime-broker-port 9800 --web-port 8080 --storage-bucket \${SCION_HUB_STORAGE_BUCKET} --auto-provide

```

:::caution[Session Secret Security]
Pass the session secret via the `SCION_SERVER_SESSION_SECRET` environment variable (e.g., through a systemd `EnvironmentFile`), **not** via the `--session-secret` CLI flag. CLI arguments are visible to any local user via `ps(1)` and `/proc/pid/cmdline`.
:::

:::note[Debug logging]
Debug logging is off by default and should stay off in production. To troubleshoot, turn it on temporarily: set `SCION_LOG_LEVEL=debug` in the Hub's environment and restart the Hub. The `--debug` startup flag turns on server debug mode, which also includes debug logs. When you are done, remove the variable or flag and restart again.
:::

This is often best managed through something like systemd

### Hub vs. Broker Processes
While they can run in the same process—known as **Combo Mode** (the default for `scion server start` with no flags, which runs in workstation mode)—they serve distinct roles:
- **The Hub** is the stateless control plane. It provides the API and Web Dashboard, and should be accessible via a public or internal URL.
- **The Broker** is the execution host. It registers with a Hub and executes agents. Brokers can run behind NAT or firewalls, as they establish outbound connections to the Hub. You can connect multiple external brokers to a single Hub.

In combo mode on GCP (Cloud Run Instances or GCE), the co-located broker
automatically detects the host's GCP service account email and project ID
from the GCE metadata server at registration time. There is no need to
configure these manually.

If you prefer to run the server in the background:
```bash
scion server start
```

To manage the background daemon, use:
- `scion server status`
- `scion server restart`
- `scion server stop`



## Configuration

The Hub is configured via the `server` section in `~/.scion/settings.yaml`.

### Basic Example
```yaml
schema_version: "1"
server:
  log_level: info
  hub:
    port: 9810       # Used in standalone mode only
    host: 127.0.0.1  # Loopback required when auth.dev_mode is true
  database:
    driver: sqlite
    url: hub.db
  auth:
    dev_mode: true
```

:::note[Combined Mode]
When running with `--enable-web`, the Hub API is mounted on the web server's port (default 8080) and the standalone Hub listener is not started. The `hub.port` setting only applies when the Hub runs without `--enable-web`.
:::

:::note[Cloud Run Instance defaults]
On Cloud Run Instances, the Hub automatically detects the environment and
applies the correct `cloudrun-sandbox` runtime profile as the default. You
do not need to configure `profiles` or `runtimes` manually — the embedded
defaults match the tier.
:::

See the [Server Configuration Reference](/scion/reference/server-config/) for all available fields.

## Authentication

The Hub supports multiple end-user authentication modes to balance ease of development with production security.

### OAuth 2.0 (Production)
Scion supports Google and GitHub as identity providers. Configuration requires creating OAuth Apps in the respective provider consoles.
See the [Authentication Guide](/scion/hosted/single-node/auth/) for detailed setup instructions.

### Dev Auth (Local Development, workstation mode)
For local testing, the Hub can auto-generate a development token:
```yaml
server:
  hub:
    host: 127.0.0.1
  auth:
    dev_mode: true
```
Dev auth requires a loopback listen host (`127.0.0.1`, `::1`, or `localhost`), both when the web server is enabled and when the Hub API runs standalone without `--enable-web`. The token is written to `~/.scion/dev-token` on startup. The CLI automatically uses this token for a loopback Hub, and browser sign-in uses a single-use launch link opened or printed by `scion server start` or `scion server open` (`POST /auth/dev-launch` and `GET /auth/dev-signin`).

### User Access Tokens (Programmatic)

The Hub supports long-lived **user access tokens (UATs)** for CI/CD or other programmatic integrations. See [User Access Tokens](/scion/hosted/user/personal-access-tokens/).

## GCP Identity & Hub-Minted Service Accounts

The Scion Hub can manage and provision Google Cloud Platform (GCP) Service Accounts directly. This allows agents to authenticate to Google Cloud services via metadata server emulation, avoiding the need to distribute static credential files. 

### Configuration

To enable GCP identity management, the Hub itself must run with a GCP identity (e.g., attached to its GCE instance or GKE pod) that has the `iam.serviceAccounts.getAccessToken` permission for the target Service Accounts.

Minting new Service Accounts needs more than that. The Hub creates each account with its own identity, sets IAM policy on it, and deletes it again if a follow-up grant fails, so the Hub's identity needs `roles/iam.serviceAccountAdmin` on the Hub's GCP project (`roles/iam.serviceAccountCreator` alone is not enough). The Hub also needs `iamcredentials.googleapis.com` enabled to issue tokens for the accounts it mints. The single-node VM deploy script (`scripts/single-node-vm/deploy.sh`) enables the API, and grants the role to the hub VM's service account by default (config key `hub_sa_minting`). On other deployments, grant them yourself:

```bash
gcloud projects add-iam-policy-binding PROJECT_ID \
  --member="serviceAccount:HUB_SA_EMAIL" \
  --role="roles/iam.serviceAccountAdmin" --condition=None
gcloud services enable iamcredentials.googleapis.com --project=PROJECT_ID
```

`roles/iam.serviceAccountAdmin` applies to every Service Account in the project. Agents in `passthrough` GCP identity mode use the same credentials as a co-located Hub, so they hold this role too. Use `passthrough` for getting started only, and `assign` or `block` beyond that.

Administrators can configure Service Accounts via the Web Dashboard:
1. Navigate to the **Service Accounts** section in the Admin dashboard.
2. View the service account quota dashboard and configure minting capability controls.
3. Register existing GCP Service Accounts by email, or configure the Hub to mint new ones dynamically.

### Default Project Identities

Projects can be configured with default GCP identities that are automatically verified upon registration and automatically applied in the agent creation form. 

### Security & Authorization

Administrative actions for GCP Service Account management require `project-owner` (`ActionManage`) permissions to enforce strict security boundaries. Direct API access to Hub secrets from agents is explicitly blocked to prevent credential leakage.

For more details on how agents assume these identities via metadata server emulation, see the [Authentication Guide](/scion/hosted/single-node/auth/#gcp-identity--metadata-emulation).

### gs:// links in chat

An agent's assigned GCP Identity has a second use beyond Vertex AI: when the
`web.gcs_links` experiment is enabled on the Hub, a `gs://bucket/object`
reference that an **agent** posts in Native Web Chat becomes a clickable
link. Clicking it fetches the object through the Hub using the sending
agent's *current* assigned Service Account, through a token minted on
demand with only the read-only `devstorage.read_only` scope — never the
broader scope the agent itself uses for Vertex AI or other Google Cloud
calls, and never a token the browser can see or reuse directly.

- **What links, and when**: only a `gs://` reference inside a message an
  agent sent. The same text in a message you send does not link — the
  Hub only ever reads an object on an agent's behalf, never a human's.
- **Which identity is used**: the sending agent's *current* assigned Service
  Account at the moment you click, resolved fresh from the message each
  time — not a snapshot from when the message was originally posted. If the
  agent's identity was later reassigned or removed, the fetch uses the new
  identity (or fails, if none is assigned).
- **Whose authority is checked**: the same rule as an agent's own token
  request (see [Token-Request Authority](/scion/hosted/ha/permissions/#token-request-authority)).
  The principal recorded as the source of the agent's assignment must still
  hold `gcp_service_account.assign` on the account and, in
  `gcp_iam_check_mode: enforce`, still pass the `actAs` check, and an
  access constraint on the sending agent applies as it does to the agent's
  token request. The sending agent must also be in good standing, as for its
  own token request: a suspended (held) agent, an agent below a suspended or
  deleted parent, or an agent whose root user is inactive or no longer in the
  project cannot open links. If any of these fails, the link fails like any
  other unavailable object.
- **Size limits and previews**: objects up to 10 MiB can be opened. PNG,
  JPEG, GIF and WebP objects preview as images; other text — including SVG,
  which is always shown as source rather than rendered — previews inline up
  to 512 KB (larger text offers Download only); binary objects show "This
  file can't be previewed." with Download.
- **Errors and the Cloud Console fallback**: an object that can't be opened
  (not found, access denied, or no Service Account currently usable) shows
  a single generic message rather than revealing which case applied; an
  object over 10 MiB shows a size-limit message instead. Every such case also
  offers an **Open in Cloud Console** link, so you can still inspect the
  object with your own Google identity and permissions.

## Project Settings & Agent Limits

The Hub provides a comprehensive UI for configuring project-level settings, ensuring administrators have control over resource allocation and project configurations. Access these settings via the Web Dashboard for any project you manage.

### Configuration Tabs

The Project Settings UI is organized into three primary tabs:

- **General**: Configure the project's display name, description, and template sync settings. For git-backed projects, you can specify default branches. For hub-managed projects, you can configure external git repositories to load templates from.
- **Limits**: Define constraints on agent execution to prevent resource exhaustion.
  - **Hub-level Defaults**: Administrators can configure global default limits that apply to all projects.
  - **Project-level Limits**: Overrides can be set per-project.
  - Limits automatically pre-populate the agent creation form and restrict maximum concurrency, runtime duration, and storage.
- **Resources**: Manage the compute and plugin environments available to the project.
  - **Runtime Brokers**: Link and manage the auxiliary runtimes (e.g., specific Kubernetes clusters or remote Docker hosts) authorized to execute agents for this project.
  - **Plugins**: Enable and configure message broker plugins or other extensions for agents running within the project.

### Template Synchronization

Projects support loading templates from external Git repositories, which is especially useful for non-Git-backed (hub-managed) projects. The UI accepts bare host/org/repo URLs (e.g., `github.com/org/repo`) and automatically normalizes them, appending `/.scion/templates/` unless a deeper path is specified. This synchronization can be manually triggered via the UI to immediately pull the latest templates.

## Server Maintenance & Updates

The Scion Hub provides a built-in maintenance administration panel in the Web Dashboard for managing routine server operations, updates, and synchronization.

### Maintenance Panel Operations

Administrators can trigger critical infrastructure operations directly from the dashboard:

- **Check for Updates**: Checks for available updates and allows administrators to execute an "Update Now" action. The update banner on the maintenance and server configuration pages runs the operation that matches the deployment tier: **Update Binary (`update-binary`)** on binary-tier deployments (single-node VMs installed from releases), which takes a `target_version`, runs an installer preflight, downloads the release from pinned GitHub release asset URLs, verifies per-asset SHA256 checksums against `SHA256SUMS` and the binary's `version` output, swaps it in, and restarts the Hub (reconciling pending updates at boot and reporting status at `GET /api/v1/admin/maintenance/update-status`); and `rebuild-server` on source-tier deployments. See [Maintenance (`server.maintenance`)](/scion/reference/server-config/#maintenance-servermaintenance).
- **Rebuild Server (`rebuild-server`)**: Initiates a fire-and-forget server rebuild and restart sequence. It uses staging paths and sudoers implementation to ensure reliable updates even while the server is running. It replaces the binary only, never the systemd unit. On a starter hub it compares the installed unit with `scripts/starter-hub/scion-hub.service.tmpl` in the checked-out tree. If they differ, the run log and a banner on this page say "re-provision needed", with the steps: re-run `scripts/starter-hub/gce-start-hub.sh --full` from the provisioning workstation with the original flags. The banner clears after a rebuild reports a match.
- **Rebuild Web (`rebuild-web`)**: Recompiles the web frontend assets.
- **Pull Images (`pull-images`)**: Triggers the Docker/Podman executor to pull the latest agent container images.
- **Starred Messages Reconciliation (`chat-star-reconcile`)**: Checks every stored star until nothing is left to update or remove. Run it before enabling starred messages again after a downgrade; see [Starred messages rollout](#starred-messages-rollout).
- **Restart Hub**: Initiates a fire-and-forget server restart (`POST /api/v1/admin/maintenance/restart`) via systemd, restricted to administrators. A modal confirmation dialog prevents accidental triggers of restarts. It is refused with `409 Conflict` while an **Update Binary** or **Rebuild Server** run is running, and once accepted it holds back new update and rebuild runs (also `409 Conflict`) until the Hub is back, for at most 2 minutes. An update that is already waiting for its own restart does not block it.

### Migrations

The panel also lists one-time data migrations, with **Run** for a pending migration and **Retry** for a failed one. The timezone-related ones, `utc-timestamp-normalize` and `applied-config-tz-cleanup`, are described in [Times and timezones: Operator steps](/scion/reference/times-and-timezones/#operator-steps). On SQLite, the automatic start-up repair of unreadable timestamp rows, and the Hub's run of `utc-timestamp-normalize` at start (in the background, while the Hub serves requests), first write a one-time snapshot next to the database file (`<db>.pre-utc-timestamp-normalize-<time>.bak`); both use the same file, and a later run reuses it. Requests wait while the snapshot is copied. It needs about the database's size in free disk space and contains secrets, so store it like the database. Start-up removes these snapshots, and the `<db>.pre-fk-reconcile-<id>-<time>.bak` snapshot taken before the foreign-key repair, 7 days after their last use; a snapshot that a repair still needs is kept. To keep one longer, copy it elsewhere. A dry run of `utc-timestamp-normalize` (`{"params":{"dryRun":true}}`) reports without writing.

`group-participant-mode-cleanup` removes legacy agent participant rows from group conversations; see [Agents in Group Conversations](/scion/reference/messaging-authorization/#agents-in-group-conversations). It only reports unless run through the API with `{"params":{"apply":"true"}}` (the string `"true"`), which the panel's **Run** does not send; a panel run without dry run reports what it would remove and is still marked completed; the migration can still be run with apply through the API.

`project-storage-migrate` moves project files from the hub's older storage locations into the project storage layout that agents mount. Run it once on a hub whose workspace storage is shared (`nfs`, `cloudrun-volume` or `gke-shared-volume`). On local storage it does nothing. On Kubernetes, agent pods do not mount `gke-shared-volume` yet, so agents there work in an `emptyDir` workspace rather than in this layout; see [Stopping, Starting and Restarting Agents](/scion/hosted/ha/kubernetes/#stopping-starting-and-restarting-agents). Until the migration has run, the project **Files** tab shows a notice for each project that still has files in an old location. The migration moves four things for each project:

- the hub-managed project tree on the share and the hub's own `~/.scion/projects/<slug>`, into `projects/<project id>/workspace`;
- the project configuration in the old `.scion` (`settings.yaml`, `project-id`, `project-slug`, `templates/`, `harness-configs/`; never broker records such as `agents/`), copied into `projects/<project id>/.scion`;
- the hub's local shared-dir content, into `projects/<project id>/shared-dirs/<name>` (each Runtime Broker also moves its own node-local shared directories from `~/.scion/project-configs/<slug>__<id8>/shared-dirs/<name>` into `<ProjectRoot>/shared-dirs/<name>` on its first start after upgrade);
- stopped (`stopped` or `halted`, not running or `error`) Cloud Run agents' legacy `agents/<agent id>/{home,secrets}` directories on the workspace share, into the unified `<subpath_root>/<project id>/agents/<slug>/{home-,secrets-}<agent id>` layout under the project's provisioning locks (running agents are deferred; starting a Cloud Run agent whose legacy home has not yet been moved is refused).

A tree is renamed only when its target does not exist yet and the filesystem supports an atomic no-replace rename. NFS does not, so on NFS the tree is copied and its source kept, which needs room for a second copy until you delete the `.migrated-<time>` directory. Otherwise only the files missing from the target are copied, and no file in the target is ever overwritten. Each conflict is listed, and the source is kept beside itself as `<name>.migrated-<time>`.

Old trees are found by project slug. A tree can therefore belong to a deleted project whose slug a new project reuses, so the migration first checks the project ID recorded in the tree. It looks in `.scion/project-id`, in the project ID keys of `.scion/settings.yaml` (current and pre-rename keys), and in the hub's workspace record. A tree whose recorded ID names another project is never migrated. A tree that records no ID is left in place and listed in the log with what was checked. To migrate it, run the migration with `{"params":{"adoptUnverified":"<slug>[,<slug>...]"}}`, or `"true"` for all projects. Symlinks and special files are never followed or copied; they are listed instead. If a broker in the hub process still uses `~/.scion/projects/<slug>`, that tree is only copied from and is left in place. Migrated files are given the agents' uid and gid from the storage configuration. If the export refuses that (root squash), the log says so; fix ownership as described in the [hybrid tier guide](https://github.com/GoogleCloudPlatform/scion/blob/main/docs/deploy/hybrid-tier.md). Once every step for a project has succeeded and its workspace on the shared storage holds content, the project's old workspace copy in object storage is deleted; otherwise it is kept, because it may be the only copy. Per-agent sync objects are kept. Run it before starting agents in the affected projects. While it moves a project's files it holds that project's provisioning locks, so an agent start for the project waits for it and may fail cleanly if the wait is too long; start the agent again afterwards. Every file is copied under a temporary name (`.scion-migrate-tmp-*`) and gets its final name only once it is complete, so an interrupted run never leaves a partial file under a real name; the next run removes such leftovers. A git checkout's `.git` is copied last, under a temporary name (`.git.scion-migrating`), and renamed into place only once every copyable entry is present with identical content (symlinks and special files are never copied; they are listed); if an entry differs (for example after a crash), it is left under the temporary name and the project is reported as still pending. Run the migration again to resume it. A git project whose old tree has no `.git` is reported as migrated with a warning: its first agent start refuses the workspace (not empty, no provisioning marker, no `.git`) until you move the non-git entries out or re-clone. Run it with `{"params":{"dryRun":true}}` first to see the plan. It is safe to run again: a second run reports `moves=0`.

### Starred messages rollout

Starred messages in native chat are gated by the `web.chat_starred_messages` [experiment](/scion/reference/experiments/), on by default. For what users see, see [Starred messages](/scion/hosted/user/messaging/#starred-messages).

- **Before upgrading a multi-instance Hub**: The experiment is on by default, so new instances serve stars as soon as they start, while older instances do not update stars when a direct message becomes a thread. If you upgrade from the 2026-10-19 release, set the experiment OFF in **Admin → Server Config → Experiments** first. If you upgrade from an earlier release, which does not know the experiment and refuses to store a setting for it, set it OFF from the first upgraded instance. Then upgrade every instance, run **Starred Messages Reconciliation** until the run is **completed** (as in **Re-enabling after a downgrade** below), and only then turn it on or reset it to the default. The star table is created at Hub start whether or not the experiment is on. A single-instance Hub needs none of this.
- **Enabling and disabling**: Toggle the experiment in **Admin → Server Config → Experiments**. Users see the change on their next page load. While it is off, the star controls and the **Starred** filter are hidden, the starred-message API routes answer `404`, and no star updates are sent to browsers. Stored stars are kept, so users find them again when it is turned back on. Chat history, read state and the other rail filters work the same either way.
- **Cleanup while off**: Each Hub instance checks a batch of up to 500 stars every 10 minutes, whether the experiment is on or off. It removes stars whose message, conversation, project or owner was deleted, and updates a star whose message moved to another conversation. It never removes a star because of a permission or an account status: a star the owner can no longer read stays hidden and comes back if access is restored.
- **Downgrading**: Disable the experiment before you downgrade any instance to a release without starred messages. When a direct message becomes a thread, older releases do not update the stars of its messages, so those stars point at the old conversation until they are reconciled.
- **Re-enabling after a downgrade**: Upgrade every instance, then run **Starred Messages Reconciliation** (`chat-star-reconcile`) from the maintenance panel, or with `POST /api/v1/admin/maintenance/operations/chat-star-reconcile/run`, and wait for the run to finish (`GET /api/v1/admin/maintenance/operations/chat-star-reconcile/runs/<runId>`). Enable the experiment only after the run is **completed**. It checks every star in batches, repeating full passes until a pass updates and removes nothing, and its log lists each pass and the totals checked, updated and removed. If stars were still changing after 3 passes, the run fails and asks to be run again; run it again until it completes. If the run fails for another reason, such as a database error or its 30-minute time limit, run it again too: each run starts from the beginning and nothing is lost. It takes no parameters, is safe to run again, and can run while users are active, with the experiment on or off. It uses the same permission as the other maintenance operations (`hub.maintenance.execute`).

### Operation Execution & History

All maintenance tasks executed through the panel are tracked. The maintenance interface provides:
- Real-time log capture for active operations.
- Execution history detailing operation duration, completion status, and log archives.
- Automated cleanup of stalled operations during server startup.

#### Concurrency Guard

To guarantee server stability, the Hub enforces a strict **concurrency guard** on all maintenance operations:
- **Mutual Exclusion:** Running multiple operations (such as parallel `go build` compilations) simultaneously is rejected.
- **Conflict Rejection:** Any new maintenance request initiated while another is active returns a `409 Conflict` HTTP status code. This prevents resource exhaustion or server outages from concurrent builds. **Update Binary**, **Rebuild Server** and **Restart Hub** form one group: none of them starts while another is in progress (Restart Hub only waits for a running update or rebuild), and the `409` error names the blocking operation (and its run, when the blocker is an update or rebuild run; a pending Hub restart has no run).
- **Status Monitoring:** Operators can trace progress and view active logs on the dashboard's maintenance interface.

## Persistence

The Hub requires a database to store its state.

### SQLite (Default)
Ideal for local development or single-node deployments. The database is a single file.
```yaml
server:
  database:
    driver: sqlite
    url: /path/to/your/hub.db
```

### PostgreSQL (Production)
Required for high-availability (multi-instance) deployments, and also usable for a single Hub. The URL is a standard Postgres connection string. The same settings can come from `SCION_SERVER_DATABASE_DRIVER` and `SCION_SERVER_DATABASE_URL`.
```yaml
server:
  database:
    driver: postgres
    url: "postgres://user:password@localhost:5432/scion?sslmode=disable"
```

Setting `driver: postgres` marks the Hub as an HA deployment. Events and Runtime Broker commands then reach every Hub replica through Postgres `LISTEN/NOTIFY`. In hosted mode (`--hosted`, or server mode `hosted` or its legacy spelling `production` in `settings.yaml` or `SCION_SERVER_MODE`), the Hub then runs the HA startup checks and refuses to start without an explicit `server.hub.hub_id`, a `server.database.url`, GCS storage (`server.storage.provider: gcs` with a bucket) and a durable session secret, plus the IAP settings when `server.auth.mode` is `proxy` (see [Deploy on GCP](/scion/hosted/ha/setup-gcp/)). See [Database (`server.database`)](/scion/reference/server-config/#database-serverdatabase) for the fields.

To move an existing SQLite Hub to Postgres, run `scion server migrate`:

```bash
scion server migrate \
  --from sqlite:///var/lib/scion/hub.db \
  --to "postgres://scion:secret@db.example.com:5432/scion?sslmode=require"
```

The copy reads the SQLite file without changing it, skips rows already in the destination (so a failed run can be restarted), and compares row counts after each table. A restart expects the destination to hold only rows from an earlier run; rows changed or deleted in the source since then are not reconciled, and a row-count mismatch stops the run. The SQLite file is kept unless you pass `--drop-source`.

## Storage Backends

The Hub stores agent templates and other artifacts.

- **Local File System**: Default. Stores files in `~/.scion/storage`.
- **Google Cloud Storage (GCS)**: Recommended for cloud deployments. Set the `SCION_SERVER_STORAGE_BUCKET` environment variable.

Template and harness-config files are stored by content: each file is stored once under `<template or harness-config path>.blobs/<sha256>` and never overwritten, and uploads are staged under `<path>.staging/` until the push is committed. A Runtime Broker that downloads a template or harness-config in this layout while a new version is pushed gets either the old version or the new one, never a mix. A commit does not delete the files a new version dropped. The Hub deletes stored files that no template or harness-config references, and abandoned staged uploads, once they are older than 24 hours (the grace period can be raised but not set below 1 hour). It does this every hour, and an admin can run it at any time as the **Template Blob Garbage Collection** maintenance operation, which covers harness-configs too, with an optional `grace` parameter (for example `48h`).

Templates and harness-configs stored before this layout keep their old layout (one object per file path) until their next commit (a push, a file edit, or a re-import), which moves them to the new layout. That commit then removes the old copy, unless another template or harness-config of the same kind still uses the same path or a path inside it. If a file in the old copy no longer matches its recorded hash (for example, two rows once shared the path), the commit keeps the content that is actually stored and records its real hash. A Runtime Broker that is downloading the old copy at that moment can fail once; it retries and gets the new version.

The Hub no longer copies Hub-managed workspaces to Runtime Brokers through its storage bucket. A Runtime Broker reaches a Hub-managed workspace through the Hub's shared project storage (`server.project_storage` with `nfs`), or runs on the Hub's host. A create sent to a remote Runtime Broker that can do neither fails up front with `409`. The only exception is a linked project with no git remote (registered from an existing checkout) on a Hub with local project storage. It keeps its workspace on that Runtime Broker (filled by the CLI workspace upload and `scion sync to`), and the Hub's Files tab answers `409` for it.

- For Hub-managed projects and shared-workspace git projects, the Hub's project **Files** tab, workspace archive download and resource import resolve `projects/<uuid>/workspace` on a shared project storage backend (`nfs`, `cloudrun-volume`, `gke-shared-volume`) or `~/.scion/projects/<slug>` on the `local` backend. They answer `409` for `empty-per-agent` projects, and the Files tab and archive answer `503` if a shared backend's mount point is missing.
- `POST /api/v1/projects/{id}/workspace/pull` works only for shared-workspace git projects. It answers `409` otherwise, or when the workspace is not provisioned.
- On a shared backend, project create seeds `projects/<uuid>/.scion`, and on project delete the Hub, its only remover, removes `projects/<uuid>` from its own mount of the share, so every Hub replica must have the share mounted (see [cleanup on project delete](/scion/reference/server-config/#cleanup-on-project-delete)).
- For a linked project in a per-agent git mode, the **Files** tab resolves only a co-located Runtime Broker's local path. It answers `409` when the project is served only by remote Runtime Brokers. The linked-project workspace cache and `POST /api/v1/projects/{id}/workspace/sync` have been removed and answer `404`.
- The CLI workspace upload of a non-git checkout still goes through the Hub's GCS bucket. A Hub with GCS storage asks the Runtime Broker to download it through Hub-signed URLs: the broker requests pages of short-lived (15 minute), GET-only URLs, one per file, from the Hub (`POST /api/v1/runtime-brokers/{id}/agents/{agentId}/workspace-download`) and fetches the files directly from the bucket. The broker needs no Google Cloud credentials and no access to the bucket. The Hub lists and signs with its own storage credential and serves only the broker the agent is assigned to. A Runtime Broker from before this change ignores the request and downloads with its own credentials, from the bucket the Hub names in the create request (or its own GCS storage bucket setting). With no bucket at all, it fails the create up front with `422 workspace_storage_unconfigured`. A download mode the broker does not know fails the create with `422 workspace_download_mode_unsupported`.
- The broker downloads the whole upload into a staging directory next to the workspace before it changes the workspace. A project workspace is then put in place with one rename, so a failed or interrupted download never leaves a partial workspace that a later create would reuse. A per-agent workspace is updated in place after the download completes.
- Roll out the Hub before the Runtime Brokers. A broker only takes the signed-URL path when the Hub asks for it, so either order keeps today's behaviour, and upgrading the Hub first lets each broker use the new path as soon as it is upgraded.

## Deployment

### GCE VM

The most direct path to getting a deployed demonstration hub is to use the GCE setup scripts in `/scripts/starter-hub` (the Developer Hub tier)

### Cloud Run, GKE (GCP)
The Hub is designed to be stateless and is highly compatible with Google Cloud Run. 
- Use **Cloud SQL** (PostgreSQL) for the database.
- Use **Cloud Storage** for template persistence.
- Connect the Hub to Cloud SQL using the Cloud SQL Auth Proxy or a VPC connector.

See [Cloud Run](/scion/hosted/single-node/hub-setup-cloudrun/) and [Helm](/scion/hosted/ha/helm/).

## Discord Integration

The Hub supports native Discord webhooks to broadcast persistent agent messages, notifications, and `ask_user` requests to a Discord channel.

To configure Discord notifications, set the `discord_webhook_url` in your `server` configuration block (or via the `SCION_DISCORD_WEBHOOK_URL` environment variable):

```yaml
server:
  discord_webhook_url: "https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_WEBHOOK_TOKEN"
```

Once configured, the Hub will automatically forward messages with severity-based color coding. Urgent messages or explicit requests for user input can also trigger role or user mentions, ensuring that critical requests receive immediate attention.

## Observability

The Hub supports structured logging and can forward its internal logs and traces to an OpenTelemetry-compatible backend (like Google Cloud Logging/Trace).

To enable log forwarding, set `SCION_OTEL_LOG_ENABLED=true` and `SCION_OTEL_ENDPOINT`. See the [Observability Guide](/scion/hosted/single-node/observability/) for full details on centralizing system logs and agent metrics.

## Monitoring

The Hub exposes health check endpoints:
- `/healthz`: Basic liveness check. Always `200`; the body's `status` is `healthy`, `degraded` (the server is up, but a non-critical check such as the co-located broker is failing), or `unhealthy` (a critical check — the database, or the configured shared workspace storage mount — is failing), with `checks` naming any failing subsystem. On a single-node setup (Hub + co-located runtime broker), the co-located broker check reports `healthy`, `unhealthy: registration failed`, or `unhealthy: registration pending` — a failed registration is not retried, so `status` stays `degraded` until the broker configuration is fixed and the server is restarted. The single-node workstation setup runs combined (web server + Hub on one port), so the web server answers `/healthz` and nests the Hub's own health under `hub` — the check is at `hub.checks.colocated_broker`, not top-level `checks.colocated_broker` (that path is only for a standalone Hub with no web server). `scion server status` reports a degraded server as running and names the failing checks (in either shape); an unhealthy one is reported as `unhealthy` with its checks. `scion server start` names them too, but only when it waits to open a browser (an interactive, non-headless terminal with web enabled, and `SCION_NO_BROWSER` unset): it waits up to 20 seconds for `healthy`, then opens the browser anyway with a warning if the server is still degraded. Other `start` invocations print nothing about it. Separately, every daemon `scion server start` and `scion server restart` waits about 30 seconds at most for each started listener (web, standalone Hub, runtime broker) to answer `/healthz` with any status before it returns, and exits non-zero, leaving the server running, if one does not.
- `/readyz`: Readiness check (verifies database connectivity and, when a non-`local` workspace storage backend is configured, that its mount is available). Unaffected by the co-located broker check above — use `/readyz`, not `/healthz`, for Kubernetes/Cloud Run readiness probes.
- `/health`: Legacy/alternative liveness check endpoint.

### Reverse Proxy / GFE Interception Handling

In distributed or hosted deployments (such as Google Cloud Run behind a Google Front End), reverse proxies may intercept calls to `/healthz` and return a non-JSON response (e.g. `text/plain`). 

To prevent connectivity and status reporting issues:
- **Hub Client Fallback**: The Scion Hub client automatically detects non-JSON `2xx` responses from reverse proxies on `/healthz` and transparently falls back to `/health`. If both fail, it appends a helpful diagnostic tip suggesting a Cloud Run or GFE configuration review.
- **Broker Client Diagnostics**: If the Runtime Broker client receives a non-JSON `2xx` response on `/healthz`, it blocks the call and returns a precise error indicating that a reverse proxy (e.g., GFE) is likely intercepting `/healthz` and provides troubleshooting guidance.

Logs are output to `stdout` in either `text` (default) or `json` format, suitable for collection by systems like Fluentd, Cloud Logging, or Prometheus.
