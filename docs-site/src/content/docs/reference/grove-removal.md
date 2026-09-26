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
| `scion grove …` | `scion project …` (the `group` alias is unaffected) |
| `scion hub groves …` / `scion hub grove …` | `scion hub projects …` / `scion hub project …` |
| `scion config cd-grove` | `scion config cd-project` |
| `grove:<name>` template-scope prefix and `--template-scope grove` | `project:<name>`; `--template-scope project` |
| `scion config get/set grove_id`, `hub.grove_id`, `hub.groveId` | `scion config get/set project_id`, `hub.project_id`, `hub.projectId` |
| `grove_id` field in `scion project list --format json` | `project_id` |
| `groveId`, `groveName`, `groveType`, `grove`, `groves`, `grovePath` keys in CLI JSON output (including `scion list --format json`) | `projectId`, `name` (`projectName` on broker project entries), `projectType`, `project`, `projects`, `projectPath` |
<!-- Rows are appended here as later changes merge. -->

## Hub environment variable

| Removed | Replacement / action |
| --- | --- |
| env `SCION_HUB_GROVE_ID` (now ignored, with a warning) | `SCION_HUB_PROJECT_ID` |

## On-disk

| Removed | Replacement / action |
| --- | --- |
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
| request fields `groveId`, `grove_id` on project register | `id` |
| request field / query param `groveId` on templates and notifications | `projectId` |
| hub accepted and stored unrecognized `scope` values on template create/clone, notification subscription-template create, and harness-config create/clone (e.g. the removed `grove` scope, or any other unrecognized value) | rejected with 400 (echoing the rejected value); use `global`, `project` or `user` for templates and harness configs, `project` or `agent` for subscription templates |
| harness-config update (`PUT /api/v1/harness-configs/{id}`) accepted `scope`, `scopeId`, `ownerId`, `storagePath`, `storageUri` and `storageBucket` from the request body | update keeps the stored record's scope, scope ID, owner and storage location, matching template updates |
| pre-existing stored `scope='grove'` rows in `templates`, `harness_configs` and `subscription_templates` | normalized to `scope='project'` automatically on hub boot; no action needed |
| `groveId` (and `groveName`/`grove` on project records) in hub API responses for notifications, subscriptions, subscription templates, schedules, scheduled events, access tokens, messages, project providers, project sync state and agent session metrics | `projectId` (`name`/`slug` where applicable). Agent, project and template API responses are not yet updated. |
<!-- Rows are appended here as later changes merge. -->

## Extras / telemetry

| Removed | Replacement / action |
| --- | --- |
| telemetry attributes `scion.grove`, `scion.grove.id`, `scion.grove_id` as identity keys | `scion.project`, `scion.project.id`, `scion.project_id`. The old names are still stripped from user-supplied attributes. |
| a2a-bridge `groves:` config key, `SCION_GROVE_ID`, `/groves/…` routes; fs-watcher `--grove` | `projects:`, `SCION_PROJECT_ID`, `/projects/…`; `--project` |
<!-- Rows are appended here as later changes merge. -->

## Finish

| Removed | Replacement / action |
| --- | --- |
<!-- Rows are appended here as later changes merge. -->

## See also

- [Settings Precedence](/scion/reference/settings-precedence/) — how environment variables like
  `SCION_HUB_PROJECT_ID` rank against settings files.
