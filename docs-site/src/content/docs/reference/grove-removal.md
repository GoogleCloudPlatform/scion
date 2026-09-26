---
title: Migrating from grove names
description: What changed as scion retired the "grove" name in favor of "project", and how to migrate.
---

Scion used to call its per-workspace grouping construct a "grove." That name is being retired in
favor of "project" across the CLI, on-disk layout, hub/broker wire protocol, environment variables,
container labels, and the hub API. This page is the durable, running list of what changed and how
to migrate, organized by area. Each row below is added as the corresponding change merges; a
section with no rows yet means nothing in that area has changed yet.

Pods and containers created by scion releases before this rename, and hub/broker pairs on mixed
versions, are not supported. If you see stale behavior after upgrading, restart the affected
agents and make sure hub and broker are on the same release.

## CLI

| Removed | Replacement / action |
| --- | --- |
| `--project <gcp-id>` on `scion project service-accounts add` and `scion hub secret migrate` | `--gcp-project <gcp-id>`. `--project`/`-g` on these commands now selects the scion project, like everywhere else. |
| `--grove` (global and on `broker provide/withdraw`, `hub env *`, `hub secret *`, `hub token create/list`, `notifications *`) | `--project` / `-g` |
<!-- Rows are appended here as later changes merge. -->

## Hub environment variable

| Removed | Replacement / action |
| --- | --- |
| env `SCION_HUB_GROVE_ID` (now ignored, with a warning) | `SCION_HUB_PROJECT_ID` |

## On-disk

| Removed | Replacement / action |
| --- | --- |
| `.scion/grove-id` file | migrated automatically to `.scion/project-id`. If the file is committed, commit the rename. |
<!-- Rows are appended here as later changes merge. -->

## Wire / env / labels

| Removed | Replacement / action |
| --- | --- |
<!-- Rows are appended here as later changes merge. -->

## Hub API

| Removed | Replacement / action |
| --- | --- |
| broker error code `global_grove_disabled` | `global_project_disabled` |
| REST routes `/api/v1/groves[/…]` | `/api/v1/projects[/…]` |
<!-- Rows are appended here as later changes merge. -->

## Extras / telemetry

| Removed | Replacement / action |
| --- | --- |
| telemetry attributes `scion.grove`, `scion.grove.id`, `scion.grove_id` as identity keys | `scion.project`, `scion.project.id`, `scion.project_id`. The old names are still stripped from user-supplied attributes. |
<!-- Rows are appended here as later changes merge. -->

## Finish

| Removed | Replacement / action |
| --- | --- |
<!-- Rows are appended here as later changes merge. -->

## See also

- [Settings Precedence](/scion/reference/settings-precedence/) — how environment variables like
  `SCION_HUB_PROJECT_ID` rank against settings files.
