# Release Notes (2026-09-27)

The grove→project rename reached the hub↔broker wire protocol, container labels, and agent environment, so hub and broker must now be upgraded together. Copilot and grok-build telemetry now passes through local redaction, a fresh-Postgres boot failure was fixed, and Kubernetes agents on NFS workspaces can write to `/workspace` again.

## ⚠️ BREAKING CHANGES
* **Hub and broker must be upgraded together** (#2015, #2017): Grove-named fields (`grove`, `groveId`, `grovePath`, `groveSlug`, `groveName`, `groves`) are removed from the hub↔broker HTTP wire types, the heartbeat payload, and the broker websocket control protocol. The `groveId` query-parameter fallbacks are gone, and `/api/v1/workspace/grove-upload` is replaced by `/api/v1/workspace/project-upload`. Mixed hub and broker versions are unsupported.
* **`SCION_GROVE*` env vars no longer injected or read** (#2016): Agent containers, the hub dispatcher, and the runtime broker no longer set or read `SCION_GROVE_ID`, `SCION_GROVE`, or `SCION_GROVE_PATH`. Setting one by hand prints a one-time warning. Use `SCION_PROJECT_ID`, `SCION_PROJECT`, and `SCION_PROJECT_PATH`.
* **`scion.grove*` labels no longer written** (#2012): Containers and pods carry only `scion.project*` labels and annotations. External tooling that filters on the old names must switch. Restart agents created before the rename, because their containers can now match same-named lookups in any project.
* **Copilot and grok-build telemetry env reserved** (#2018): Agents fail to start if the runtime env sets `COPILOT_OTEL_*`, `GROK_TELEMETRY_*`, or `GROK_EXTERNAL_OTEL` to values other than the provisioner's.

## 🚀 Features
* **Workspace recreation on start** (#2026): On runtimes that don't keep a workspace across a stop (such as Kubernetes), start now sends the git clone config, branch, and workspace mode that create sends, so the agent's workspace can be recreated. This is Phase 1, covering the start path only.
* **Image version stamping** (#2025): scion-base builds `sciontool` with a version and git commit, so `sciontool version` reports both, and images carry the OCI `org.opencontainers.image.revision` label. `build-images.sh` warns when a build inherits a scion-base it did not build in the same run.

## 🔒 Security
* **Harness telemetry redaction bypass** (#2018): Copilot and grok-build exported native OTel straight to the cloud endpoint, skipping sciontool redaction and identity stamping. Both now always send to the local sciontool receiver. `SCION_COPILOT_OTEL_ENDPOINT` remains as a documented debugging bypass.
* **Agent name path validation** (#1998): Agent names are validated as single, clean path elements wherever they resolve on-disk agent state. Broker dispatch and scheduled-event lookups address agents by slug.

## 🐛 Fixes
* **Hub failing to boot on fresh Postgres** (#2028): A migration hook updated `runtime_brokers` before the table existed, which aborted the migration transaction and stopped the hub from booting. Each update now runs inside its own savepoint.
* **Unwritable NFS workspaces on Kubernetes** (#2027): With the NFS workspace backend, the workspace-provision init container was never added, so the per-project subPath was owned by root and agents could not write `/workspace`. The init container now runs for both git and non-git agents with minimal capabilities and without following symlinks, and a chown failure stops the start.
* **Agent identity-key uniqueness** (#2019): Agent identity keys are unique per project across rename, every create path, and restore/purge. A backfill migration handles existing agents.
* **Agent operations across the rename** (#2014): The hub and broker compare recorded project paths after resolving symlinks, so agent listing, stop, and delete keep working for paths recorded before the directory rename. The direct reads of the old `groves/` directories are removed.
* **Terminal pane in light mode** (#2011): The terminal toolbar, buttons, and dialogs now use theme tokens instead of a hard-coded dark palette, which made controls look disabled and dialog text unreadable. *(Credit: miller79)*
* **starter-hub IAM bindings** (#2020): Adds `--condition=None` to `gcloud` IAM policy bindings in the starter-hub setup. *(Contributor: G. Hussain Chinoy)*

## 🔧 CI & Infrastructure
* **pkg/hub SQLite test timeout** (#2034): Raises the test timeout from 15 to 25 minutes and the job timeout from 20 to 30, since the suite was hitting its limit.
* **Web test teardown flake** (#2030): Fixes an intermittent Web Tests failure caused by two role-export tests triggering a real navigation during worker teardown.

## 📖 Docs
* **Nightly doc update** (#2013): Nightly update for 2026-09-26.
