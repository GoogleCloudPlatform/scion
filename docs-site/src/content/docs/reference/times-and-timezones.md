---
title: Times and Timezones
description: How Scion stores, sends and shows times, and where an agent container's TZ comes from.
---

Scion keeps two kinds of time zone apart:

- **Display zone.** The zone a person sees times in, in the web dashboard or the CLI. It changes only how a time is shown.
- **Agent `TZ`.** The `TZ` environment variable of an agent container. It is infrastructure: it changes what the agent's own clock and tools report.

Neither one changes how times are stored or sent. Stored and transmitted times are always UTC.

## API contract: UTC with `Z`

Every timestamp the Hub returns, over REST, SSE and the Hub↔broker wire, is an RFC 3339 instant in UTC with a `Z` suffix, for example `2026-10-04T17:30:00Z`. This holds regardless of the timezone the Hub host runs in.

- **Inputs** may carry any offset (for example a schedule `fireAt` of `2026-10-05T09:00:00+02:00`). The Hub stores the instant and returns it in UTC.
- **Zones** are never baked into a timestamp. Where a zone matters, it travels as a separate IANA name field, such as the user preference `timezone` or the agent field `explicitTimezone`.
- **Clients** convert to a display zone at the edge.

## Display zone

### Web dashboard

Each user picks a zone with the **Display timezone** card on the profile settings page (see [Web Dashboard](/scion/workstation/dashboard/#display-timezone--clock)).

- **Auto**, the default, follows the browser's zone.
- The value is stored as the `timezone` user preference, an IANA name; empty means Auto. `PATCH /api/v1/users/:id` with an invalid name returns `400` (see the [Users API](/scion/reference/api/)).
- The display zone never changes an agent's `TZ`.

### CLI

The CLI shows times in the local zone of the machine it runs on. Inside an agent container, that is the agent's `TZ`. Two global flags override it:

| Flag | Effect |
| :--- | :--- |
| `--tz <zone>` | Show times in this IANA zone, for example `--tz America/New_York`. |
| `--utc` | Show times in UTC. |

- The two flags are mutually exclusive.
- `--tz Local` and names that are not IANA zones are rejected.
- JSON output (`--format json`) is unchanged by either flag; it always carries UTC `Z` timestamps.
- There is no environment-variable override. The CLI does not read the Hub display preference.

### 24-hour clock

The web dashboard shows clock times in 24-hour form (`00:05`, not `12:05 AM`), with a fixed English format locale. There is no 12-hour or locale setting.

## Agent `TZ` (Hub-dispatched agents)

For an agent dispatched by a Hub, the Hub is the only source of `TZ`. On create, start and restart it sends the first value set in this chain:

| Step | Source | `timezoneSource` |
| :--- | :--- | :--- |
| 1 | The agent's pinned timezone (see [Pin and unpin](#pin-and-unpin)). | `explicit`, or `legacy` for a pin adopted from a `TZ` an older Hub saved in the agent's env |
| 2 | A `TZ` variable in the Hub environment-variable store with injection mode `always`, at user, project, hub or broker scope. The first scope in that order wins. An empty value never wins. | `user`, `project`, `hub`, `broker` |
| 3 | An ancestor agent's user-scope `TZ` variable shared with progeny. | `progeny` |
| 4 | The Hub default timezone, `agent_defaults.default_timezone`. | `hub-default` |
| 5 | Nothing. No `TZ` is sent, and the container uses the image default (UTC). | `none` |

### Hub default timezone

Admins set it in the web dashboard under **Admin → Server Config**, in the **Agent Defaults** card (field **Default Timezone**), or through `PUT /api/v1/admin/server-config` with the top-level field `default_timezone` (stored as the Layer-1 key `agent_defaults.default_timezone`).

- An invalid name, or `Local`, is rejected with `422`.
- Empty means no Hub default.
- In `settings.yaml` it is the top-level `default_timezone` key.

See [Operational settings](/scion/reference/server-config/#layer-1--operational-postgres-hub_settings-table) and [Admin settings](/scion/reference/admin-settings/).

To give every agent on one Runtime Broker a zone, set a broker-scope `TZ` variable on the Hub:

```bash
scion hub env set --broker=<broker> --always TZ Europe/Berlin
```

### What does not set `TZ`

- **Runtime profiles.** There is no profile step. A `TZ` in a profile's `harness_overrides` env is not used (profiles have no `env` key) (see [Removed: profile `timezone`](/scion/reference/orchestrator-settings/#removed-profile-timezone)).
- **Broker-local values.** The Runtime Broker ignores `TZ` from:
  - broker-local templates;
  - the broker settings' harness-config entry `env`;
  - the agent's persisted `scion-agent.json`.

  It logs a warning, and adds a start warning, for each non-empty value it drops that differs from the value the Hub sent. An empty `TZ: ""` marker never passes the broker host's `TZ` through.
- **`config.env` on update.** A `TZ` key in `config.env` on an agent `PATCH` is ignored with the warning `TZ in config.env is ignored; use explicitTimezone`.
- **Secrets.** A secret that targets `TZ` is dropped with a warning.

### Pin and unpin

A pin is the agent's explicit timezone. It is set in three ways:

- **At create.** A `TZ` value in the create request's `config.env`, in a Hub-resolved template, or in the Hub harness config becomes the pin.
- **Later, by API.** `PATCH /api/v1/agents/:id` with `explicitTimezone`:
  - an IANA name pins the agent; an invalid name, or `Local`, returns `400`;
  - `""` unpins it, and the agent follows steps 2-5 from then on.

  The field is accepted in any phase except on a deleted agent (`409`). A running container keeps its `TZ` until the next start, and the response warns `explicitTimezone applies at the agent's next start`. The response also carries `resolvedTimezone` (the `TZ` the next start sends, `""` when none) and `timezoneSource` (the step in the table above). See the [Agents API](/scion/reference/api/).
- **Later, in the web dashboard.** The agent's **Configure** page has a **Timezone** row. It shows the resolved zone and its source, and offers **Pin** and **Unpin**.

An agent created by an older Hub may have a `TZ` saved in its applied config env. The Hub converts that value to a pin (source `legacy`) the first time it reads the agent's timezone. The `applied-config-tz-cleanup` operation does the same for every agent in one pass (see [Operator steps](#operator-steps)).

### Reincarnate

`scion reincarnate` keeps the agent's timezone choice:

- a pin, including a `legacy` pin, is carried to the new agent;
- an unpin is carried too, so a create-time `TZ` does not come back;
- an agent with neither re-derives its pin from its original create inputs and the current template.

## Cron schedules are UTC only

Recurring schedules are evaluated in UTC, and there is no schedule timezone setting.

- An expression that starts with `CRON_TZ=` or `TZ=` is rejected with `400`.
- Schedules saved with such a prefix by an older Hub are paused at Hub start, with one warning per schedule in the Hub log. To resume one, edit its expression to UTC first.

See [Scheduling](/scion/hosted/user/scheduling/#recurring-schedules).

## Local mode

Without a Hub there is no `TZ` chain, no Hub default timezone and no display preference.

- **Agent `TZ`** comes only from the `env` you give the agent:
  - a template's `env`;
  - the harness-config `env` in `settings.yaml`, including a profile's `harness_overrides` entry for that harness config;
  - an inline config passed with `scion create --config`.

  An empty `TZ: ""` in a template or an inline config passes your machine's `TZ` through. A `TZ` in a `settings.yaml` harness-config entry (or a profile's `harness_overrides`) takes precedence over a template or inline-config `TZ`. If it is empty, `TZ` is omitted with a warning, even when a template or inline config sets one, and the container uses the image default (UTC). With no `TZ` in any of them, the container uses the image default (UTC).
- **CLI display** uses your local zone, with `--tz` and `--utc` as above.

A Hub-created agent that is later started with a local `scion start` uses the `TZ` saved in its `scion-agent.json` again.

## Operator steps

Two maintenance migrations relate to times. An admin runs them from **Admin → Maintenance** in the web dashboard, which offers a dry run, or with `POST /api/v1/admin/maintenance/migrations/<key>/run`. Nothing runs them automatically.

| Key | What it does | When to run it |
| :--- | :--- | :--- |
| `utc-timestamp-normalize` | Rewrites stored timestamps to canonical UTC, so that ordering and paging are exact. On SQLite it covers every table time column and the times inside JSON fields; on Postgres, the times inside JSON fields. On SQLite, rows that the database driver cannot read are also repaired automatically, at Hub start, after a database snapshot. Such rows come from a Hub running in a zone that has no letter abbreviation and an offset that is not a whole hour (for example `Asia/Kathmandu`). For such a zone Go prints the four-digit numeric offset in place of the abbreviation, so the stored value ends in `+0545 +0545`. | Once after upgrading, after a backup. On SQLite, the Hub logs at start which tables still need it; Postgres has no startup check, so run it once after upgrading. Safe to re-run, and it can run again after it completes. |
| `applied-config-tz-cleanup` | Converts `TZ` values that older Hubs saved in agents' applied config env into `legacy` pins, in one pass, and reports how many agents it converted. | Optional, because the Hub already converts each agent lazily. Run it **before** `applied-config-env-cleanup` to keep every saved `TZ` as a pin; if the env cleanup runs first, saved values with no live source are removed and those agents follow the chain instead. Safe to re-run, and it can run again after it completes; a second run converts 0. |

The Maintenance page offers **Run** for a pending migration and **Retry** for a failed one, and no button once a migration has completed. To re-run one of these after it has completed, use `POST /api/v1/admin/maintenance/migrations/<key>/run`.

## Related

- [Release notes, week of September 21, 2026](/scion/release-notes/2026-09-21/), and the [timezone handling changes](/scion/release-notes/#timezone-handling-changes) since then
- [Settings Precedence](/scion/reference/settings-precedence/)
