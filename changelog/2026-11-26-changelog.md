# Release Notes (2026-11-26)

This day turned on eleven experiments by default, including Artifacts, flat Runtime Brokers and agent plugins, and graduated filter queries and scheduled chat sends. It also added per-agent usage tracking, Hub-level run-limit ceilings, decision retention, artifact review requests and publishing attachments as artifacts. Access constraint management is now behind an experiment.

## ⚠️ BREAKING CHANGES
* **Experiments now on by default** (#5674, #5652): `hub.artifacts`, `hub.flat_runtime_brokers`, `hub.agent_plugins`, `hub.k8s_nfs_home`, `hub.user_delete_force_cascade`, `hub.skill_content_hash_v2`, `hub.content_hash_v2`, `server.secret_rotation`, `web.artifact_review_editor`, `web.artifact_manage_access` and `web.sse_hidden_tab_close` are on unless a Hub turns them off. With `hub.content_hash_v2` on, template and harness-config hashes also cover each file's path and executable bit, so a rename-only or chmod-only commit changes the hash.
* **Access constraint management needs an experiment** (#5596): The access constraints admin API and UI are now gated behind `hub.access_constraints`, which is off by default. Admins who manage access constraints must turn it on.
* **Agents can only change their descendants' message modes** (#5604): A full-role agent's `set_message_mode` now works only on agents it started, directly or through its children, not on itself or peers. Cloning an agent now requires `project.update` on the source.
* **`variable` secret staging removed** (#5600): Runtimes no longer stage `variable` secrets or deliver `secrets.json`, completing the retirement of that type.

## 🚀 Features
* **Graduated: filter queries and scheduled chat sends** (#5554, #5644, #5640): The filter query language is always on across agents, the agent graph, artifacts, files and Decisions, and the old filter controls are gone. Scheduled send in web chat is always available. Version 2 authorization decision audit records are always written.
* **Per-agent usage tracking (experimental)** (#5594, #5606, #5646, #5683, #5664, #5647, #5678, #5635, #5641, #5642, #5670, #5637): Behind `tokenomics.usage_ledger`, the Hub records model usage per agent, with hourly rollups and an Agent Detail Usage panel (`GET /api/v1/agents/{id}/usage`). The `pi`, `grok-build`, `muse-code` and `antigravity` harnesses report live usage, harnesses describe their usage in a `usage.yaml` sidecar, and `sciontool` drains usage to the Hub with health diagnostics.
* **Run-limit ceilings** (#5621, #5655, #5663): Admins can set Hub-level `agent_limits` ceilings, such as maximum turns, from the admin and project UI. They are validated on create, and brokers that can't enforce them are refused.
* **Decision retention (experimental)** (#5591, #5611, #5615, #5680): Behind `hub.decision_retention`, admins can limit how long settled decisions are kept, with per-project overrides, a preview and a manual run.
* **Artifact review requests and attachments as artifacts** (#5624, #5671, #5661, #5662, #5673, #5633, #5636): Behind `hub.artifacts_review_requests`, a publisher can ask specific reviewers to review an artifact, and admins can raise requests. Review drafts show as a review state. A message attachment can be published as an artifact without copying it, from the CLI or web chat. The artifact page keeps review actions in a sticky header.
* **Never-suspend management** (#5623, #5656): The never-suspend policy can be edited on existing agents, filtered on (`suspendPolicy:never`), and capped by a per-project quota.
* **Waking delivers one input** (#5612): A message that wakes a suspended agent arrives together with the resume instruction as a single input, instead of two.
* **Inbox updates (experimental)** (#5576, #5616, #5618, #5648, #5665): Inbox cards summarize state, link their actions inline, and cover agent secret requests. The Chat card counts starred conversations and mentions.
* **Command execution and triggers (experimental)** (#5634, #5654, #5675, #5601, #5613, #5632, #5627): Approved command requests get a web card and confirmation dialog, run through the broker with status reporting, and keep output only for those allowed to see it. Projects get a Triggers page, webhooks support GitHub and Stripe signature modes, and admins can route GitHub repositories to projects.
* **On-demand secrets in the UI and CLI** (#5597, #5598): The web create form and the CLI support the `on_demand` secret type.
* **Phone-friendly terminal** (#5614, #5603): On a phone the terminal viewer uses the whole screen, and the toolbar no longer overflows at small widths. *(Contributor: stevegeek)*

## 🔒 Security
* **Broker host service accounts can't be project identities** (#5650): A Runtime Broker's host GCP service account is refused as a project GCP service account.
* **Hosted explicit workspaces are contained** (#5599): On a hosted broker, an explicit workspace path must sit under a root that comes from broker-global or structural settings, never from project settings.

## 🐛 Fixes
* **Smaller fixes** (#5548, #5628, #5668, #5666, #5653, #5667, #5631, #5676): Duplicate skills, skill versions and user templates get specific 409 messages. A conversation's last activity updates on every message. Message failure is atomic under concurrent binds. Decision responses keep live delivery updates. Harnesses without a skills directory, such as `shell`, skip skill resolution. Credentialed template fetches use the GitHub tarball API. The project Members header stacks properly on mobile. *(Contributor for #5548: GodD001)*
