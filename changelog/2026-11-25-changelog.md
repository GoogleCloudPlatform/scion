# Release Notes (2026-11-25)

This day renamed agent message modes, retired the `variable` secret type, and moved the in-agent secret commands from `sciontool` into `scion`, with migrations that raise the downgrade floor. It also locked down project-level settings and pre-start hooks, and added cross-broker move bundles, a never-suspend policy, broker agent policies and on-demand schedule runs.

## ⚠️ BREAKING CHANGES
* **The `variable` secret type is retired** (#5531, #5583): On startup, project- and agent-scope `variable` secrets become `on_demand` secrets, which are never delivered when an agent starts. Leftover `variable` secrets in other scopes (user, Runtime Broker, non-internal Hub keys) and pending ones are **deleted along with their stored values**. Hub-internal keys become `internal`. Before upgrading, check for `variable` secrets in those scopes that you still need.
* **Message modes renamed** (#5523): `lineage` is now `owner` and `branch` is now `direct` (shown as "Direct plus owner" in the web UI), and a new `tree` mode lets agents in the same creation tree message each other. Stored values are rewritten on startup. The old names are rejected everywhere, including templates and `server.hub.default_message_mode`. On an HA Hub, restart one replica after the rollout so rows written by older replicas are rewritten.
* **Downgrade floor raised to `nightly-20261126`** (#5523, #5531): Because of the migrations above, the Hub refuses update targets older than `nightly-20261126`.
* **In-agent secret commands moved to `scion secret`** (#5551): `sciontool secret get`, `list` and `set` are now `scion secret get --value`, `scion secret list` and `scion secret set`. The old commands print the replacement and exit 1.
* **Project pre-start hooks need a dedicated permission** (#5577): Creating, updating, activating or deleting a project pre-start hook requires `project.manage_pre_start_hooks`, which no built-in project role holds, so in practice hub admins manage them. Project members can still list and read hooks.
* **Project settings are deny-by-default** (#5592): A project's `.scion/settings.*` may only set project-scoped keys. Hub-level keys such as `server.*`, `hub_connections`, `quotas`, `agent_secrets`, `broker_policy` and `default_*` are ignored with a warning, as are unknown keys. Setting writes to those keys are refused, and the Hub's workspace file write, upload and delete APIs refuse `.scion` paths, as they already did for `.git`.
* **Hosted Hubs refuse project names that shadow global ones** (#5565): On a hosted Hub, a project template or harness config can't use the name or slug of a global or built-in one (409 `global_name_reserved`).

## 🚀 Features
* **Move agents between brokers with a bundle (experimental)** (#5460, #5589, #5549): Behind `hub.agent_move_bundle`, `scion reincarnate <agent> --broker <target>` can carry the workspace and home directory through Hub object storage, including for Kubernetes and Cloud Run agents on NFS. Leftover staged bundles are swept up.
* **Never-suspend agents (experimental)** (#5517, #5561): Behind `lifecycle.never_suspend`, an agent can be marked never-suspend. Auto-suspend skips it, and a manual suspend is refused unless a human forces it. `scion list` marks such agents.
* **Run a schedule now** (#5535, #5552): `scion schedule run <id>` or **Run now** on the schedule card fires a recurring schedule once without changing its timing or status.
* **Broker agent policy** (#5569): A Runtime Broker's global settings can set `agent_policy` with `allowed_image_prefixes` and `max_resources`, enforced on every agent's final configuration just before its container is created. Projects, Hub settings and environment variables can't loosen it.
* **Hub-wide init firewall** (#5501): `server.hub.init_firewall` sets a Hub-wide egress allowlist that is combined with each broker's own allowlist when an agent turns on `SCION_INIT_FIREWALL`.
* **Approved command execution (experimental)** (#5567): Behind `hub.command_requests` (with `hub.exec_containers`), an agent can propose a command for a named person to approve and run once in that person's execution container.
* **Artifact ownership transfer (experimental)** (#5571, #5581): Behind `hub.artifacts_transfer`, an artifact's steward can transfer an agent-owned artifact to another agent they control in the same project, from the CLI or the web UI.
* **Decisions and Inbox** (#5553, #5573, #5588, #5537, #5538): Chat headers have an Open decisions button. The project and profile Decisions pages accept filter queries. The Inbox is hardened, and its CLI matches the web UI.
* **Web UI additions** (#5566, #5568, #5584, #5570, #5497, #5499): Admins can invite several users at once with project roles and see results per address. Terminal panes scroll by touch, and behind `web.terminal_key_bar` they have a touch key bar. GitHub App settings moved to a new Admin > Integrations tab. Filter queries cover the agent graph.
* **Experimental groundwork** (#5490, #5472, #5482, #5555, #5563, #5572, #5532, #5582, #5484, #5556, #5562): GitHub triggers gained webhook binding and rotation, message templates, an offline retry queue, Hub-token webhook auth and comment commands in the CLI. Execution containers gained launch, ownership and home-reuse protections. Message policies can use System One judgments. Usage-accounting experiments were registered.

## 🔒 Security
* **Changing an agent's image needs attach access** (#5587): Changing an agent's image, by edit or by `reincarnate --image`, now also requires `agent.attach`.
* **File secrets masked in the create form** (#5487): File-type secrets are masked in place on the agent create form's Secrets tab.

## 🐛 Fixes
* **Smaller fixes** (#5557, #5580, #5545, #5550, #5546, #5575): Telemetry applies `SCION_OTEL_HEADERS`. Volume checks and runtime arguments agree on which volumes are mounted. The Decisions list keeps its truncation notice under low project caps. The agent graph keeps dimmed ancestors dimmed, the artifact review comment dialog stays above the mobile keyboard, and message deliveries point agents to the scion-messaging skill.
