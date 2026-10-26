# Release Notes (2026-10-25)

This period hardened agent isolation by mounting repository git metadata read-only, unified project storage into one configuration block, and moved Hub-signed values onto the rotating signing ring. Agent delegation gained revoke-all and visible authorization on messages, and user deletion now explains what blocks it.

## ⚠️ BREAKING CHANGES
* **Repository git metadata mounted read-only into agents** (#4160): agents can no longer change git config, hooks or sibling-worktree state.
  * On bind-mount runtimes, agents see a redacted, read-only copy of `.git/config` with credential keys stripped. Deleting a branch or other packed-ref rewrites now fail cleanly.
  * The Apple runtime refuses starts whose repository config holds credential settings, and refuses git-backed shared-workspace projects.
  * A workspace in a subdirectory of a repository must be a registered git worktree, and unregistered workspaces outside the repository root are refused.
  * A project `.scion` mounted read-only on Docker or Podman must already contain an `agents` directory; create it once with `mkdir <path>/.scion/agents`.
* **One `server.project_storage` block** (#4177): it replaces `workspace_storage`, `shared_dir_storage` and `home_storage`. Old blocks are converted automatically with a warning, but per-profile, per-runtime and per-directory backend overrides are now rejected with a message pointing to `volumes:`. The Hub refuses shared-mode dispatch to a Runtime Broker whose storage layout disagrees, and refuses to start if two NFS blocks name different shares or subpath roots.
* **User delete reports blockers** (#4157): deleting a user with blockers returns `409 user_delete_blocked` with `details.agents` (now carrying `deletionState`) and a new `details.edges` list of active delegations. A new read-only `GET /api/v1/users/{id}/delete-blockers` endpoint and the admin delete dialog list the blockers. A successful delete deactivates the user's remaining delegator edges.
* **`server.hub.gcp_project_id` is settings-file only** (#4190): the setting now takes effect at startup from `settings.yaml`. The server-config API refuses it and GET hides it. Database-backed saves that change `telemetry.cloud.cloud_logging` now fail with 422.

## 🚀 Features
* **Agent delegation: revoke-all and visible authorization** (#4182, #4185):
  * Revoke-all endpoints: `POST /api/v1/users/me/delegations/revoke-all` and `POST /api/v1/agents/{agentId}/delegations/revoke-all`. Revocations run in one transaction and report grants the caller cannot revoke as skipped.
  * The CLI and web show "authorized by <user> via delegation <grant>" on messages sent with a delegated credential.
* **Project visitors (experimental)** (#4189): behind `federation.project_visitors`, project settings gain a Visitors tab to list, add, extend and revoke visitor grants and domain rules. A new per-issuer `allow_visitor_grants` switch replaces the Default Role selector; the old key is accepted and ignored.
* **Markdown artifact review editor (experimental)** (#4187): behind `web.artifact_review_editor`, Review renders the document with inline review marks, records edits as CriticMarkup suggestions, and supports comments.
* **User provisioning with access tokens** (#4178): `POST /api/v1/users` now accepts user access tokens with `user:invite` (and `user:read` for the detailed replay view). Each request is checked against the holder's live authority and audited as `user_provision`. Dev authentication refuses every caller.

## 🔒 Security
* **Signed values use the signing ring** (#4139): capability URLs, artifact view links, list cursors and attachment deliveries are signed with the active signing-ring generation, while values signed with older configured generations still verify. An unusable ring refuses instead of falling back.
* **Hub reads collected resource files through their root** (#4176): template and harness-config bootstrap and import check each file's size and hash, the same way CLI uploads do.

## 🐛 Fixes
* **Agent status and launch records** (#4191):
  * A heartbeat read before a reincarnation can no longer store the previous run's phase or exit details.
  * A synchronous start clears the error left by an ended launch.
  * Reincarnation keeps the start answer's template, runtime and container fields.
* **Atomic reservation transfer on forward moves** (#4181): a refused or failed move leaves the source Runtime Broker's reservation untouched.
* **Settings and telemetry** (#4190, #4188):
  * `telemetry.cloud.cloud_logging` now reaches the agent config.
  * `gen_ai.api.calls` uses the same `scion_*` identity labels as `scion.usage.tokens`.
  * Several Hub operational-settings edge cases are fixed: revision ordering and resync, a stale thinking level reads as unset, and sections the writing Hub could not apply are reported.

## 🔧 CI & Infrastructure
* **cr-ci runner hardening** (#4070): idle JIT runners exit after `RUNNER_IDLE_TIMEOUT` (10m by default) so handed-off jobs keep enough time. The runner image gets patched lint, and reconcile is hardened.
