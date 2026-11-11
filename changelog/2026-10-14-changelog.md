# Release Notes (2026-10-14)

The CLI now reads its Hub configuration only from the Hub list, and the terminal workspace graduated out of its experiment. Async launches survive Runtime Broker restarts and unknown send outcomes. Agents gain a terminal `halted` phase for runs the platform ended. A fix keeps built-in profiles that agents still reference when stored settings replace the catalog.

## ⚠️ BREAKING CHANGES
* **The Hub list is the only CLI Hub configuration** (#3654): The CLI imports the settings Hub keys into `context.yaml` once and stops reading them. The global endpoint becomes a Hub entry, and linked projects become link entries. Nothing in the CLI writes `hub.endpoint`, and the retired keys are deprecated in the schema. `scion login` no longer makes a Hub current. Runtime Broker admin commands use the current Hub. Server and Runtime Broker processes read no `context.yaml` apart from a `--hub` nickname.
* **User-scope writes need token selectors** (#3509): A user access token writing to the caller's own scope needs the matching selector on a Hub boundary. For example, `/users/me/templates` and user-scope template delete or reimport need the `template:*` selectors. Clone into the user scope also needs create on the destination.
* **Template env no longer fills auth keys** (#3608): No env key listed in a harness-config's auth metadata is filled from template env, for any auth type. Before, only `required_env` `any_of` keys were excluded.
* **API keys beat gcloud-adc files in auth auto-detect** (#3328): For harnesses whose default auth type is `api-key` (claude, gemini-cli, grok-build, hermes), an API key env credential now wins over a Vertex (gcloud-adc) file credential at agent start and in the Runtime Broker preflight.
* **`SCION_SHARED_WORKSPACE` removed** (#3639): The Runtime Broker no longer sets it and `sciontool init` ignores it. `SCION_WORKSPACE_MODE` and `SCION_WORKSPACE_GIT` are the only inputs.
* **Pinned placement is enforced** (#3645, #3634): Operations on a pinned agent with an inconsistent saved placement answer 409 (`runtime_target_pin_stale`, `runtime_target_mismatch`, `runtime_broker_missing`) instead of acting through another Runtime Broker. A reservation held on one Runtime Broker no longer satisfies a request on another (409 `broker_reservation_scope_conflict`).
* **Cloud Run Instances restart policy** (#3713): The runtime no longer sets the `run.googleapis.com/restart-policy` annotation, which the API rejected, so instance creates work again. Instances now get the platform default restart behavior, under which an agent that exits 0 may not be restarted.
* **Agent stats return 501** (#3559): Agent stats answer 501 `stats_not_implemented` instead of zeros. Runtime Brokers report WebPTY only for runtimes whose web terminal works (Docker, Podman, Apple container, Kubernetes, Cloud Run sandbox).

## 🚀 Features
* **Terminal workspace always on** (#3641): The persistent terminal workspace graduates and the `web.terminal_workspace` experiment is retired. `/agents/{id}/terminal` links redirect to `/terminals/{id}`, and stored overrides for the old flag are pruned automatically.
* **`halted` agent phase** (#3643): Agents whose run the platform ended (preempted, evicted, OOM-killed, or container missing) enter a terminal `halted` phase, with the cause kept in the exit reason. `scion list` shows `halted (<reason>)`. The creator gets a HALTED notification even without a subscription, and lifecycle hooks gain a `halted` trigger. `scion start` resumes a halted agent as a fresh start.
* **Async launch resilience** (#3699, #3633, #3616, #3623, #3689, #3416):
  * Runtime Brokers journal accepted async launches. After a restart they report what a dead process left behind as `broker_restarted` and clean up. On SIGTERM they answer 503 `broker_draining`.
  * A send whose outcome is unknown no longer ends the launch as `not_launched`.
  * Cloud Run sandbox creates can go async. Cloud Run Instances gain the launch hooks but still create synchronously.
  * Discord `/scion thread` uses async create (`async_create`, default true) through the new `hubclient.WaitForLaunch` helper.
* **Agent defaults and skills** (#3650, #3638, #3675, #3697, #3640): Hub `agent_defaults` limits (max turns, model calls, duration) apply on every start and restart, not only at create. An optional skill that fails to install is skipped with a warning. A failed required skill is named as `skill_resolution_failed`. Public `gh://` skills retry without credentials when GitHub rejects the user's credential. Templates imported from private repositories can be reimported with their scope's repository credential.
* **Harness-config storage** (#3669): Harness configs get a single commit path, an optional `CheckDir` pre-upload check, reimport by ID, and compare-and-swap writes that answer 409 (the CLI retries once). The create and PUT API is aligned with templates.
* **Retained agent files** (#3636): Once the soft-delete window passes, the Hub asks the agent's Runtime Broker to remove its retained files before purging the record. Offline Runtime Brokers are retried for up to 30 days.
* **Admin and messaging** (#3680, #3709, #3621): The admin Maintenance page gains a "Re-issue scopes" card with dry run and apply. `scion message` rewrites `@agent:name` mentions to `@name`, so the agent gets the mention. The new project form puts Name first.
* **Test Hubs** (#3651, #3535): Admin-tier test identities with a classified test-admin policy, a hash-chained audit sink and a test super-admin kind. Test-identity web sessions work behind IAP, bound to a configured driver principal.
* **Agent Plugins, experimental** (#3698): Native Agent Plugins install, phase 1, behind `hub.agent_plugins` (default off), with Codex supported end to end.
* **Conduit relay role, experimental** (#3631): `scion server --role relay` runs a policy-free relay, and the Hub routes to it in external-relay mode, behind the conduit experiment for development and test Hubs.
* **Groundwork** (#3660, #3555, #3666, #3700, #3721): Project visitor grants by email behind `federation.project_visitors` (default off). Flat Runtime Broker instances carry target infrastructure settings, plus a profile-to-instance converter not yet wired in. Profile catalog checks for the coming rebuild preflight.

## 🔒 Security
* **Credentialed Hub URLs redacted** (#3706): Hub URLs are shown without user info in CLI errors, transport errors and endpoint lines, and URLs with user info are never saved. `runtime-broker register` and `join` refuse them.
* **Transactional audit and cleanup** (#3690, #3686, #3694, #3468): Group create, member removal and delete write their audit records in the same transaction, and roll back with a 500 if the audit write fails. Agent row removal deactivates delegation edges and service-account assignments in the same transaction. A role-binding replace that moves an admin binding runs the last-admin check.

## 🐛 Fixes
* **Referenced built-in profiles kept** (#3729, #3684): Built-in profiles and runtimes that agents or projects still reference keep resolving when stored settings replace the catalog, with a stored entry still winning. An unresolvable saved profile is memoized and its fallback logged once, instead of a WARN on every request.
* **Unknown create outcomes kept** (#3657): A synchronous create whose dispatch outcome is unknown (Hub deadline, timeout, lost connection) is no longer rolled back while the Runtime Broker may still be starting it.
* **Agent delete keeps other worktrees** (#3678): Agent delete removes only its own worktree entry instead of running a repository-wide `git worktree prune`.
* **SQLite migrations** (#3665, #3628, #3659): Every pooled SQLite connection enforces foreign keys. Rows orphaned while foreign keys were off get their declared ON DELETE action before a migration, so startup no longer aborts. The database is snapshotted before a reconcile changes rows.
* **Agent state** (#3711, #3672, #3637): Clear and compact no longer wipe sticky states and waits on session-start. A failed start is recorded in the host-owned `host-state.json`, so `scion list` no longer shows the agent as starting forever, and the files-owner run ID moves there too.
* **Cloud Run sandbox** (#3687, #3661, #3653): Log retrieval, exec and probes capture output in a file, so a process holding the output can no longer block them past their timeout. Null state entries, late watchers and stale temp files no longer break list and reconcile.
* **Settings and telemetry** (#3662, #3664, #3648, #3676): File-mode telemetry saves keep omitted nested keys. Admin saves validate `telemetry.cloud.tls`. The admin view reports built-in defaults as their own source. The Cloud Logging handler writes special fields top-level once, so Error Reporting sees grouped errors.
* **CLI and web** (#3605, #3692, #3683, #3350, #3556, #3646, #3644): Hub-mode create applies the `--config` branch and sends `--workspace`. A `scion tunnel` listener refusal no longer closes open tunnels. DM attachment uploads from the web work. Chat deep links resolve their project on a cold load. The WebPTY tag reads the right capability field. An unlinked Hub project is detected by status code, and `whoami --full` output is aligned. *(Contributor: Ruslan Semagin)*
* **Utilities** (#3663, #3667, #3668): Failed token file reads are retried. JSONC comment boundaries are preserved. Unrepresentable memory quantities are rejected. *(Contributor: Ravshanbek Nazarov)*
* **Notification dispatch** (#3656): A concurrent retry no longer leaves a delivered notification marked for redelivery.

## 🔧 CI & Infrastructure
* **CI and deploy** (#3632, #3670, #3571): Every package with SQLite-tagged tests runs in a blocking SQLite job, with a coverage check. `deploy.sh` prints the IAP OAuth client ID with the supported gcloud lookup.

## 📖 Docs
* **Guides** (#3629, #3693): Docs for the Hub-always flow after local-mode removal. Cloud Run pages describe async create through the `hub.async_agent_launch` experiment.
