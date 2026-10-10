---
title: Hub Upgrade and Binary Rollback
description: Which stored data a newer Hub build rewrites one way on startup, why rolling the binary back afterwards is unsupported, and the startup warning an older build logs.
---

A Hub upgrade is one-way. When a newer Hub build starts on a database (and on
the settings files of its host) for the first time, it rewrites some stored
data to newer forms. Older builds do not write that data back. **Running an
older Hub binary after a newer build has started once (a binary rollback) is
unsupported.** To return to an older build, restore the database and settings
files from a copy taken before the newer build first started.

## What a newer build rewrites one way

| Stored data | What startup does | Effect on an older binary |
| :--- | :--- | :--- |
| Built-in role definitions and their revision markers (`hub_settings` keys `builtin_role.revision.<role>`) | The role reconciler sets each built-in role's permission list to the build's list and records the build's role revision. It never lowers a revision: a role whose stored revision is higher than the build's is left alone. | The older build keeps the newer build's permission lists, because it will not lower them. It logs the warning described below. |
| Project members group marker (group annotation) | A one-time migration rewrites the old key `scion.io/system-project-members-group` to `scion.io/project-members-group`, and records completion in the `hub_settings` key `migration_legacy_project_members_group_marker_v1`. | A build from before the marker change recognises only the old key, so it does not recognise the rewritten groups as project members groups. |
| Settings files (`settings.yaml`, global and per project) | When the file is loaded, a legacy `hub.grove_id` key is rewritten in place to `hub.project_id`. | A build that reads only `hub.grove_id` no longer finds the project link. |
| `.scion` project marker files | When the file is read, legacy `grove-id`, `grove-name` and `grove-slug` keys are rewritten in place to `project-id`, `project-name` and `project-slug`. | A build that reads only the `grove-*` keys no longer finds them. |

Other one-time data migrations record their completion in `hub_settings` keys
whose names start with `migration_`. They are not reverted either.

## The startup warning

At startup, the role reconciler checks each built-in role's stored revision
against the running build's. When one or more stored revisions are higher
(a newer build has started on this database), it leaves those roles
unchanged and logs one warning:

```text
level=WARN msg="Stored built-in role revisions are newer than this Hub build's. A newer Hub build has started on this database; this build does not lower the roles, so it runs with the newer build's permission lists. Running an older Hub binary after a newer build has started (a binary rollback) is unsupported." roles="hub-member (stored revision 5, this build's revision 4)" count=1
```

`roles` names each affected role with its stored revision and the running
build's revision. The Hub still starts. Nothing is logged when every stored
revision is equal to or lower than the build's. If you see this warning, run
the newer build again, or restore the database and settings files from
before the newer build first started.
