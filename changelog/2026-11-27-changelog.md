# Release Notes (2026-11-27)

This day turned Conduit on by default outside hosted HA, added a usage dashboard and `scion usage` CLI on top of the usage ledger, and let humans curate agent labels with `scion label`. It also refused inactive harness configs, bounded API request bodies, and kept extending artifact reviews, command requests and the Inbox.

## ⚠️ BREAKING CHANGES
* **Conduit is on by default** (#5742): `hub.conduit` now defaults to on for workstation and single-node Hubs, so agents connect to the Hub over a Conduit session. Hosted HA deployments keep it off until their readiness checks pass, and log the unmet prerequisite. Turn the experiment off explicitly to keep the old behavior.
* **Inactive harness configs are refused by name** (#5790): Creating or reincarnating an agent with a harness config whose first match is archived or pending now fails with 400 naming the config. It no longer falls through to a config in a lower scope or a broker-local fallback.

## 🚀 Features
* **Usage dashboard and `scion usage` (experimental)** (#5716, #5692, #5694, #5696, #5691, #5707): Under `tokenomics.usage_ledger`, the web UI has a usage dashboard with cache hit rates, and `scion usage` reports token and API call usage for a project, a user, an agent or the whole Hub, with a measurement status. Older session metrics are backfilled into the ledger, and `telemetry.retention` bounds how long usage data is kept.
* **Curate agent labels** (#5701): `scion label <agent> key=value ... [--remove <key>]` adds, updates or removes labels on an existing agent, merging concurrent changes on the server. Only humans can change labels, and every change is audited.
* **Upload limits** (#5726): A new `uploads` setting sets per-file limits for chat attachments and workspace and shared-directory uploads, applied live. The web UI checks files before sending.
* **Hard run-duration backstop (experimental)** (#5686): Behind `agent_policy.duration_backstop`, the Runtime Broker stops an agent that runs past its `max_duration` plus a grace period, using native deadlines on Kubernetes and a broker sweep elsewhere.
* **Enforced telemetry (experimental)** (#5688, #5687): Behind `agent_policy.telemetry_enforcement`, admins can require telemetry for every agent. Creates that can't meet it, and brokers too old to honor it, are refused.
* **Never-suspend in the web UI** (#5639, #5781): Agents can be set to never-suspend from the web UI, and agent pages show when activity started and when an idle suspend is due.
* **Artifact review updates** (#5703, #5689, #5760, #5761, #5764, #5714, #5728): Agents can request reviews with time-limited review grants, which are removed when a review is dismissed or expires. Draft reviews can be abandoned. The Inbox shows artifact reviews, and decision prompts can reference artifacts.
* **Command requests and the Inbox (experimental)** (#5709, #5699, #5754, #5788, #5806, #5684): Command requests have a complete lifecycle, and their card is simpler, with one confirmation. The Inbox gains Agent Executions and Agent Secrets pages and cards.
* **Chat and CLI** (#5682, #5729, #5744, #5745, #5752, #5774): Agent pages list shared conversations. `scion conversation` can show and download attachments. The chat rail has an Unread/Mentions dropdown, agent lists have a column picker, and the agent graph zooms more smoothly on trackpads.
* **Other experimental work** (#5693, #5796, #5727, #5763, #5799, #5777, #5697, #5708, #5786, #5695, #5713, #5736, #5780, #5723, #5782): Triggers can run several actions in order and enforce a baseline authoring floor. Message policies gate messages to agents and get a read-only web page. The mandatory agent preamble can list key platform skills. Brokers choose which runtimes deliver messages over Conduit. Conversations always carry a kind and surface, with a Slack integration slice. Raw trajectory capture, raw-token budgets and a unified GitHub fetcher are in progress.

## 🔒 Security
* **Bounded request bodies** (#5740, #5724, #5776): Agent create (up to 16 MiB, with a clear 413 message), env, exec, message and agent edit requests have size limits.
* **Other hardening** (#5785, #5718, #5789, #5735): Agent file removal is confined to the agent's directory. The Antigravity usage decoder caps row sizes. Embedded broker registration clears leftover credentials. The grok-build CLI is pinned, and unverified `agy` and `muse` versions log a warning.

## 🐛 Fixes
* **Wake and suspend** (#5690, #5651, #5758, #5743): A direct message that races an automatic suspend now wakes the agent. Group posts aimed at people no longer wake the default agent. The idle-suspend delivery guard is bounded by the idle threshold.
* **Smaller fixes** (#5705, #5704, #5755, #5797, #5737, #5772, #5731, #5747, #5787, #5712, #5700, #5733): Container lookups prefer an exact ID. Measured zero token counts are kept. The `antigravity` harness passes its default model on the command line, and the `generic` harness skips `--model`. Large artifact uploads get longer deadlines. Stale broker versions are cleared. The iOS home-screen app keeps its viewport height. Gated commands in agent mode say they're not available there.
