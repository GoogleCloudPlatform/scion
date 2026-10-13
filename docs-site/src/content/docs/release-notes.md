---
title: Release Notes
---

Scion release notes are published weekly.

## Latest: Week of October 5 -- 11, 2026

Conduit, still behind its experiment flag, now carries agent terminals, Runtime Broker traffic and user tunnels, including SSH through `scion tunnel`. Agent create, start, stop and delete hold up under races: a create or start that loses to a delete no longer revives the agent, operations are scoped to the run they target, and starts fail closed when credentials or settings cannot be resolved. Access follows membership: removed members' agents are suspended, sharing a Runtime Broker needs its owner's consent, and schedules run under recorded authority. `scion reincarnate` can move agents between Runtime Brokers and change their settings, and the health dashboard was rebuilt around a single attention policy.

[Read the full release notes for this week ->](/scion/release-notes/2026-10-05/)

## Templates describe the job, not the environment

A template now describes the agent's **job** (harness config, model, instructions, skills, tools, limits). Its **environment** (image, resources, volumes, Kubernetes settings, telemetry, hub access) comes only from the harness-config, settings and profiles, the hub and the project. See [Settings Precedence](/scion/reference/settings-precedence/#the-model-a-template-describes-the-job-settings-describe-the-environment) and [Agent Configuration](/scion/reference/agent-config/#key-classes).

**Templates and environment keys** ([ptone/scion#4524](https://github.com/ptone/scion/issues/4524), [ptone/scion#4222](https://github.com/ptone/scion/issues/4222)):

- A template's `telemetry`, `hub`, `image`, `resources`, `kubernetes` and `volumes` are ignored with a warning, and the Hub no longer copies a template's image, env or telemetry into agents. `GET /api/v1/admin/validate-resources` lists the affected templates (`templateConfigFindings`). Move these values to the harness-config or settings.
- Template `env:` variables are now a lowest-precedence fallback: each applies only when no other source sets the key. Previously they beat settings harness-config env. Their values are no longer expanded from the broker host's environment. `SCION_*` keys and the harness-config's auth keys are never taken from a template. Template env is read from the template's current version on every start, so edits take effect on the next start.
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

**Settings are no longer baked into agents** ([ptone/scion#4241](https://github.com/ptone/scion/issues/4241), [ptone/scion#4561](https://github.com/ptone/scion/issues/4561), [ptone/scion#4222](https://github.com/ptone/scion/issues/4222)):

- Broker settings telemetry and the settings- and profile-derived image, image pull policy, resources and volumes are applied at each start, not persisted in `scion-agent.json`. A settings or profile change takes effect on the agent's next start. Hub `agent_defaults.resources` are also applied on every start.
- Agents created before this change keep their already-baked values until they are reprovisioned.
- Agents created before ptone/scion#4222 keep a copy of the Hub default telemetry in their stored config until they are reprovisioned. Starting them no longer resends it.

**Upgrade order:** upgrade the Hub before the Runtime Brokers. A newer Runtime Broker talking to an older Hub receives Hub `agent_defaults.resources` only at create, and, until the Hub is upgraded, gets the live template env only through the template version the agent was created from.

## `scion project list` lists projects across hubs

`scion project list` (and `scion projects list`) now lists the projects on all your registered hubs, each with its hub, merged with the project directories on this machine. Use `--hub <hub>` to show one hub and `--refresh` to fetch each logged-in hub's project list. The earlier output, the project directories on this machine with their type, agent count, status and workspace, moved to `scion project list --local`, which keeps that format byte for byte, JSON included. **Scripts that parse `scion project list` should switch to `scion project list --local`.**

## Timezone handling changes

During the week of September 28 ([release notes](/scion/release-notes/2026-09-28/)), timezone handling changed: the Hub sends every timestamp in UTC with `Z`, the web dashboard has a per-user display timezone and a 24-hour clock, and cron schedules are UTC only. The runtime-profile `timezone` setting introduced the week before ([release notes](/scion/release-notes/2026-09-21/)) has been removed: an agent's `TZ` now comes from its pin, `TZ` environment variables on the Hub, or the Hub default timezone. See [Times and Timezones](/scion/reference/times-and-timezones/) for the full behaviour and the operator steps after upgrading.

## Deleted built-in harness configs and templates stay deleted

A deleted built-in harness config (for example `claude`) or the built-in `default` template no longer comes back when the Hub restarts or is upgraded ([ptone/scion#3544](https://github.com/ptone/scion/issues/3544)). This also applies to harness configs left unselected in the setup wizard. The Hub records each built-in it has seeded in the `builtin_resources_seeded` Hub setting, and startup only creates built-ins that are not in that record. Built-ins that are new in a release are still added, and built-ins you keep still receive content updates.

To bring a deleted built-in harness config back, re-import it as a global harness config from its canonical URL, `https://github.com/GoogleCloudPlatform/scion/harnesses/<name>` (for example `.../harnesses/claude`). Use the import box on the **Harness Configs** tab of the Hub's **Settings** page, or `POST /api/v1/resources/import`. On a hosted Hub, a copy imported from any other URL (such as a `/tree/<ref>/` URL), or a re-imported `default` template, is usable but user-managed: it does not receive the content bundled with later releases. Workstation Hubs update it either way. See [Recovering a deleted built-in](/scion/reference/harness-settings/#recovering-a-deleted-built-in).

Upgrade note: on the first start after upgrading, the Hub has no record yet, so built-ins you deleted since the last restart before the upgrade reappear once. Delete them again and they stay deleted.

## Migration guides

- [Migrating from raw message delivery](/scion/reference/raw-message-removal/): raw keystroke delivery through messages is removed. `scion message --raw` fails before sending anything, and any message request that still carries `raw` is refused with `422 raw_input_removed`. Use `scion keys` or `POST .../keys` instead.
- [Migrating from grove names](/scion/reference/grove-removal/): the grove→project rename.

## Previous Weeks

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
