---
title: Release Notes
---

Scion release notes are published weekly.

## Latest: Week of October 19 -- 25, 2026

Scion now mounts repository git metadata into agents read-only with a private, credential-stripped `.git/config`, mounts project `.scion` directories read-only in shared layouts, and requires clone-per-agent for Kubernetes git projects that would otherwise share one repository across agents. Project storage consolidated into a single `server.project_storage` block and layout authority that backs the Files tab and chat attachments, while WebDAV, project-level `scion sync`, the Hub project copy and GCS workspace sync were retired. Agent-bound delegation added in-memory credential exchange over `~/.scion/delegation.sock`, lifecycle revocation, revoke-all endpoints and visible delegation attribution on messages, alongside `project.chat_post` for project thread posts and expanded User Access Token scopes for users, groups, roles, access boundaries and project creation. Across the web client, CLI and Runtime Brokers, API responses and static assets are compressed, the Agent Edit page replaced Configure, a single model channel feeds the harness `--model` argument, and `scion reincarnate --broker <target> --force` can move agents by recreating their workspace on another Runtime Broker.

[Read the full release notes for this week ->](/scion/release-notes/2026-10-19/)

## Agent mounts that reach Runtime Broker host files are refused

A Runtime Broker now refuses any agent mount whose host source reaches its own credentials, local settings, identity and state files, plugins or co-located Hub database, read-only or not. This includes any source that contains the Scion global directory, such as `~` or `/`. Inside `~/.scion`, `volumes:` and shared directories may only use workspace, worktree and shared-directory subtrees. Harness auth files and `SCION_DEV_BINARIES` may not come from `~/.scion` at all, and relative bind sources are refused. A refused create or start returns `400` with the code `control_path_mount`, and a broker whose worktree base or NFS mount root reaches these files does not start.

The broker also records each container's binds when it creates it and re-checks running agents. Running Docker and Podman agents created before this release are checked against the binds the runtime reports, and keep running if those are clear. **They are stopped if any bind reaches these files, if a bind source does not exist on the host, or if the container runtime is not reached over a local unix socket or that cannot be determined, including a remote daemon selected through a Docker context or a Podman connection. A remote daemon reached through a local unix socket, such as an SSH-forwarded socket or a socat proxy, is treated as local; this affects only agents created before this release. Running Apple container agents created before this release are always stopped once.** Stopped agents are marked `error`, and their workspaces are kept; start them again. See [Volume Source Restrictions](/scion/reference/orchestrator-settings/#volume-source-restrictions).

## Runtime Brokers on shared project storage must see the share

A Runtime Broker whose `server.project_storage` uses a shared backend reads project configuration and prepares project directories through its own view of the share, at `<mount_root>/<shares[0].id>` (nfs) or `/mnt/<volume_name>` (gke-shared-volume, cloudrun-volume). If that directory (with its `<subpath_root>`) is not there, the Runtime Broker refuses to create agents in shared-storage projects (`503 project_storage_unavailable`) and reports it in its health: `/healthz` shows `project_storage: "unhealthy: share_not_visible (…)"` and the status is `degraded` (readiness is unaffected), and the heartbeat to the Hub reports `project_storage: unhealthy`. The refusal refers to the share's mount point as "its configured mount point", never by its path, and says what to do for that Runtime Broker:

- **Runtime Brokers whose default runtime is Kubernetes or Cloud Run** (including a Runtime Broker co-located in a Cloud Run or Kubernetes Hub) never mount the share themselves. Make it available in the Runtime Broker's container at that path. A Cloud Run Hub's NFS volume at `<mount_root>/<shares[0].id>`, or the Helm `scion-hub` chart with `workspaceStorage.nfs.enabled: true`, already does this. A separate Runtime Broker pod needs the volume added.
- **Other Runtime Brokers:** mount it at that path, or set `server.project_storage.nfs.auto_mount: true`. If `auto_mount` is already on and the mount fails, the refusal says so and points at the NFS server and export and the Runtime Broker log.
- **Check:** list `<path>/<subpath_root>` in the Runtime Broker's container; it shows project IDs.

**Changed status codes and texts.** On a Kubernetes runtime, a create, start or restart that fails because the share is not mounted on the Runtime Broker now gets `503 project_storage_unavailable` with that path-free text, instead of `500 runtime_error` with the generic "Failed to create agent". An async launch reports the same code and text, and the Hub passes the Runtime Broker's `503 project_storage_unavailable` through to the client with its code instead of a `502 runtime_error`; the Hub passes the message on only when it is one of these path-free texts, and otherwise (for example from an older Runtime Broker, whose message named the mount path) answers with a fixed text. The same holds when a message, agent DM or chat message sent with wake resumes a suspended agent, which now answers `503 project_storage_unavailable` instead of `502 runtime_error`, and for the warning of a provision-only create and the error recorded for a reincarnation or move, which carry the code and the filtered text. The `503 nfs_unavailable` refusal of an NFS-backed create no longer includes the share ID, the mount point or the server and export, with or without shared project storage; the Runtime Broker log has them.

## Lifecycle hooks re-check their writer before minting a token

An `http` lifecycle hook with an execution identity now mints its service-account token only if the hook's writer (`createdBy`) still qualifies at that moment. The writer must still be an active user created before the hook, must still hold `hub.lifecycle_hooks.update`, and, when the GCP IAM check is enforced, must still be able to act as the service account. The service account must still be verified and in scope. Before, these were checked only when the hook was written. When a check fails, the request is not sent, and the execution audit records `hook writer no longer authorized` with the failed check, at most once every 10 minutes per hook and cause. **Existing hooks stop authenticating** if their writer was deleted, suspended or demoted, if the writer's email changed, or, with the IAM check enforced, if the writer lost `actAs`. Updating a hook does not change its writer. To restore such a hook, an admin who holds the authority today re-creates it. On a Hub that uses development authentication, every `http` hook fails if the development user's stored email differs from the configured development identity's email. Re-creating does not help there: make the configured email match the stored one, or update the stored email. `webhook` actions are unchanged. See [Lifecycle Hooks](/scion/hosted/ha/lifecycle-hooks/#the-writers-authority-is-re-checked-every-time-a-token-is-minted).

## Templates describe the job, not the environment

A template now describes the agent's **job** (harness config, model, instructions, skills, tools, limits). Its **environment** (image, resources, volumes, Kubernetes settings, telemetry, hub access) comes only from the harness-config, settings and profiles, the hub and the project. See [Settings Precedence](/scion/reference/settings-precedence/#the-model-a-template-describes-the-job-settings-describe-the-environment) and [Agent Configuration](/scion/reference/agent-config/#key-classes).

**Templates and environment keys** ([ptone/scion#4524](https://github.com/ptone/scion/issues/4524), [ptone/scion#4222](https://github.com/ptone/scion/issues/4222)):

- A template's `telemetry`, `hub`, `image`, `resources`, `kubernetes` and `volumes` are ignored with a warning, and the Hub no longer copies a template's image, env or telemetry into agents. `GET /api/v1/admin/validate-resources` lists the affected templates (`templateConfigFindings`). Move these values to the harness-config or settings.
- Template `env:` variables are now a lowest-precedence fallback: each applies only when no other source sets the key. Previously they beat settings harness-config env. Their values are no longer expanded from the broker host's environment. `SCION_*` keys and every env key the harness-config's auth metadata lists are never taken from a template ([ptone/scion#4968](https://github.com/ptone/scion/issues/4968)). Template env is read from the template's current version on every start, so edits take effect on the next start.
- Existing agents: a template env key baked into an agent's stored config with the same value as the template is treated as the fallback from the agent's next start.

**Precedence** ([ptone/scion#4222](https://github.com/ptone/scion/issues/4222), [ptone/scion#4223](https://github.com/ptone/scion/issues/4223), [ptone/scion#4218](https://github.com/ptone/scion/issues/4218), [ptone/scion#4217](https://github.com/ptone/scion/issues/4217)):

- The Hub default model and thinking level apply only when neither the template nor the project sets one. The template beats the Hub default; the project still beats the template.
- Requester telemetry is on/off only. Other inline telemetry fields are ignored with a warning, on every Runtime Broker.
- Project telemetry policy beats `--enable-telemetry` / `--disable-telemetry` and `SCION_TELEMETRY_ENABLED`.
- When a template sets both `harness_config` and `default_harness_config`, `default_harness_config` wins.
- On the Hub, an explicit `harness:` key in a template wins over inferring the harness from the template's name.

**Template API and storage** ([ptone/scion#4223](https://github.com/ptone/scion/issues/4223), [ptone/scion#4222](https://github.com/ptone/scion/issues/4222), [ptone/scion#4221](https://github.com/ptone/scion/issues/4221), [ptone/scion#4217](https://github.com/ptone/scion/issues/4217)):

- The template API rejects `config` (`400 template_config_in_file`) and `image` (`400 template_image_not_supported`) on create and update, and `PUT` updates metadata only. Edit `scion-agent.yaml` instead.
- `GET` on a template returns a new read-only `agentConfig`, the parsed `scion-agent.yaml`.
- Concurrent pushes of the same template: one wins, the other gets `409 template_conflict`, and the CLI retries once.
- Each template's storage moves to a content-addressed layout, once, on its next commit. Unreferenced blobs are garbage-collected after a grace period (24 hours by default, never less than 1 hour).
- Cloning a template whose bundled harness-config has an unusable provisioner fails with `422`.
- The template row's stored `config` and `image` columns are removed. Their values have been ignored since the previous release and listed by the validate report; they are now discarded. The validate report's `stored_config` kind is gone with them ([ptone/scion#4224](https://github.com/ptone/scion/issues/4224)).

**Settings are no longer baked into agents** ([ptone/scion#4241](https://github.com/ptone/scion/issues/4241), [ptone/scion#4561](https://github.com/ptone/scion/issues/4561), [ptone/scion#4222](https://github.com/ptone/scion/issues/4222)):

- Broker settings telemetry and the settings- and profile-derived image, image pull policy, resources and volumes are applied at each start, not persisted in `scion-agent.json`. A settings or profile change takes effect on the agent's next start. Hub `agent_defaults.resources` and the Hub default limits are also applied on every start ([ptone/scion#623](https://github.com/ptone/scion/issues/623)).
- Agents created before this change keep their already-baked values until they are reprovisioned.
- Agents created before ptone/scion#4222 keep a copy of the Hub default telemetry in their stored config until they are reprovisioned. Starting them no longer resends it.
- Agents without recorded create inputs that are reincarnated keep any old template-derived env keys as explicit env, with a warning ([ptone/scion#4224](https://github.com/ptone/scion/issues/4224)).

**Minimum Runtime Broker version and upgrade order** ([ptone/scion#4224](https://github.com/ptone/scion/issues/4224)): the Hub no longer sends telemetry in the old inline form. Runtime Brokers older than the release that introduced the `templateTiers` capability (TS-2a, [ptone/scion#4218](https://github.com/ptone/scion/issues/4218)) keep working but receive no Hub telemetry default and no project telemetry policy; the Hub logs a warning once per such Runtime Broker. **Upgrade all Runtime Brokers to at least that release before upgrading the Hub to this one.** This replaces the earlier advice to upgrade the Hub first. While the Hub is still older, an upgraded Runtime Broker receives Hub `agent_defaults.resources` only at create. It also gets no live template env: the older Hub sends no template identity on start, so the Runtime Broker uses the template version the agent was provisioned from if that version is still in its cache, and otherwise applies no template env, with a start warning.

## `scion templates sync` and `push` accept a directory path

`scion templates sync` and `scion templates push` now also take a template directory path: `.` or `..`, an absolute path, or one that contains a path separator (such as `./name`). The directory is synced as is, and the Hub name defaults to its base name (`--name` overrides it). Agents that share the project workspace see the project's `.scion` directory read-only, so they now edit a template elsewhere in their checkout and sync it by path, for example `scion templates sync ./templates/planner --name planner`. **An argument containing a `/`, and `.` or `..`, is now read relative to the current directory**: before, `sync a/b` looked up `<templates dir>/a/b`, and `sync .` resolved to the templates directory itself. Bare names are looked up as before. See [Templates & Roles](/scion/local/templates/#hub-commands).

## The harness command line uses the Hub-resolved model

The `--model` argument a Runtime Broker passes to the harness now carries the same model as `SCION_MODEL`: the model the Hub resolved (request, then project default, then template, then Hub default), or an explicit `SCION_MODEL` from `--env`. Before, the argument came from the Runtime Broker's own config, so a project or Hub default model could be shown on the agent while codex, gemini-cli or claude ran on the harness-config default. A size alias the Hub could not resolve is now resolved through the harness-config's `model_aliases`, else the harness's built-in alias table, instead of being replaced by the Runtime Broker's own model. `scion create` also gains `--model` and `--thinking-level`, as on `scion start` ([ptone/scion#3914](https://github.com/ptone/scion/issues/3914), [ptone/scion#378](https://github.com/ptone/scion/issues/378)). See [Settings Precedence](/scion/reference/settings-precedence/#b1-harness-configuration-model-thinking-level-and-scalar-limits).

## The web Create Agent form no longer picks a harness config for you

The Create Agent form used to pre-select a harness config (the template's, else the project or Hub default, else `claude`) and always send it, which made it an explicit request value: a project `default_harness_config` could never apply to an agent created from the web, and a template's harness *type* was used as a harness-config name. The form now sends a harness config only when you pick one. Left unset, the Hub applies its own order (project default, then the template's declared `default_harness_config`, then the Hub default), and the picker shows that inherited value as its placeholder ([ptone/scion#3914](https://github.com/ptone/scion/issues/3914)).

## `scion project list` lists projects across hubs

`scion project list` (and `scion projects list`) now lists the projects on all your registered hubs, each with its hub, merged with the project directories on this machine. Use `--hub <hub>` to show one hub and `--refresh` to fetch each logged-in hub's project list. The earlier output, the project directories on this machine with their type, agent count, status and workspace, moved to `scion project list --local`, which keeps that format byte for byte, JSON included. **Scripts that parse `scion project list` should switch to `scion project list --local`.**

## Posting in project chat threads needs `project.chat_post`

Reading a project's chat threads still needs `project.read`. Posting in them, and promoting a direct message with one of the project's agents into a thread, now also needs the new `project.chat_post` permission on the project. The built-in `project-owner`, `project-admin` and `project-member` roles and the `hub-admin` and `super-admin` system roles get it automatically on upgrade, so their holders keep posting. A send without it answers `403 chat_post_denied`, and the web thread shows a read-only notice instead of the message box ([ptone/scion#5518](https://github.com/ptone/scion/issues/5518)). See [Chat posting](/scion/hosted/ha/permissions/#chat-posting). These chat writes do not check `project.chat_post` yet: thread create; thread rename, default-agent change and delete; project attachment upload; message edit and delete; and thread posts sent through the agent message endpoint (`POST /api/v1/agents/{id}/message` with a thread key). They keep their existing checks, and are tracked in [ptone/scion#5519](https://github.com/ptone/scion/issues/5519).

A new token scope, `project:chat_post`, keeps that permission on a user access token so the token can grant roles that include it. A token that grants `project-member`, or any other role that holds `project.chat_post`, through `project:manage` now also needs `project:chat_post`; mint a new token for existing membership automation. Posting by token is unchanged: `inbox:write` on the token and `project.chat_post` on your account.

Upgrade note: **custom project roles are not changed.** A principal whose only access to a project is a custom role that holds `project.read` but not `project.chat_post` can no longer post in that project's threads, and their scheduled chat messages fail with `no_access` when they come due. This is intended: a role that only reads should not post. If such a role should keep posting, an administrator must add `project.chat_post` to it (Role & Binding Management).

## Timezone handling changes

During the week of September 28 ([release notes](/scion/release-notes/2026-09-28/)), timezone handling changed: the Hub sends every timestamp in UTC with `Z`, the web dashboard has a per-user display timezone and a 24-hour clock, and cron schedules are UTC only. The runtime-profile `timezone` setting introduced the week before ([release notes](/scion/release-notes/2026-09-21/)) has been removed: an agent's `TZ` now comes from its pin, `TZ` environment variables on the Hub, or the Hub default timezone. See [Times and Timezones](/scion/reference/times-and-timezones/) for the full behaviour and the operator steps after upgrading. An upgraded SQLite Hub now runs `utc-timestamp-normalize` once at start, after a database snapshot, so no manual step is needed; Admin → Maintenance shows the run as started by `system:hub-start` ([ptone/scion#5194](https://github.com/ptone/scion/issues/5194)).

## Deleted built-in harness configs and templates stay deleted

A deleted built-in harness config (for example `claude`) or the built-in `default` template no longer comes back when the Hub restarts or is upgraded ([ptone/scion#3544](https://github.com/ptone/scion/issues/3544)). This also applies to harness configs left unselected in the setup wizard. The Hub records each built-in it has seeded in the `builtin_resources_seeded` Hub setting, and startup only creates built-ins that are not in that record. Built-ins that are new in a release are still added, and built-ins you keep still receive content updates.

To bring a deleted built-in harness config back, re-import it as a global harness config from its canonical URL, `https://github.com/GoogleCloudPlatform/scion/harnesses/<name>` (for example `.../harnesses/claude`). Use the import box on the **Harness Configs** tab of the Hub's **Settings** page, or `POST /api/v1/resources/import`. On a hosted Hub, a copy imported from any other URL (such as a `/tree/<ref>/` URL), or a re-imported `default` template, is usable but user-managed: it does not receive the content bundled with later releases. Workstation Hubs update it either way. See [Recovering a deleted built-in](/scion/reference/harness-settings/#recovering-a-deleted-built-in).

Upgrade note: on the first start after upgrading, the Hub has no record yet, so built-ins you deleted since the last restart before the upgrade reappear once. Delete them again and they stay deleted.

## Migration guides

- [Local-only mode removal](/scion/reference/local-mode-removal/): every agent command now goes through a Hub; after `scion server start`, each repository's existing agents are imported on its first command.
- [Migrating from raw message delivery](/scion/reference/raw-message-removal/): raw keystroke delivery through messages is removed. `scion message --raw` fails before sending anything, and any message request that still carries `raw` is refused with `422 raw_input_removed`. Use `scion keys` or `POST .../keys` instead.
- [Migrating from grove names](/scion/reference/grove-removal/): the grove→project rename.

## Previous Weeks

- [Week of October 12 -- 18, 2026](/scion/release-notes/2026-10-12/)
- [Week of October 5 -- 11, 2026](/scion/release-notes/2026-10-05/)
- [Week of September 28 -- October 4, 2026](/scion/release-notes/2026-09-28/)
- [Week of September 21 -- 27, 2026](/scion/release-notes/2026-09-21/)
- [Week of September 14 -- 20, 2026](/scion/release-notes/2026-09-14/)
- [Week of September 7 -- 13, 2026](/scion/release-notes/2026-09-07/)
- [Week of August 31 -- September 6, 2026](/scion/release-notes/2026-08-31/)
- [Week of August 24 -- 30, 2026](/scion/release-notes/2026-08-24/)
- [Week of August 17 -- 23, 2026](/scion/release-notes/2026-08-17/)
- [Week of August 10 -- 16, 2026](/scion/release-notes/2026-08-10/)
- [Week of August 3 -- 9, 2026](/scion/release-notes/2026-08-03/)
- [Week of July 27 -- August 2, 2026](/scion/release-notes/2026-07-27/)
- [Week of July 19--25, 2026](/scion/release-notes/2026-07-19/)
- [Week of July 12--19, 2026](/scion/release-notes/2026-07-12/)

---

Looking for older release notes? See the [Prior Release Notes Archive](/scion/release-notes-archive/) for daily entries from the early development period (Feb--Jul 2026).
