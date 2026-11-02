# Release Notes (2026-10-29)

This period tightened what an agent start will accept (repository git config, mounts, secret delivery) and limited user- and Hub-level secrets to interactive logins. Asynchronous agent create and attachment artifacts are now on by default, and an important fix restores starting, resuming and restarting existing agents.

## ⚠️ BREAKING CHANGES
* **Agent start checks repository git config** (#4322, #4327): on Docker, Podman and Apple runtimes, the repository git config and the worktree and submodule layers an agent can reach are checked before start. The start is refused when a layer sets a key outside the allow-list, such as `core.sshCommand`, `core.hooksPath`, `core.editor`, filter drivers, `url.<base>.insteadOf`, or `include`/`includeIf`. The error names the key (never its value) and the config file to fix.
* **User- and Hub-level env and secrets need an interactive login** (#4319): these operations now accept only CLI login, web session or workstation dev auth credentials, and Hub scope also requires an unscoped local platform admin. Scoped access tokens are refused with a `CREDENTIAL_MANAGEMENT` error pointing to `scion hub auth login`. Automation using an access token (for example `SCION_HUB_TOKEN` without a stored login) must switch to an interactive login. Project- and Runtime Broker-scope behavior is unchanged.
* **Runtime Broker refuses mounts that reach its control files** (#4331): agent mounts whose host source reaches the Runtime Broker's credentials, local settings, host identity and state, plugins or co-located Hub database are refused. The check follows symlinks and runs at Runtime Broker start, agent create and start, and container creation. A periodic check stops running agents whose binds fail it.
* **Per-item secret delivery at start** (#4318): start and restart apply the same per-item authorization as runtime reads. A start is refused when a selected secret value cannot be read. Agents whose delegation records predate current provenance keep their launch material under a logged exception.
* **Inline NFS mounts without `pv_name`** (#4342): with `project_storage` NFS and no `shares[0].pv_name`, agent pods mount the share directly instead of getting a per-pod workspace. The Runtime Broker must mount the export writably at `mount_root/<share id>`, or the start is refused. Agent homes on NFS are refused in this mode.
* **Chat history `totalCount` is always 0** (#4307): chat history and interagent pages no longer count the whole conversation. The field stays in responses but is always `0`.

## 🚀 Features
* **Asynchronous agent create on by default** (#4334): `hub.async_agent_launch` is now on by default and can be turned off in Experiments without a restart.
* **Attachment artifacts on by default** (#4314): `hub.attachment_artifacts` defaults on wherever the artifact service is ready, including workstation and single-node Hubs with local blob storage.
* **Port view for multi-file apps (experimental)** (#4348): behind `hub.port_view`, opening an agent's forwarded port sends the browser to a short-lived page link. The app's stylesheets, scripts, fetches and WebSockets all load through that link as the signed-in user.
* **Kubernetes placement pre-upgrade scan** (#4321, #4328): `scion doctor` scans legacy runtimes, profiles and local agents for placement problems, and `scion hub brokers info` shows placement. Profile conversion now splits profiles with differing details into separate instances instead of refusing.
* **Chat interface** (#4305, #4346, #4315): a per-message More actions button is always visible inside each bubble, including on phones. The members sidebar's People and Agents sections fold. New message indexes make agent DMs open quickly on large Hubs.
* **Project storage validation** (#4340, #4320): storage settings are validated in one place with field-level errors. An invalid block is refused by the Hub and makes the Runtime Broker answer 422 `storage_invalid`. A share the Runtime Broker cannot see returns 503 `project_storage_unavailable` and shows as degraded in `/healthz`.

## 🔒 Security
* **Proxy auth mode request checks** (#4326): behind an authenticating proxy (`auth.mode: proxy`), state-changing browser API requests are checked for the expected request properties and content type, with 403 or 415 on mismatch. Non-browser clients are unaffected.
* **Hardened project repository clone and pull** (#4306): the Hub validates clone URLs, refuses local paths and file transport, and offers the project's git token only to its recorded remote.
* **Narrower credential delivery** (#4329, #4325): the Hub sends a Runtime Broker only the project credentials an agent's GitHub skills can use. The deprecated embedded-broker registration returns `secretKey` only when it mints the first secret.
* **Deleted Runtime Brokers lose their credentials** (#4347): deleting a Runtime Broker deletes its HMAC secret and join token in the same transaction. Runtime Broker auth requires the row to exist, and orphaned credentials are removed at startup.
* **Bounded file reads** (#4332, #4312, #4308): harness-config reads before collection stay inside the directory. Shared project tree cleanup removes only the canonical location. Chat messages render through one sanitizing markup boundary.

## 🐛 Fixes
* **Starting existing agents** (#4338): starting, resuming or restarting an existing agent failed because the Hub did not send its env classifications on start and restart. It now sends them over both transports, as create does.
* **Server config saves keep unsent runtime keys** (#4333): runtime entries merge key by key, so a key left out of a save keeps its value. Resource quantities are checked.
* **Smaller fixes**:
  * Agent messages treat a soft-deleted target as unknown and no longer create conversations for it (#4337).
  * Content-addressed skill versions resolve only to published or deprecated versions (#4311).
  * Auto-suspend revokes delegation grants before releasing the claim (#4310).
  * Review editor task lists, strikethrough and theme colors are fixed (experimental) (#4335).
  * API client error body reads are bounded and nil responses guarded (#4250). *(Contributor: Ravshanbek Nazarov)*
