---
title: Admin Settings Model
description: How Scion manages operational settings in database (postgres) mode, including the seeded/managed lifecycle, environment variable namespaces, and the admin UI.
---

This document describes how Scion manages operational settings when running with a postgres database, including the configuration precedence model, the seeded/managed lifecycle, and the admin settings UI.

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
| **Layer-0** (bootstrap) | `server.mode`, `server.database.*`, `server.storage.*`, `server.secrets.*`, `server.hub.port`, `server.auth.dev_mode` | Always resolved from bootstrap configuration. Cannot be changed via the admin UI in database mode. |
| **Layer-1** (operational) | `server.hub.admin_emails`, `server.auth.user_access_mode`, `telemetry.*`, `agent_defaults.*`, `server.github_app.*`, `server.notification_channels` | In database mode, stored in the database and editable via the admin UI. Bootstrap values serve as initial defaults. |

### Database Mode

In database (postgres) mode, Layer-1 settings follow this resolution:

```
Database value > Bootstrap merge (for Layer-1 keys)
Bootstrap only (for Layer-0 keys)
```

The database always wins for Layer-1 keys. Bootstrap values are used as initial seeds and as fallback when no database row exists.

### File Mode

In file mode (sqlite / no postgres), all settings come from the bootstrap merge. The admin UI writes directly to `settings.yaml`.

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
| `server.hub.auto_suspend_stalled` | `SCION_SEED_SERVER_HUB_AUTOSUSPENDSTALLED` |
| `server.hub.soft_delete_retain_files` | `SCION_SEED_SERVER_HUB_SOFTDELETERETAINFILES` |
| `server.hub.image_registry` | `SCION_SEED_SERVER_HUB_IMAGEREGISTRY` |
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
| `server.log_level` | `SCION_SERVER_LOGLEVEL` |

### `SCION_SERVER_*` Deprecation for Layer-1

:::caution[Deprecation]
Using `SCION_SERVER_*` for Layer-1 operational settings is **deprecated**. These variables still work during the transition period (they are treated as the top of the bootstrap merge), but a warning is logged on startup.

**Migration:** Replace `SCION_SERVER_<KEY>` with `SCION_SEED_<KEY>` for any Layer-1 setting.

Example:
```diff
- SCION_SERVER_HUB_ADMINEMAILS=admin@example.com
+ SCION_SEED_SERVER_HUB_ADMINEMAILS=admin@example.com
```
:::

The deprecation notice also appears in the admin UI when deprecated variables are detected.

## Maintenance Mode

### Break-Glass Removal

`SCION_SERVER_ADMIN_MODE` no longer forces maintenance mode on a per-node basis. In previous versions, setting this environment variable would override the database maintenance state on that specific node, creating inconsistency in HA deployments.

Maintenance mode is now **cluster-consistent**:
- Set via the admin API (`PUT /api/v1/admin/maintenance`)
- Propagated to all replicas via the event system
- Survives restarts (persisted in the database)
- Cannot be overridden by per-node environment variables

## HA Bootstrap Guidance

For high-availability deployments with multiple hub replicas:

1. **Initial setup:** Use `SCION_SEED_*` environment variables to configure operational settings across all replicas. All replicas share the same bootstrap configuration.

2. **After first admin edit:** Once an admin edits a setting in the UI, the section becomes managed. The database value propagates to all replicas within ~2 seconds via the event system. No per-node env configuration is needed.

3. **Updating seeds:** To change the default for a seeded (unedited) section, update the `SCION_SEED_*` variable and restart the replicas. The new value syncs to the database on boot.

4. **Managed sections:** For sections that have been admin-edited, changing the `SCION_SEED_*` variable has no effect — the database value wins. A "superseded" notice in the admin UI shows which deployment values differ from the database.

5. **Resetting:** To return a managed section to tracking mode, use "Reset to bootstrap" in the admin UI. The section reverts to the seed value and will track future seed changes.

## Admin UI

The admin settings page (`/admin/server-config`) is layer-aware:

- **Database mode:** Layer-0 fields are read-only with a "Managed via deployment configuration" badge. Layer-1 fields are editable.
- **File mode:** Fields pinned by `SCION_SERVER_*` environment variables are read-only with a "Set via environment variable" badge. Other fields are editable and write to `settings.yaml`.

### Visual Indicators

| Indicator | Meaning |
|-----------|---------|
| 🔒 *Managed via deployment configuration* | Layer-0 field in database mode — not editable |
| 🔒 *Set via environment variable* | Field pinned by `SCION_SERVER_*` in file mode |
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
| **422** `layer0_rejected` | Safety-net notice (should not occur with layer-aware UI) |
