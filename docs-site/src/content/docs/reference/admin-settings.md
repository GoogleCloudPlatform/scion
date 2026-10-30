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

`settings.yaml` is the global `~/.scion/settings.yaml`. With `scion server start --config <path>`, the merge picks its settings file with the same rule the server uses to load its configuration at startup:

- If the global file has a `server` key, it is used and the `--config` `settings.yaml` is not read.
- If the global file has no `server` key and the `--config` `settings.yaml` has one, the `server` block and the top-level hub sections (`telemetry`, `quotas`, `agent_secrets`, `project_defaults`, `default_harness_config`, `default_timezone` and `default_gcp_identity_*`) come only from the `--config` file. Other keys it does not set, such as `image_registry`, `runtimes` and `profiles`, still come from the global file.
- If neither file has a `server` key, the `--config` `settings.yaml` is used only when there is no global `settings.yaml`, and the legacy `server.yaml` in the `--config` location, or the named file, is layered as at startup.

### Layer-0 vs Layer-1 Keys

Settings are classified into two layers:

| Layer | Examples | Behavior |
|-------|----------|----------|
| **Layer-0** (bootstrap) | `server.mode`, `server.database.*`, `server.storage.*`, `server.secrets.*`, `server.hub.port`, `server.auth.dev_mode` | Always resolved from bootstrap configuration (`settings.yaml` and environment). Editable through the admin UI only on workstation Hubs; see [Layer-0 edits by deployment mode](#layer-0-edits-by-deployment-mode). |
| **Layer-1** (operational) | `server.hub.admin_emails`, `server.auth.user_access_mode`, `server.auth.default_user_role`, `server.hub.public_url`, `server.hub.hub_name`, `image_registry`, `telemetry.*`, the agent defaults (top-level `default_*` keys such as `default_template` and `default_timezone`), `server.github_app.*`, `server.notification_channels`, `server.federation.*`, `runtimes`, `profiles`, `harness_configs`, `auto_expose_ports.enabled`, `quotas.enforce_broker_quotas`, `agent_secrets.user_scope_only`, `project_defaults.default_scratchpad`, `server.hub.observability.metrics_enabled`, `server.hub.observability.tracing_enabled` | Stored in the Hub database on every driver and editable via the admin UI. Bootstrap values serve as initial defaults. |

For the full key lists by section, see [Layer 0](/scion/reference/server-config/#layer-0--bootstrap-file--env-only) and [Layer 1](/scion/reference/server-config/#layer-1--operational-hub_settings-table) in the server configuration reference.

### Resolution

On every Hub, SQLite or Postgres, Layer-1 settings follow this resolution:

```
Database value > Bootstrap merge (for Layer-1 keys)
Bootstrap only (for Layer-0 keys)
```

The database always wins for Layer-1 keys. Bootstrap values are used as initial seeds and as fallback when no database row exists. Editing a Layer-1 key in `settings.yaml` therefore only changes the seed: once an admin has saved that section through the API, the database value wins, on SQLite exactly as on Postgres.

This applies to `SCION_SERVER_*` too. On a Layer-1 key it is only the top of the bootstrap merge: it changes the seed (which a seeded section re-syncs from at restart) and the fallback, and it never overrides a value an admin has saved. The admin UI marks such keys (see [Visual Indicators](#visual-indicators)), and the variable is deprecated for Layer-1 keys (see [`SCION_SERVER_*` Deprecation for Layer-1](#scion_server_-deprecation-for-layer-1)).

### `server.hub.public_url`: environment first

`server.hub.public_url` is the one Layer-1 key where an operator setting on the node ranks above the database. The Hub resolves it once at startup, in this order:

1. **Operator pin.** If the `--base-url` flag, `SCION_SERVER_HUB_ENDPOINT` or `SCION_SERVER_BASE_URL` is set, the Hub uses its operator-configured endpoint: the first of them that is set, in that order, as described in [Hub Endpoint Resolution](/scion/reference/server-config/#hub-endpoint-resolution). A value saved in the database is not used.
2. **Database.** Otherwise a value an admin saved through the admin API or UI is used, even when `settings.yaml` also sets one. A stale bootstrap file therefore does not override a value managed in the UI. Rows the Hub seeds from bootstrap material at startup do not count as saved values, and a save that leaves the Public URL unchanged (or sends back the value shown) does not store it. A public URL stored by a save in an earlier release stays in effect until an admin clears it.
3. **File.** Otherwise the `settings.yaml` value applies, then the existing fallbacks.

The endpoint resolved this way is the Hub endpoint everywhere it is used: the URL dispatched agents receive (unless `server.hub.agent_endpoint` is set), invite links, chat-bridge settings, the `hub.endpoint` written into hub-managed project settings, the broker join response, the OIDC issuer default and the `cloudrun_invoker` transport audience default. The web server's base URL (OAuth redirect base, session cookie `Secure` flag, and browser config) is resolved separately at startup from operator inputs only and never reads the settings database; see [Web Base URL](/scion/reference/server-config/#web-base-url).

- **Restart required.** A save is stored at once and listed in the save report's `pending_restart`. The Hub picks it up at the next restart.
- **Validation.** A value saved in the database must be an absolute URL of the form `scheme://host[:port]`: `https`, or `http` only for `localhost` or a loopback address; a host; no user credentials, query, fragment or path. A trailing `/` is removed. The admin API rejects anything else with `422 validation_error` naming `server.hub.public_url`. At startup an invalid database value is logged as an error and not used; the operator-configured endpoint applies instead. Values from the environment, flags and `settings.yaml` keep their existing handling.
- **Startup log.** The Hub logs one line with the operator-configured endpoint and its source, and one line with the endpoint in effect and its source (`env`, `flag`, `database`, `file`, or a fallback such as `localhost`). When a database value replaces a `settings.yaml` value, it logs that too.
- **Audit.** A change to the stored value, by a save or a section reset, is recorded as a `hub_setting_update` audit record with the actor and the old and new values, before it is written. If the record cannot be written, the change is not made. If the write then fails, a `hub_setting_update_not_applied` record follows.
- **Admin UI.** When an operator setting pins the endpoint, the Public URL field is read-only and names the setting that pins it. Otherwise the field is editable and shows the saved value or, with none saved, the `settings.yaml` or environment value; it never shows a seed value, which startup does not use. An **In effect** hint next to it shows the endpoint in effect and its source (the admin API reports it in `public_url_status`).

The environment variable for this key is `SCION_SERVER_HUB_ENDPOINT`, and its seed variable is `SCION_SEED_SERVER_HUB_ENDPOINT`. Startup does not use the seed variable to resolve the endpoint, so the admin UI does not show its value as the field value. The spellings `SCION_SERVER_HUB_PUBLICURL` and `SCION_SEED_SERVER_HUB_PUBLICURL` are no longer read; the Hub warns about them at startup and names the replacement. Because the environment pins this key, `SCION_SERVER_HUB_ENDPOINT` is not part of the `SCION_SERVER_*` deprecation for Layer-1 keys.

### Hot reload vs restart

- **Layer-1:** a save applies without a restart. The serving node applies it immediately, and other replicas pick it up through the settings event (within about 2 seconds), with a 60-second poll as a backstop. Live applies are ordered by a store-wide revision so an older snapshot never overwrites a newer one. A few Layer-1 keys take effect only after a restart. The schema endpoint (`GET /api/v1/admin/server-config/schema`) lists them per section in `restart_required`: today `server.hub.public_url` and `server.github_app.app_id`.
- **Layer-0:** changes take effect only when the Hub restarts. On a workstation Hub, a save that changes a Layer-0 key in `settings.yaml` lists it in the response's `reload.requires_restart`. `log_level` is the exception and applies immediately.
- **Restoring `hub_settings`:** the Hub orders settings by a revision counter stored in `hub_settings` (the reserved `_settings_revision` row). If the table is restored from a backup, or the database is replaced, while a Hub is running, the stored counter can be lower than the one the Hub has seen. Each replica detects this on its next settings read (the settings event or the 60-second poll), logs a warning (`the stored settings revision went backward`) and resynchronises its settings to the database. This assumes that every replica reads from the one primary database it writes to; the Hub does not route reads to read replicas. The database URL must therefore target the read-write primary: with several hosts in the URL, set `target_session_attrs=read-write`, and do not put the Hub behind a proxy that sends some connections to a standby. Restarting the Hub after a restore is still the most predictable option, in particular for the keys that take effect only after a restart.

Clearing one of these Layer-1 keys returns the running Hub to the value it started with (from `settings.yaml`, the seed or the environment): `soft_delete_retention`, `soft_delete_retain_files`, `auto_expose_ports.enabled` and `telemetry.enabled`. Clearing `image_registry` returns it to the settings-file value (see below). Two sections revert as a whole:

- **GitHub App:** clearing `app_id` returns all the non-secret GitHub App fields to their startup values. While `app_id` is set, clearing another field applies the empty value, except `private_key_path`, which keeps the startup path.
- **Telemetry:** a telemetry section with no keys returns to the startup telemetry config. Clearing one telemetry key while others remain applies the config without that key, not its startup value.

Some admin fields are saved only when you change them:

- **Federation page:** a save sends only the fields that differ from the values the page loaded, and the Hub keeps every key a request leaves out (in database and `settings.yaml` modes alike). A default the page shows for a key `GET` left out (`enabled`, `algorithms`, `refresh_interval`, `debounce_interval`) is therefore not stored unless you edit it. Adding, editing or removing an issuer saves straight away, sending `trusted_issuers` together with any other field you have changed.
- **Server Config page:** the `auto_expose_ports.enabled`, `telemetry.enabled`, `telemetry.cloud.enabled`, `telemetry.cloud.cloud_logging`, `telemetry.hub.enabled`, `quotas.enforce_broker_quotas` and `agent_secrets.user_scope_only` switches show **Not set** while no value is stored and are sent only when you change them; **Use default** clears a stored value (the page sends `null`). `default_runtime_broker` is sent only when you change it, so a broker name or slug stays as you wrote it although the dropdown shows the broker. Some other Server Config fields are still sent on every save, including the default the page shows when `GET` left the key out: `server.auth.user_access_mode` and `server.auth.default_user_role`, and in `settings.yaml` mode also `server.broker.auto_provide` and `server.native_chat.enabled`. On a workstation Hub, Layer-0 and `settings.yaml`-only fields are sent only when changed (see [Layer-0 edits by deployment mode](#layer-0-edits-by-deployment-mode)).
- **`telemetry.cloud.cloud_logging` on a database-backed Hub:** the Hub reads this switch only at startup, from `SCION_CLOUD_LOGGING` or `settings.yaml`, before the settings database is opened, so a stored value would never take effect. A database-backed `PUT /api/v1/admin/server-config` (workstation Hubs included) therefore refuses a value sent for the key itself (`true`, `false` or `null`, at any nesting or dotted spelling) that differs from what `GET` shows, with `422` and `{"error": "startup_only_key_rejected", "keys": ["telemetry.cloud.cloud_logging"]}`, and writes nothing. Sending back the value `GET` shows is accepted and stores nothing. The check looks only at the key: a body that replaces or nulls a parent object (`{"telemetry": {"cloud": null}}`) is not checked for it and follows the normal telemetry rules, which can drop a stored, unused value. The Server Config page shows the switch read-only there and never sends it. In `settings.yaml` mode the switch stays editable and applies at the next start.
- **Integrations page:** an unset text field shows the plugin default as its placeholder, and an unset select or switch shows the default as its value; neither is saved unless you change it. Clearing a stored text value, or **Use default** on a select or switch, sends `null` in `PUT /api/v1/admin/integrations/{name}/config`, which deletes the key from the plugin config so the plugin default applies again. Turning a stored switch off sends `"false"`.

For `soft_delete_retention` and `soft_delete_retain_files`, `GET` returns that running value while no value is stored. A save that sends the same value back leaves the key unset: it is not stored and not reported. A retention is compared as a duration, so `72h0m0s` matches `72h`.

`image_registry` follows one order everywhere; see [Image registry order](/scion/reference/settings-precedence/#image-registry-order). In the admin settings:

- The top-level field shows the saved value, or else this Hub replica's settings-file value (`settings.yaml`, `SCION_SEED_IMAGEREGISTRY` and `SCION_SERVER_IMAGEREGISTRY`). It never shows an environment value such as `SCION_IMAGE_REGISTRY`. A save stores the field only when it differs from the value shown, so saving the page unchanged stores nothing. To pin the settings-file value, change it once.
- Each profile's field shows only a value saved for that profile. A hint shows what the profile inherits and from where. Typing a value saves it.
- `image_registry_provenance` in the `GET` response is read-only and tells you where the value in effect comes from: the Hub-wide source and value, every Runtime Broker that reports `SCION_IMAGE_REGISTRY` (which beats every saved or file value for that Runtime Broker's agents), and each profile's value. The page shows it as one badge on the field. Every value in the report has its credentials part (everything up to the last `@` that is not part of a digest, after any scheme) replaced with `***`, as in `***@registry.example.com` for `user:password@registry.example.com`.

#### Save report

A database-backed save (`PUT /api/v1/admin/server-config`) returns a `reload` object:

| Field | Meaning |
|---|---|
| `applied` | Database sections written, plus, on a workstation Hub, the `settings.yaml` changes applied live. |
| `applied_keys` | Changed Layer-1 keys (koanf paths, for example `server.hub.soft_delete_retention`) that the running Hub now uses. |
| `pending_restart` | Changed Layer-1 keys flagged `restart_required`: saved, but used only after a restart. |
| `skipped` | Changed Layer-1 keys that were saved but are not in use, as a list of `{"key", "reason"}` objects. They are not in `applied_keys`. |
| `file_applied` | Workstation Hubs: the `settings.yaml` changes applied live (also listed in `applied`). |
| `requires_restart` | Workstation Hubs: the `settings.yaml` changes that need a restart. |

A key is listed in `applied_keys` or `pending_restart` only when its value changed. An echoed `server.hub.hub_name` equal to the hub name in effect (the bootstrap value when no `endpoints` row is stored yet) is not a change and is not listed. GitHub App fields other than `app_id` are listed under neither heading while the saved `github_app` section has no `app_id`, because they are not used then; a change to `app_id` itself is listed in `pending_restart`. When the running Hub cannot use a saved change, the key is listed in `skipped` instead of `applied_keys`: `server.notification_channels` when a notification channel fails validation (the reason gives the channel's position; the channels that pass validation are in use), and the changed `server.federation.*` keys when the federation configuration is not installed: it fails validation, its authenticator cannot be built, or the test admin tier is on. Rarely, the Hub that serves the save cannot include a section it just wrote in its own update (it could not re-read the database after the write, or newer writes kept overtaking the re-read); that section's changed keys are then listed in `skipped` with the reason `Saved; this Hub applies it on its next settings refresh.` The change is saved, and the Hub picks it up within the usual propagation time. A reason never repeats a configured value. The Server Config page lists these keys under **Saved but not applied**.

A `409 revision_conflict` response covers the sections written before the conflict:

| Field | Meaning |
|---|---|
| `applied` | Revisions of the sections this request wrote. |
| `unchanged_revisions` | Current revisions of sections the request sent unchanged; they were not written. |
| `conflicted` | The section that conflicted, with its expected and current revision. |
| `applied_keys`, `pending_restart`, `skipped` | As in `reload`, for the sections written before the conflict. |

When a save hits `409 revision_conflict` after earlier sections were written, the Server Config conflict banner also lists the keys saved before the conflict: `applied_keys` under **Applied now**, `pending_restart` under **Saved; applies after restart**, and `skipped` under **Saved but not applied**.

### Layer-0 edits by deployment mode

Whether Layer-0 settings can be edited through `PUT /api/v1/admin/server-config` depends on the deployment mode, not on the database driver. `GET` reports it as `layer0_editable`.

- **Hosted Hubs** (started with `--hosted`, or `server.mode: hosted` in `settings.yaml` / `SCION_SERVER_MODE=hosted`): Layer-1 keys are written to the database. A Layer-0 field the body carries is rejected with `422 layer0_rejected` when its value differs from what `GET` returns, an explicit `false`, `""` or `0` included. Fields with no database home (such as `schema_version`, `active_profile`, `workspace_path` or `auto_inject_gcloud_adc`) follow the same rule with `422 unclassified_keys_rejected` or `422 unpersisted_keys_rejected`. An unchanged value is ignored, so sending back the body `GET` returned is a `200` that writes nothing. Deployment tooling owns `settings.yaml`.
- **Workstation Hubs** (the default mode): Layer-1 keys are written to the database. Layer-0 keys and keys with no database home are written to `settings.yaml`. `log_level` is applied immediately and the co-located broker reloads its runtime. Every other key that changed in the file is listed in the response's `reload.requires_restart`, and `file_keys` lists every key that changed in the file.

Layer-1 keys written to the database are merged section by section onto the stored row: a key the body leaves out keeps its stored value, an explicit `null` clears it, and a save that races another write to the same section returns a `409 Conflict` error instead of overwriting it.

Federation settings are sent as the top-level `federation` object of the `PUT` body, in database and `settings.yaml` modes alike. On a database-backed Hub, federation sent under `server.federation` is refused with `422` unless it repeats what `GET` returns. `{"federation": null}` is refused in every mode with `422 validation_error` and `details.keys: ["federation"]`; nothing is saved. To return the federation settings to their bootstrap values, use `DELETE /api/v1/admin/server-config/sections/federation` (see [Reset to Bootstrap](#reset-to-bootstrap)). To clear single fields, send them explicitly (`false`, `[]` or `""`). `GET` reports the federation settings in effect in both places it shows them, the top-level `federation` object and `server.federation`. On a database-backed Hub these are the stored values (or the bootstrap values while no row is stored), never a `settings.yaml` value the stored row replaced. A stored federation section that sets `enabled` or `trusted_issuers` is shown even when it is disabled and has no trusted issuers, with its other fields such as `algorithms` or the intervals.

`server.mode` must be empty (workstation), `workstation`, `hosted` or `production` (the legacy name for hosted). Values are case-sensitive. The server refuses to start with any other value (a typo used to mean workstation), and a workstation save that sets another value is rejected with `400`. To fix a bad value, edit `server.mode` in `settings.yaml` or `SCION_SERVER_MODE`; `--hosted` selects hosted mode but does not make a bad value valid.

On a workstation Hub the file edit follows the request body field by field:

- Each field the body carries is written exactly as sent, an explicit empty value (`""`, `false`, `0`, `[]`) included. Only `null` removes a key or block.
- The Hub then compares the settings it would load from the edited file with the settings it loads today (the server config the Hub builds, the typed settings, and the `active_profile`/`workspace_path` merge over the built-in defaults). Fields whose effective value does not change are not written, not listed in `file_keys` and not reported as needing a restart. If nothing effective changes, the file is not written, so saving an unchanged form writes nothing. For example, `""` for a key the file does not have changes nothing and is skipped. `[]` for `server.hub.cors.allowed_origins` replaces the default list with an empty one and is written.
- A field whose value in the edited file would not be what was sent is rejected with `422 unsaved_keys_rejected` naming it, and nothing is saved.
- Fields the body leaves out are kept.
- Creating a `cors` block (`server.hub.cors` or `server.broker.cors`) without sending `enabled` also writes `enabled` with its current value. Inside the block a missing `enabled` means off. A missing block means on for the Hub and off for the broker (broker CORS is off by default), so adding an origin never silently changes whether CORS is on.
- `server.broker.broker_id` and `broker_token` are written by the Hub itself. A save may send them back unchanged (the token as `********`), but changing or clearing them is rejected with `422 hub_owned_keys_rejected`. A `null` on a block that contains them (`{"server":null}` or `{"server":{"broker":null}}`) removes everything else in that block and keeps them.
- If `settings.yaml` has no `server` key and a deprecated `server.yaml` is in effect (in `~/.scion`, in the server's `--config` path, or in its working directory), the Hub still reads its server configuration from `server.yaml`. A save that would create the `server` block in `settings.yaml` (and so drop every `server.yaml` setting at the next start) is refused with `409 legacy_server_yaml`. Move the contents of `server.yaml` under a top-level `server:` key in `settings.yaml`, remove `server.yaml`, then save again.
- The admin UI sends only the fields you changed since the page loaded, so defaults the form shows for keys the file does not have are never written.
- The file is edited in place: comments, key order and keys the Hub does not know survive. A `settings.yaml` that uses YAML anchors or aliases on an edited path cannot be edited in place, and the save is rejected with `422` (edit that file by hand).

Some workstation fields are overridden at every start by the workstation defaults and `scion server start` flags, so a value in `settings.yaml` has no effect: `server.broker.enabled` (`--enable-runtime-broker`), `server.hub.host` (`--host`), `server.auth.dev_mode` (`--dev-auth`), `server.storage.provider` (`--storage-bucket`) and `server.secrets.backend`. The admin UI shows them read-only with a "Set by workstation startup defaults / server flags" badge. In workstation mode the runtime broker listens on 127.0.0.1 unless `server.broker.host` is set to another address; `--host` does not move it, and `0.0.0.0` counts as unset.

A workstation save that touches both destinations is checked as a whole first. Any validation failure, for either part, writes nothing. The Hub then takes its settings-file lock and prepares the new `settings.yaml` in memory, writes the database sections, and writes the file last, atomically, before releasing the lock. These writers in the same server process take the same lock for their whole read-modify-write, so neither of two concurrent changes is lost: Runtime Broker registration writing its ID and token, the identity and workstation-settings endpoints, the plugin-registration writes made by the integrations API, and the hub-sync cleanup of project settings. The loopback runtime and image-registry endpoints (`PUT /api/v1/system/runtime` and `PUT /api/v1/system/registry`) write the database `profiles` and `endpoints` sections, because those are Layer-1 keys. The lock also works across processes, so a `scion` CLI command that writes settings while the server runs waits for it too. It is a file lock on `.settings.lock` in the settings directory. A writer that cannot get it within 10 seconds fails with an error that says to try again, and writes nothing. If a database write fails (for example `409 revision_conflict`), `settings.yaml` is left unchanged. Database sections written before the failure stay written, as for any save that touches several sections, and are listed in `applied`. Only if the final file write fails are the database changes saved without the file changes; the `500` response lists them.

Of the system routes, only `PUT /api/v1/system/identity` and `PATCH /api/v1/system/workstation-settings` require a Hub admin (the `hub.config.update` permission, held by the hub-admin and super-admin roles; the workstation's dev user holds it as super-admin); other signed-in callers get `403`.

On every Hub, a key the API cannot store anywhere (an unknown or misspelled key, or a field with no settings home) is rejected with `422 unpersisted_keys_rejected`, unless its value is unchanged from what `GET` returns. Keys that are set only in the settings file or the environment and are never writable through the API (such as `server.hub.gcp_project_id`, which is also omitted from `GET /api/v1/admin/server-config` and `env_overrides` and has no UI field; see [Hub server settings](/scion/reference/server-config/#hub-settings-serverhub)) are rejected with `422` `validation_error` in every mode, with the refused keys in `keys`, even when sent as `""` or `null`. A save never answers `200` for a key it dropped. An empty body is a `400`.

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

:::tip[Boolean environment variables]
A `SCION_SERVER_*` or `SCION_SEED_*` variable for a boolean setting takes `true`/`false`, `1`/`0` or `t`/`f`. A value that does not parse stops the server at startup with an error that names the variable (never the value); see [Environment Variables](/scion/reference/server-config/#environment-variables). Only the directly read variables under [Tracked Boolean Variables](/scion/reference/server-config/#tracked-boolean-variables) also accept `yes`/`y`/`on` and `no`/`n`/`off`.
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
| `server.hub.public_url` | `SCION_SEED_SERVER_HUB_ENDPOINT` |
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
| `server.log_level` | `SCION_SERVER_LOGLEVEL` (applied at startup and on a workstation server-config save, see below) |

`server.log_level`, or `SCION_SERVER_LOGLEVEL`, is applied when the server starts and again when a workstation admin server-config save that changes `server.log_level` re-reads the config. Clearing it reverts to `info`. `--debug` and `SCION_LOG_LEVEL` take precedence over it.

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

## Profiling

The `profiling` section holds operational switches for in-app profiling. It is stored only in the Hub database: it has no `settings.yaml` key and no environment variable, and it is not seeded. A change applies to every replica through the usual settings propagation.

| Key | Type | Default | Effect |
|-----|------|---------|--------|
| `readiness_marks` | boolean | `false` | The web client writes readiness marks (User Timing marks for agent data arrival, first visible rows and graph ready). See [readiness marks](/scion/contributing/perf-tracing/#readiness-marks). |

- **Changing it:** `PUT /api/v1/admin/profiling` with `{"readiness_marks": true}` (or `false`, or `null` to reset). It needs a hub administrator on an interactive session; user access tokens are refused. There is no admin UI card. `GET /api/v1/admin/profiling` returns the current value and the section revision.
- **Reading it:** `GET /api/v1/profiling` returns `{"readinessMarks": true|false}` to any signed-in caller.
- **In the web client:** while it is on, the page shell's initial data carries `"readinessMarks": true` for a signed-in user, the same value that user reads from `GET /api/v1/profiling`. Signed-out pages, and every page while it is off, are served exactly as without the setting. The client reads the value once per page load, so a change applies on the next full load.
- Off, the client makes no `performance.mark` calls and the server adds nothing to the shell.

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
   - Configures default resource constraints (`max_turns`, `max_duration`, limits). These fields can be cleared to blank in the UI, which persists them as `null` in the Hub settings database instead of preserving their previous values, allowing administrators to remove default constraints entirely. A change to the limits (`default_max_turns`, `default_max_model_calls`, `default_max_duration`) or to `default_resources` reaches existing agents at their next start or restart, unless the agent, its template or (for resources) its broker profile sets the value. Agents created before this behaviour was added keep the limits they have stored. See [Where hub `agent_defaults` rank](/scion/reference/settings-precedence/#where-hub-agent_defaults-rank).
   - Introduces **Default Model** (`default_model`), **Default Thinking Level** (`default_thinking_level`), and **Default Agent Role** (`default_agent_role`) fields directly into the agent default pipeline (with the default agent role updated from `baseline` to `full` for usability).
   - Introduces **Default Runtime Broker** (`default_runtime_broker`), a hub-level default that participates in the [broker resolution cascade](/scion/hosted/ha/multi-broker/#broker-selection). When a project has no default broker and no broker is explicitly requested, the hub-level default is used if the broker is online and dispatchable.
   - **Default Timezone** (`agent_defaults.default_timezone`): an IANA zone picker for the `TZ` sent to agent containers that have no pinned timezone and no `TZ` in the Hub environment-variable store. Empty means no default, so those containers use the image default (UTC). Invalid names and `Local` are rejected with `422`. It sets agent containers only, not how the web UI displays times. See [Times and Timezones](/scion/reference/times-and-timezones/#agent-tz-hub-dispatched-agents) for the full chain.
   - Adds a hub-level **Default GCP Identity Mode** (`agent_defaults.default_gcp_identity_mode`) with an optional service account picker (`default_gcp_identity_service_account_id`). It is the fallback for new agents when neither the create request (not applicable to a scheduled dispatch, which has none) nor the project's default GCP identity sets a mode — including agents dispatched by a schedule, which follow the same fallback ladder as interactive/API creates. **Passthrough** applies only on the Hub's own embedded (co-located) broker, as recorded by the Hub server at startup; elsewhere, including any Kubernetes profile, the hub default is left unset and the agent gets the runtime default (**Block**, or **Passthrough** on Kubernetes). A broker label cannot opt a broker in. **Assign** accepts only a verified hub-scoped service account and requires `gcp_iam_check_mode: enforce`. Other values are rejected with `422`. The setting is stored in the Hub settings database on every driver (SQLite or Postgres). It applies to new agents without a restart and persists across restarts. On the Kubernetes runtime an explicit **Block** (from any rung, including this one) runs the agent pod as a zero-privilege ServiceAccount; see [block on Kubernetes](/scion/hosted/ha/kubernetes/#block). An agent with no mode configured anywhere gets **Passthrough** on Kubernetes. A hub default of **Passthrough** is denied for Kubernetes profiles, so those agents get the same runtime default. See [Hub-Default GCP Identity](/scion/hosted/ha/permissions/#hub-default-gcp-identity).
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
- **Saving runtimes**: a save sends each runtime entry as edited, with `null` for each field you cleared. The Hub keeps any stored field the page does not show, such as the Kubernetes `node_selector`, `tolerations` and `kubernetes_service_account_mappings`. Changing a runtime's type to or from Cloud Run clears the other type's fields; when any of them hold a value, the page names them and asks you to confirm first. See the [`runtimes` merge rule](/scion/reference/server-config/#admin-api-behavior-notes).
- **Resource values**: CPU, memory and disk fields (Agent Defaults → Resources and each profile) are checked as you type. A memory or disk value needs a unit: `16` is 16 bytes, so write `16Gi`. See [Resource quantities](/scion/reference/server-config/#resource-quantities).
- **Harness Configs Editor**: Features a rich JSON textarea for writing and editing raw harness properties directly, making it easy to tune model parameters or customize env environments.

In highly available (HA) Postgres deployments, all configurations edited through this tab are stored in the database's `hub_settings` table as whole-map JSONB documents. This ensures edits propagate immediately to all replicas, support optimistic locking (CAS), and are never silently dropped or ignored on server PUT actions.

Furthermore, these database-backed settings are wired directly into the runtime broker consumption path. During agent dispatch, both the Scion Hub and the Runtime Broker resolve runtime execution environments and resource profiles dynamically from these persisted database settings. This eliminates the need to distribute or maintain on-disk configuration files (such as local `settings.yaml` files) on individual broker nodes, ensuring a centralized, real-time control plane.

#### Navigation Updates

- **Message Broker Integration**: The Message Broker configuration has been consolidated and moved from the General tab to the **Hub Server** tab, grouping external integrations and network-bound transports in one logical place.
- **Hub Observability Export**: The **Hub Server** tab has one switch each for the Hub's own metrics and trace export (`server.hub.observability.*`). Both apply after a restart. An unset switch shows the state in effect and keeps the earlier rule; **Use default** clears a switch that is set. The section also shows each exporter's state at startup and, read-only, the export project with its source (`env`, `file` or `metadata`). Only a Hub admin in an interactive session can change the switches; a user access token is refused. See [Observability export](/scion/reference/server-config/#observability-export-serverhubobservability).

### Visual Indicators

| Indicator | Meaning |
|-----------|---------|
| 🔒 *Managed via deployment configuration* | Layer-0 field on a hosted Hub — not editable |
| 🔒 *Set via environment variable* | Field pinned by `SCION_SERVER_*` on a workstation Hub |
| ⚠ *Overridden by environment on this node* | A `SCION_SERVER_*` variable sets this field's key on the node that served the page (listed in `env_overrides`), and the Hub runs with that value. The field shows the environment value as **In effect on this node**. When a different value is stored (`saved_differs`), the stored value is labelled as not in effect: *Saved value* for a database setting, *settings.yaml value* for a file setting. For a masked (secret) key, whether the environment value is in effect is not reported, so the field shows only this badge. |
| ⚠ *Environment value superseded by the saved setting* | A `SCION_SERVER_*` variable sets this key on this node, but a saved database value wins over it: on a Layer-1 key the variable only changes the bootstrap seed and fallback. The field shows the environment value on a secondary *not in effect* line. |
| ⚠ *Environment value not in effect on this node* | A `SCION_SERVER_*` variable sets this key, but `scion server start` sets it afterwards, from the workstation defaults or a command-line flag. The page does not receive the value start-up set: the field shows its configured value (from `settings.yaml`, or the default), labelled *Configured value (settings.yaml or default), not the value in effect*, and the environment value on a secondary *not in effect* line. |
| ⚠ Section badge with a key list (`runtimes`, `profiles`) | The same badges for a map section: one badge for the section, and a list of the environment-set keys under it. The values of these keys are always masked, and for a masked key whether it is in effect is not reported (a secret field keeps the plain *Overridden by environment on this node* badge). |
| ⚠ *Some settings are overridden by environment variables on this node* | Banner listing every key in `env_overrides` |
| 🔒 *Set by workstation startup defaults / server flags* | Workstation field that `scion server start` overrides at every start |
| *Tracking deployment configuration* | Seeded section — re-syncs on restart |
| ⓘ *Superseded by database value* | Deployment config differs from the admin-set value |
| Deprecation banner | `SCION_SERVER_*` used for Layer-1 settings |

The environment values come from `env_values` in `GET /api/v1/admin/server-config` (see [Environment Override Warnings](/scion/reference/server-config/#environment-override-warnings)). Secret values are masked there as in the rest of the response, and the page never shows them.

`server.auth.authorized_domains` is a special case on database-backed Hubs: the Hub does not apply a saved value (it runs with the startup value from `settings.yaml` and the environment until [ptone/scion#6000](https://github.com/ptone/scion/issues/6000) is fixed). So the environment value is shown as in effect, and the key is not reported as superseded.

The superseded notice names the layer each deployment value comes from: `SCION_SERVER_*`, `settings.yaml` (or the `--config` location), `SCION_SEED_*`, or *built-in default* for a value that no setting or variable sets. A layer is named when it sets the key, even to the default value, so a `SCION_SEED_*` value equal to the default is shown as `SCION_SEED_*`. The admin API reports these as `source` in `superseded_keys`: `server_env`, `yaml`, `seed_env`, or `default`.

### Error Handling

The admin UI provides structured feedback on save:

| Response | UI Treatment |
|----------|-------------|
| **200** | Success; shows ignored-keys notice if applicable |
| **400** `validation_failed` | Inline per-section validation errors |
| **409** `revision_conflict` | "Settings changed since you loaded this page" banner with Reload button, listing any keys saved before the conflict (`applied_keys`, `pending_restart`, and `skipped`) |
| **409** `legacy_server_yaml` | Shows the message: the server config still comes from a deprecated `server.yaml`; move it under `server:` in `settings.yaml`, remove `server.yaml`, then save again. Nothing was saved |
| **422** `validation_error` | On a database-backed Hub, a duration-valued key, including one nested in a map, does not parse as a duration (for example `"5 minutes"`); `details.field` names the full key path. `""` or `null` clears the key. Also: `default_thinking_level` outside 1-100 (`0` included; `null` clears it), or `{"federation": null}` (`details.keys`). Nothing was saved |
| **422** `layer0_rejected` | Safety-net notice on hosted Hubs (should not occur with layer-aware UI) |
| **422** `startup_only_key_rejected` | On a database-backed Hub, `telemetry.cloud.cloud_logging` was sent with a value different from what `GET` returns (`keys: ["telemetry.cloud.cloud_logging"]`); nothing was saved |
| **422** `unclassified_keys_rejected` / `unpersisted_keys_rejected` / `hub_owned_keys_rejected` / `unsaved_keys_rejected` | Shows the message and the offending keys; nothing was saved |
