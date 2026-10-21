# Release Notes (2026-10-20)

Schedules whose agent is deleted are now paused with a visible reason, and the Health dashboard surfaces failing and Hub-paused schedules. CLI settings moved to one loader with per-value sources and a locked atomic writer. Web boot and bundle loading got faster again, and async launch reached existing-agent starts, the chat app and the A2A bridge.

## ⚠️ BREAKING CHANGES
* **Stored template config columns dropped** (#3863): An irreversible, idempotent post-migration step drops the stored template config and image columns on Postgres and SQLite, and the legacy no-tiers telemetry path is removed. Back up before upgrading if you might need to roll back.
* **`scion config list` and `get` JSON shapes** (#3905, #3903): Both commands now share one resolver and report each value's source (default, global file, project file, env variable or project-id file), with secrets masked, which changes their JSON output. The legacy CLI settings loader is retired with no deprecation period; legacy-format files are converted per layer on read.

## 🚀 Features
* **Schedule health** (#3877, #3899, #3875): When the agent that a recurring schedule runs under is deleted, the Hub pauses the schedule with status reason `creator_deleted` instead of refusing every fire. The reason appears in the API, CLI and web list, and resume re-records authority. The Health dashboard lists schedules whose last run failed, with run and error counts and an excerpt of the last error, and schedules the Hub paused itself, each limited to schedules the caller may read.
* **Settings commands and safe writes** (#3905, #3852, #3861): `scion config unset` is new and `cli.mode` is settable. Every whole-file `settings.yaml` rewrite, including integration plugin writers and the file-mode server-config PUT, holds a cross-process lock and goes through one atomic writer, so concurrent writers no longer clobber each other.
* **Async launch everywhere** (#3924, #3803, #3801, #3882, #3759, #3763): Starting an existing agent with the async launch fields runs asynchronously, and a stop, delete or newer start cancels it (409 `start_cancelled`). The chat app's `/create` and `/thread` and the A2A bridge's auto-provisioning ask for async create by default and wait for launching agents before sending. Failed dispatches end with the new `dispatch_failed` code, failed reports record their launch step, and a Runtime Broker retries an unanswered terminal report at its next boot.
* **Faster web boot** (#3868, #3834, #3880, #3892, #3870, #3902): Boot requests and the route chunk start in parallel, and the shell emits modulepreload hints and flushes its head early. CodeMirror languages load as separate chunks, halving the core editor chunk. Shared data such as public settings is read once, network pollers pause in hidden tabs, and the unused always-on debug log is gone.
* **Cross-project service accounts** (#3893, #3867): Registering a GCP service account from another project warns, with the `gcloud` grant command, when the Hub cannot read that project. A refused assignment check names the project, the Hub's service account and the missing `roles/iam.securityReviewer` role.
* **User access tokens for roles and constraints** (#3910): Hub-boundary tokens can administer roles, role bindings and access constraints with the matching selectors, within the holder's live authority and delegation limits.
* **`scion reincarnate --force` dry run** (#3897): Phase 1 of moves that a shared NFS export cannot carry. `--force` evaluates recreating the workspace on the target and reports what the move keeps, recreates and loses.
* **Test Hubs** (#3887): `scion server test-infra delegate` creates an offline member-tier workload delegation, so a test runner can issue member test identities without a live human grantor.
* **Conduit, experimental** (#3927, #3876, #3896, #3898, #3895, #3911, #3918, #3916): In external relay mode with enforced target stream deadlines, user streams open only to agents that support stream renewal (409 `agent_upgrade_required` otherwise) and no longer fall back to legacy routes. Relay-held tunnels are re-checked by the relay, Hub-held streams renew over the bridge, and port-forward tunnel requests get the same evaluation. With `hub.conduit` on, a live conduit session counts as Runtime Broker presence for create, lifecycle, purge, move and tunnel checks.
* **Groundwork** (#3775, #3754): Plumbing for async start, resume and restart behind `hub.async_agent_start` (default off). Flat Runtime Broker capacity accounting frees a slot only when no start owns it and adds a flat instance capacity planner.

## 🔒 Security
* **Tighter static serving** (#3873, #3881): The Hub no longer returns directory listings or dot-path files such as `/.vite/manifest.json`, while `/.well-known/` still works. Fingerprinted assets are cached as immutable for a year. Source maps are no longer shipped, embedded or served; CI keeps them as a build artifact.
* **Sanitized failure logs** (#3890, #3860): Error-valued log attributes are written as sanitized strings instead of `{}` on every Cloud Logging, stdout, OTel and agent log path. Every create and start failure record logs a sanitized, bounded cause with the runtime and profile.

## 🐛 Fixes
* **Runtime Broker registration keeps admin-set fields** (#3888): Re-registration no longer overwrites the GCP host service account, project and labels set through the update API, and the update API now emits audit events.
* **Agent state after failures** (#3885, #3879): A reincarnation no longer carries the previous run's exit reason and code onto the new generation, and a failed start shows as `error` instead of `starting` when its Kubernetes pod stays Pending.
* **Kubernetes home file placement** (#3853): Exec dial errors on a node still joining after a scale-up are retried instead of failing the start.
* **Other agents' worktrees** (#3845, #3894): Partial-worktree cleanup removes only its own entry, and the last repository-wide prune helpers are deleted.
* **Token file replacement** (#3900): A transport auth token read during a file replacement keeps the last value instead of briefly returning empty.

## 📖 Docs
* **Weekly release notes** (#3872): A new docs-site page summarizes October 12-18, 2026.
