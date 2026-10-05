---
title: Admin Settings Model
description: How Scion manages operational settings in the Hub database (SQLite or Postgres), including the seeded/managed lifecycle, environment variable namespaces, and the admin UI.
---

This document describes how Scion manages operational settings in the Hub settings database, including the configuration precedence model, the seeded/managed lifecycle, and the admin settings UI. Every Hub keeps its operational settings in its database, whichever driver it uses: embedded SQLite (workstation and single-node hosted) or Postgres (HA hosted).

## Configuration Precedence

Scion resolves configuration values using a layered merge:

```
coded defaults → SCION_SEED_* → settings.yaml → SCION_SERVER_*
```

Each layer overrides the one before it. The final merged result is the **bootstrap configuration** — the starting point for a hub instance.

### Layer-0 vs Layer-1 Keys

Settings are classified into two layers:

| Layer | Examples | Behavior |
|-------|----------|----------|
| **Layer-0** (bootstrap) | `server.mode`, `server.database.*`, `server.storage.*`, `server.secrets.*`, `server.hub.port`, `server.auth.dev_mode` | Always resolved from bootstrap configuration (`settings.yaml` and environment). Editable through the admin UI only on workstation Hubs; see [Layer-0 edits by deployment mode](#layer-0-edits-by-deployment-mode). |
| **Layer-1** (operational) | `server.hub.admin_emails`, `server.auth.user_access_mode`, `server.auth.default_user_role`, `telemetry.*`, `agent_defaults.*`, `server.github_app.*`, `server.notification_channels`, `server.federation.*`, `runtimes`, `profiles`, `harness_configs`, `quotas.enforce_broker_quotas`, `agent_secrets.user_scope_only` | Stored in the Hub database on every driver and editable via the admin UI. Bootstrap values serve as initial defaults. |

### Resolution

On every Hub, SQLite or Postgres, Layer-1 settings follow this resolution:

```
Database value > Bootstrap merge (for Layer-1 keys)
Bootstrap only (for Layer-0 keys)
```

The database always wins for Layer-1 keys. Bootstrap values are used as initial seeds and as fallback when no database row exists. Editing a Layer-1 key in `settings.yaml` therefore only changes the seed: once an admin has saved that section through the API, the database value wins, on SQLite exactly as on Postgres.

### Layer-0 edits by deployment mode

Whether Layer-0 settings can be edited through `PUT /api/v1/admin/server-config` depends on the deployment mode, not on the database driver. `GET` reports it as `layer0_editable`.

- **Hosted Hubs** (started with `--hosted`, or `server.mode: hosted` in `settings.yaml` / `SCION_SERVER_MODE=hosted`): Layer-1 keys are written to the database. A Layer-0 field the body carries is rejected with `422 layer0_rejected` when its value differs from what `GET` returns, an explicit `false`, `""` or `0` included. Fields with no database home (such as `schema_version`, `active_profile`, `workspace_path` or `auto_inject_gcloud_adc`) follow the same rule with `422 unclassified_keys_rejected` or `422 unpersisted_keys_rejected`. An unchanged value is ignored, so sending back the body `GET` returned is a `200` that writes nothing. Deployment tooling owns `settings.yaml`.
- **Workstation Hubs** (the default mode): Layer-1 keys are written to the database. Layer-0 keys and keys with no database home are written to `settings.yaml`. `log_level` is applied immediately and the co-located broker reloads its runtime. Every other key that changed in the file is listed in the response's `reload.requires_restart`, and `file_keys` lists every key that changed in the file.

`server.mode` must be empty (workstation), `workstation`, `hosted` or `production` (the legacy name for hosted). Values are case-sensitive. The server refuses to start with any other value (a typo used to mean workstation), and a workstation save that sets another value is rejected with `400`. To fix a bad value, edit `server.mode` in `settings.yaml` or `SCION_SERVER_MODE`; `--hosted` selects hosted mode but does not make a bad value valid.

On a workstation Hub the file edit follows the request body field by field:

- Each field the body carries is written exactly as sent, an explicit empty value (`""`, `false`, `0`, `[]`) included. Only `null` removes a key or block.
- The Hub then compares the settings it would load from the edited file with the settings it loads today (the server config the Hub builds, the typed settings, and the `active_profile`/`workspace_path` merge over the built-in defaults). Fields whose effective value does not change are not written, not listed in `file_keys` and not reported as needing a restart. If nothing effective changes, the file is not written, so saving an unchanged form writes nothing. For example, `""` for a key the file does not have changes nothing and is skipped. `[]` for `server.hub.cors.allowed_origins` replaces the default list with an empty one and is written.
- A field whose value in the edited file would not be what was sent is rejected with `422 unsaved_keys_rejected` naming it, and nothing is saved.
- Fields the body leaves out are kept.
- Creating a `cors` block (`server.hub.cors` or `server.broker.cors`) without sending `enabled` also writes `enabled` with its current value. Inside the block a missing `enabled` means off, while a missing block means on, so adding an origin does not silently turn CORS off.
- `server.broker.broker_id` and `broker_token` are written by the Hub itself. A save may send them back unchanged (the token as `********`), but changing or clearing them is rejected with `422 hub_owned_keys_rejected`. A `null` on a block that contains them (`{"server":null}` or `{"server":{"broker":null}}`) removes everything else in that block and keeps them.
- The admin UI sends only the fields you changed since the page loaded, so defaults the form shows for keys the file does not have are never written.
- The file is edited in place: comments, key order and keys the Hub does not know survive. A `settings.yaml` that uses YAML anchors or aliases on an edited path cannot be edited in place, and the save is rejected with `422` (edit that file by hand).

Some workstation fields are overridden at every start by the workstation defaults and `scion server start` flags, so a value in `settings.yaml` has no effect: `server.broker.enabled` and `server.broker.host` (`--enable-runtime-broker`, `--host`), `server.hub.host` (`--host`), `server.auth.dev_mode` (`--dev-auth`), `server.storage.provider` (`--storage-bucket`) and `server.secrets.backend`. The admin UI shows them read-only with a "Set by workstation startup defaults / server flags" badge.

A workstation save that touches both destinations is checked as a whole first. Any validation failure, for either part, writes nothing. The Hub then takes its settings-file lock and prepares the new `settings.yaml` in memory, writes the database sections, and writes the file last, atomically, before releasing the lock. These writers in the same server process take the same lock for their whole read-modify-write, so neither of two concurrent changes is lost: broker registration writing its ID and token, the identity, workstation-settings, runtime and image-registry endpoints, and the hub-sync cleanup of project settings. Not covered yet: the plugin-registration writes made by the integrations API, and writers in other processes, such as a `scion` CLI command running while the server runs (ptone/scion#3047). If a database write fails (for example `409 revision_conflict`), `settings.yaml` is left unchanged. Database sections written before the failure stay written, as for any save that touches several sections, and are listed in `applied`. Only if the final file write fails are the database changes saved without the file changes; the `500` response lists them.

On every Hub, a key the API cannot store anywhere (an unknown or misspelled key, or a field with no settings home) is rejected with `422 unpersisted_keys_rejected`, unless its value is unchanged from what `GET` returns. A save never answers `200` for a key it dropped. An empty body is a `400`.

## Seeded/Managed Lifecycle

Each settings section in the database has an **origin** that tracks whether it was populated by the system or edited by an admin.

### Seeded Sections

When a hub first boots with a postgres database, it populates the settings database from the bootstrap configuration. These sections are marked as **seeded** — they track the deployment configuration.

Seeded sections:
- Re-sync from bootstrap on every hub restart (if the deployment config changes, the database updates)
- Show a caption in the admin UI: *"Tracking deployment configuration — will re-sync on restart until edited"*
- Skip writes when the bootstrap value hasn't changed (no unnecessary revision bumps)

### Managed Sections

When an admin edits a setting in the UI and saves, the section becomes **managed**. Managed sections:
- Are owned by the database — bootstrap changes no longer overwrite them
- Show standard metadata in the admin UI (source, revision, last updated by/at)
- Display superseded notices when the deployment config differs from the database value

### Reset to Bootstrap

A managed section can be reset to tracking mode via the "Reset to bootstrap" button in the admin UI. This:
1. Deletes the database row for that section
2. The section falls back to bootstrap values
3. On next boot, the section is re-seeded from current deployment config

:::note[Known Limitation]
The "Reset to bootstrap" feature requires a backend endpoint that is not yet available. The UI is wired and ready — the endpoint will be added in a follow-up release.
:::

## Environment Variable Namespaces

:::tip[Boolean Environments]
Operational boolean settings configured via environment variables (such as `SCION_SEED_*` and `SCION_SERVER_*`) are processed using the standard `parseBoolEnv` logic. They support case-insensitive spellings for `yes`/`y`/`on` and `no`/`n`/`off`, and issue startup warnings on unrecognized non-empty inputs to help administrators catch configuration typos.
:::

### `SCION_SEED_*` (New)

Use `SCION_SEED_*` to provide initial/default values for operational settings. Seeds are overridable — admins can change them in the UI, and the database value takes precedence.

Seeds re-sync on restart for sections that haven't been admin-edited (seeded sections). Once a section is managed, the seed value is superseded.

**Mapping rules:**
- `server.*` keys: `SCION_SEED_SERVER_` + uppercase snake_case suffix
- Non-`server.*` keys: `SCION_SEED_` + uppercase snake_case suffix

| Setting | Seed Environment Variable |
|---------|--------------------------|
| `server.hub.admin_emails` | `SCION_SEED_SERVER_HUB_ADMINEMAILS` |
| `server.hub.public_url` | `SCION_SEED_SERVER_HUB_PUBLICURL` |
| `server.auth.user_access_mode` | `SCION_SEED_SERVER_AUTH_USERACCESSMODE` |
| `server.auth.authorized_domains` | `SCION_SEED_SERVER_AUTH_AUTHORIZEDDOMAINS` |
| `server.auth.default_user_role` | `SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE` |
| `server.hub.auto_suspend_stalled` | `SCION_SEED_SERVER_HUB_AUTOSUSPENDSTALLED` |
| `server.hub.soft_delete_retain_files` | `SCION_SEED_SERVER_HUB_SOFTDELETERETAINFILES` |
| `image_registry` | `SCION_SEED_IMAGEREGISTRY` |
| `telemetry.enabled` | `SCION_SEED_TELEMETRY_ENABLED` |
| `server.github_app.webhooks_enabled` | `SCION_SEED_SERVER_GITHUBAPP_WEBHOOKSENABLED` |

### `SCION_SERVER_*` (Existing)

`SCION_SERVER_*` remains the authoritative namespace for **Layer-0** (bootstrap) settings:

| Setting | Environment Variable |
|---------|---------------------|
| `server.hub.port` | `SCION_SERVER_HUB_PORT` |
| `server.hub.host` | `SCION_SERVER_HUB_HOST` |
| `server.database.driver` | `SCION_SERVER_DATABASE_DRIVER` |
| `server.database.url` | `SCION_SERVER_DATABASE_URL` |
| `server.auth.dev_mode` | `SCION_SERVER_AUTH_DEVMODE` |
| `server.secrets.backend` | `SCION_SERVER_SECRETS_BACKEND` |
| `server.mode` | `SCION_SERVER_MODE` |
| `server.log_level` | `SCION_SERVER_LOGLEVEL` (applied on a workstation server-config save only, see below) |

There is no boot-time override for `server.log_level`. `SCION_SERVER_LOGLEVEL` only affects the level applied when a workstation admin server-config save that changes `server.log_level` re-reads the config. At startup, use `--debug` or `SCION_LOG_LEVEL=debug`.

### `SCION_SERVER_*` Deprecation for Layer-1

:::caution[Deprecation]
Using `SCION_SERVER_*` for Layer-1 operational settings is **deprecated**. These variables still work during the transition period (they are treated as the top of the bootstrap merge), but a warning is logged on startup.

**Migration:** Replace `SCION_SERVER_<KEY>` with `SCION_SEED_<KEY>` for any Layer-1 setting.

Example:
```diff
- SCION_SERVER_HUB_ADMINEMAILS=admin@example.com
+ SCION_SEED_SERVER_HUB_ADMINEMAILS=admin@example.com
- SCION_SERVER_AUTH_DEFAULTUSERROLE=viewer
+ SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE=viewer
```
:::

The deprecation notice also appears in the admin UI when deprecated variables are detected.

## Maintenance Mode

Maintenance mode set through the admin API (`PUT /api/v1/admin/maintenance`) is stored in the Hub database on every driver, so it survives restarts. Until a maintenance row exists, the Hub reports and builds on the state it started with (`SCION_SERVER_ADMIN_MODE` or `settings.yaml` `admin_mode`).

### Break-glass by deployment mode

- **Hosted Hubs:** maintenance is **cluster-consistent**. A database row is propagated to all replicas and cannot be overridden by per-node environment variables or `settings.yaml`.
- **Workstation Hubs:** starting the Hub with `SCION_SERVER_ADMIN_MODE=true` or `admin_mode: true` in `settings.yaml` keeps it in maintenance for that run, even when the database row says otherwise. `GET /api/v1/admin/maintenance` then reports `"break_glass": true` and the admin UI shows a notice. Restart without the setting to hand control back to the row.

While a break-glass is active, a save from the maintenance page updates the stored row (for example its message) but cannot take this run out of maintenance. When a row already exists, a save that does not set `enabled` keeps the row's own on/off value, so the Hub leaves maintenance after a restart without the break-glass. When no row exists yet, such a save stores the current state, which is "on" during a break-glass. Turn maintenance off explicitly, or the Hub stays in maintenance after that restart.

`admin_mode: true` in `settings.yaml` can only be cleared by editing the file: the admin API stores maintenance in the database, not in `settings.yaml`.

## HA Bootstrap Guidance

For high-availability deployments with multiple hub replicas:

1. **Initial setup:** Use `SCION_SEED_*` environment variables to configure operational settings across all replicas. All replicas share the same bootstrap configuration.

2. **After first admin edit:** Once an admin edits a setting in the UI, the section becomes managed. The database value propagates to all replicas within ~2 seconds via the event system. No per-node env configuration is needed.

3. **Updating seeds:** To change the default for a seeded (unedited) section, update the `SCION_SEED_*` variable and restart the replicas. The new value syncs to the database on boot.

4. **Managed sections:** For sections that have been admin-edited, changing the `SCION_SEED_*` variable has no effect — the database value wins. A "superseded" notice in the admin UI shows which deployment values differ from the database.

5. **Resetting:** To return a managed section to tracking mode, use "Reset to bootstrap" in the admin UI. The section reverts to the seed value and will track future seed changes.

## Admin UI

The admin settings page (`/admin/server-config`) is layer-aware, permission-gated, and has been restructured for improved usability:

- **Permission-Gated Tabs:** Settings page tabs are dynamically gated by the caller's actual resource-level permissions. For example, a role with template-only permissions (like `template.*`) sees only the **Templates** tab, with other administrative tabs hidden. Nav and route guards use granular, per-item permission checks driven by the `/api/v1/admin/status` permissions array.
- **Hosted Hubs:** Layer-0 fields are read-only with a "Managed via deployment configuration" badge. Layer-1 fields are editable.
- **Workstation Hubs:** Fields pinned by `SCION_SERVER_*` environment variables are read-only with a "Set via environment variable" badge, and fields that server start flags override with a "Set by workstation startup defaults / server flags" badge. Other fields are editable: Layer-1 fields save to the database, Layer-0 fields to `settings.yaml`.
- **Default Agent Role, Default Maximum Agent Role and Default Harness Auth** are shown read-only on every Hub. The Hub cannot save or apply them from its settings database yet (ptone/scion#3067).

### Layout Structure

To streamline management of complex environments, the **General** settings tab is restructured into three high-density, focused cards:

1. **General Card**: Houses central Hub identity, registration endpoints, and core server configurations.
2. **Agent Defaults Card** (with dedicated sub-tabs):
   - Configures default resource constraints (`max_turns`, `max_duration`, limits). These fields can be cleared to blank in the UI, which persists them as `null` in the Hub settings database instead of preserving their previous values, allowing administrators to remove default constraints entirely.
   - Introduces **Default Model** (`default_model`), **Default Thinking Level** (`default_thinking_level`), and **Default Agent Role** (`default_agent_role`) fields directly into the agent default pipeline (with the default agent role updated from `baseline` to `full` for usability).
   - Introduces **Default Runtime Broker** (`default_runtime_broker`), a hub-level default that participates in the [broker resolution cascade](/scion/hosted/ha/multi-broker/#broker-selection). When a project has no default broker and no broker is explicitly requested, the hub-level default is used if the broker is online and dispatchable.
   - **Default Timezone** (`agent_defaults.default_timezone`): an IANA zone picker for the `TZ` sent to agent containers that have no pinned timezone and no `TZ` in the Hub environment-variable store. Empty means no default, so those containers use the image default (UTC). Invalid names and `Local` are rejected with `422`. It sets agent containers only, not how the web UI displays times. See [Times and Timezones](/scion/reference/times-and-timezones/#agent-tz-hub-dispatched-agents) for the full chain.
   - Adds a hub-level **Default GCP Identity Mode** (`agent_defaults.default_gcp_identity_mode`) with an optional service account picker (`default_gcp_identity_service_account_id`). It is the fallback for new agents when neither the create request (not applicable to a scheduled dispatch, which has none) nor the project's default GCP identity sets a mode — including agents dispatched by a schedule, which follow the same fallback ladder as interactive/API creates. **Passthrough** applies only on the Hub's own embedded (co-located) broker, as recorded by the Hub server at startup; on any other broker the hub default is left unset, so the dispatching broker applies its own runtime-aware default instead (**Block** on most runtimes, **Passthrough** on Kubernetes — see below). A broker label cannot opt a broker in. **Assign** accepts only a verified hub-scoped service account and requires `gcp_iam_check_mode: enforce`. Other values are rejected with `422`. The setting is stored in the Hub settings database on every driver (SQLite or Postgres). It applies to new agents without a restart and persists across restarts. **Block** is not offered on the Kubernetes runtime: an explicit **Block** (from any rung, including this one) fails a Kubernetes dispatch instead of starting the agent, and a dispatch with no mode configured anywhere resolves to **Passthrough** rather than **Block** specifically on Kubernetes. See [Hub-Default GCP Identity](/scion/hosted/ha/permissions/#hub-default-gcp-identity).
   - Houses the **Telemetry Toggle**, which has been moved to this card to keep telemetry configuration closely aligned with operational defaults.
3. **Project Default Settings Card**: Configures platform-level default annotations and behaviors for newly created projects.
4. **Quotas Card**: Houses the **Enforce broker agent quotas** switch (`quotas.enforce_broker_quotas`), on by default. When turned off, agent creates on a broker are no longer rejected once `max_agents_per_broker` is reached — usage is still counted (reservations, release, reconcile and backfill are unaffected), so the Admin → Quotas usage page keeps showing the true count (e.g. "31 / 16") while it is off, and turning the switch back on makes the next over-cap create return `429` immediately with no reconcile wait. The switch only affects `max_agents_per_broker`; other quota limits (such as `max_agents_per_project`) are always enforced. This is a normal Layer-1 section on every driver (`PUT /api/v1/admin/server-config`, no restart, propagates to replicas within 60s). The card links to Admin → Quotas, where the hub-wide `max_agents_per_broker` default itself is edited.
5. **Agent Secrets Card**: Houses the **Restrict agent-written secrets to profile scope** switch (`agent_secrets.user_scope_only`), off by default. When on, the hub rejects every agent-originated secret write that resolves to project scope — including an empty scope, which defaults to project — with `403 secret_scope_restricted`, whatever harness, flag, or `force` is used. This covers the web terminal's Capture Auth button (which disables the Project option and preselects Profile while the setting is on) and ad-hoc `sciontool secret set` calls alike. User-originated project writes (web UI, `scion hub secret set --project`, chat and Discord apps) and agent user-scope writes are unaffected. Existing project secrets are not removed or migrated — turning the setting on stops new agent writes only; use the project secrets UI to clean up existing ones. This is a normal Layer-1 section on every driver (`PUT /api/v1/admin/server-config`, no restart, propagates to replicas within 60s).

### Runtimes & Profiles Tab

To support complete infrastructure configuration directly from the Web Dashboard, the admin page features a dedicated **Runtimes & Profiles** tab. This tab provides full CRUD (Create, Read, Update, Delete) editors for core execution settings:

- **Runtimes Editor**: Type-aware input fields tailored to specific runtime types:
  - **Docker & Podman**: Fields for daemon sockets, host networks, and storage paths.
  - **Kubernetes (GKE)**: Fields for cluster endpoints, namespace scoping, service account bindings, and PVC volumes.
  - **Cloud Run**: Fields for service endpoints, memory limits, and CPU allocation.
- **Profiles Editor**: Configures resource constraints (limits), target container image registries, and specific template overrides. Supports a native Runtime Profile Selector when launching or configuring agents.
- **Harness Configs Editor**: Features a rich JSON textarea for writing and editing raw harness properties directly, making it easy to tune model parameters or customize env environments.

In highly available (HA) Postgres deployments, all configurations edited through this tab are stored in the database's `hub_settings` table as whole-map JSONB documents. This ensures edits propagate immediately to all replicas, support optimistic locking (CAS), and are never silently dropped or ignored on server PUT actions.

Furthermore, these database-backed settings are wired directly into the runtime broker consumption path. During agent dispatch, both the Scion Hub and the Runtime Broker resolve runtime execution environments and resource profiles dynamically from these persisted database settings. This eliminates the need to distribute or maintain on-disk configuration files (such as local `settings.yaml` files) on individual broker nodes, ensuring a centralized, real-time control plane.

#### Navigation Updates

- **Message Broker Integration**: The Message Broker configuration has been consolidated and moved from the General tab to the **Hub Server** tab, grouping external integrations and network-bound transports in one logical place.

### Visual Indicators

| Indicator | Meaning |
|-----------|---------|
| 🔒 *Managed via deployment configuration* | Layer-0 field on a hosted Hub — not editable |
| 🔒 *Set via environment variable* | Field pinned by `SCION_SERVER_*` on a workstation Hub |
| 🔒 *Set by workstation startup defaults / server flags* | Workstation field that `scion server start` overrides at every start |
| *Tracking deployment configuration* | Seeded section — re-syncs on restart |
| ⓘ *Superseded by database value* | Deployment config differs from the admin-set value |
| Deprecation banner | `SCION_SERVER_*` used for Layer-1 settings |

### Error Handling

The admin UI provides structured feedback on save:

| Response | UI Treatment |
|----------|-------------|
| **200** | Success; shows ignored-keys notice if applicable |
| **400** `validation_failed` | Inline per-section validation errors |
| **409** `revision_conflict` | "Settings changed since you loaded this page" banner with Reload button |
| **422** `layer0_rejected` | Safety-net notice on hosted Hubs (should not occur with layer-aware UI) |
| **422** `unclassified_keys_rejected` / `unpersisted_keys_rejected` / `hub_owned_keys_rejected` / `unsaved_keys_rejected` | Shows the message and the offending keys; nothing was saved |
