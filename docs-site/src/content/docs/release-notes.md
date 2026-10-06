---
title: Release Notes
---

Scion release notes are published weekly.

## Latest: Week of September 28 -- October 4, 2026

Raw keystroke messaging was replaced by a dedicated agent keys API (`scion keys`), and raw message delivery is now removed. A timezone overhaul pinned servers, storage and APIs to UTC and made the Hub the only source of an agent's `TZ`. Agent delete became asynchronous and failure-aware, and asynchronous create arrived as an opt-in. Kubernetes gained per-agent NFS workspaces, Workload Identity and a new Substrate runtime. Security work continued with user access token boundaries, frozen delegation ceilings and project access granted only through role bindings.

[Read the full release notes for this week ->](/scion/release-notes/2026-09-28/)

## Timezone handling changes

During the week of September 28 ([release notes](/scion/release-notes/2026-09-28/)), timezone handling changed: the Hub sends every timestamp in UTC with `Z`, the web dashboard has a per-user display timezone and a 24-hour clock, and cron schedules are UTC only. The runtime-profile `timezone` setting introduced the week before ([release notes](/scion/release-notes/2026-09-21/)) has been removed: an agent's `TZ` now comes from its pin, `TZ` environment variables on the Hub, or the Hub default timezone. See [Times and Timezones](/scion/reference/times-and-timezones/) for the full behaviour and the operator steps after upgrading.

## Migration guides

- [Migrating from raw message delivery](/scion/reference/raw-message-removal/): raw keystroke delivery through messages is removed. `scion message --raw` fails before sending anything, and any message request that still carries `raw` is refused with `422 raw_input_removed`. Use `scion keys` or `POST .../keys` instead.
- [Migrating from grove names](/scion/reference/grove-removal/): the grove→project rename.

## Previous Weeks

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
