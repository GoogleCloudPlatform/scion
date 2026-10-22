---
title: Release Notes
---

Scion release notes are published weekly.

## Latest: Week of October 12 -- 18, 2026

The CLI's local-only mode is gone. Every agent command now goes through a Hub, picked from a per-user list of named Hubs and a current context set with `scion use`. Async agent launch became the `hub.async_agent_launch` experiment, with per-step progress, typed failures and launches that survive Runtime Broker restarts. User access tokens now need matching selectors for chat, quota, agent and user-scope operations, and can now administer invitations, membership and schedules, create global skills and manage skill registries. `scion ssh` and `scion vscode` open shells and editors on agents, and the terminal workspace left its experiment.

[Read the full release notes for this week ->](/scion/release-notes/2026-10-12/)

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

## The harness command line uses the Hub-resolved model

The `--model` argument a Runtime Broker passes to the harness now carries the same model as `SCION_MODEL`: the model the Hub resolved (request, then project default, then template, then Hub default), or an explicit `SCION_MODEL` from `--env`. Before, the argument came from the Runtime Broker's own config, so a project or Hub default model could be shown on the agent while codex, gemini-cli or claude ran on the harness-config default. A size alias the Hub could not resolve is now resolved through the harness-config's `model_aliases`, else the harness's built-in alias table, instead of being replaced by the Runtime Broker's own model. `scion create` also gains `--model` and `--thinking-level`, as on `scion start` ([ptone/scion#3914](https://github.com/ptone/scion/issues/3914), [ptone/scion#378](https://github.com/ptone/scion/issues/378)). See [Settings Precedence](/scion/reference/settings-precedence/#b1-harness-configuration-model-thinking-level-and-scalar-limits).

## The web Create Agent form no longer picks a harness config for you

The Create Agent form used to pre-select a harness config (the template's, else the project or Hub default, else `claude`) and always send it, which made it an explicit request value: a project `default_harness_config` could never apply to an agent created from the web, and a template's harness *type* was used as a harness-config name. The form now sends a harness config only when you pick one. Left unset, the Hub applies its own order (project default, then the template's declared `default_harness_config`, then the Hub default), and the picker shows that inherited value as its placeholder ([ptone/scion#3914](https://github.com/ptone/scion/issues/3914)).

## `scion project list` lists projects across hubs

`scion project list` (and `scion projects list`) now lists the projects on all your registered hubs, each with its hub, merged with the project directories on this machine. Use `--hub <hub>` to show one hub and `--refresh` to fetch each logged-in hub's project list. The earlier output, the project directories on this machine with their type, agent count, status and workspace, moved to `scion project list --local`, which keeps that format byte for byte, JSON included. **Scripts that parse `scion project list` should switch to `scion project list --local`.**

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
