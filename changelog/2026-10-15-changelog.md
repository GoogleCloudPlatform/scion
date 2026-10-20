# Release Notes (2026-10-15)

Agent Plugins Phase 1 landed for the copilot, muse-code and claude harnesses behind the `hub.agent_plugins` experiment. Plugin support moved into a sidecar file so that older Hubs and CLIs can install harness configs again. The last local-mode code paths were removed. Artifacts gained stewards for agent-owned artifacts and a dedicated decision log. Cloud Run agents no longer restart every few minutes.

## ⚠️ BREAKING CHANGES
* **Local-mode code removed** (#3746): The local branches of create, delete, stop, suspend, attach, logs, look, keys, broadcast, message, list and harness-config install are gone. `scion logs --follow` is dropped, and `scion sync <agent>` now requires a direction. Without a Hub, the no-hub error now comes before any image-registry check.
* **Runtime Broker IDs must be a single path element** (#3771): A Runtime Broker ID that is empty, `.`, or contains `..`, a path separator or NUL is refused on credential save, join and every `hub.brokerId` write, and the server refuses to start with one.
* **Blocking messages index migration** (#3725): A new `(project_id, created, id)` index on messages speeds up project message pages (a backfill test went from 145 s to 24 s). On Postgres the index is built with a plain `CREATE INDEX`, which blocks writes to messages until the startup migration commits. During a rolling deploy, writes from the other Hub instances wait too.

## 🚀 Features
* **Agent Plugins, experimental** (#3735, #3726, #3730, #3780): Behind `hub.agent_plugins`, the copilot and muse-code harnesses install plugins natively from Scion's bundle mirror, and claude installs them into `~/.claude/skills/` with validated MCP servers. Plugin support now lives in a `harnesses/<h>/agent-plugins.yaml` sidecar, so Hubs, Runtime Brokers and CLIs that predate the feature accept harness-config installs from main again. A malformed sidecar downgrades to "unsupported" with a warning.
* **Artifact stewards and decision log** (#3776, #3743, #3740, #3777): On agent-owned artifacts, owners of the home project and the agent's delegating user can act as stewards. Stewards can give view and edit grants, create share links, change expiry and delete, but cannot give admin grants or move the artifact. A dedicated `scion-artifacts` Cloud Logging log records every publish, grant, link, move, expiry and delete decision, with reason codes for refusals. Review grants return distinct 403 codes to callers who hold review-grant authority.
* **Web-chat attachments as artifacts, experimental** (#3658): Behind `hub.attachment_artifacts` (default off), files uploaded in web chat become message-scoped artifacts. Anyone who can read the message can read them, and they are retired with the message.
* **Mentions in web and chat sends** (#3716): The Hub rewrites recipient-form mentions such as `@agent:hub-ops` to `@hub-ops` in received message bodies, the same rewrite the CLI applies, so the mention reaches the named agent.
* **Antigravity model sizes** (#3761): `large` and `extra-large` now map to Gemini 4 Argon at Medium and High effort, replacing Gemini 3.1 Pro (Low).
* **inotify limit warnings** (#3712): Linux Docker and Podman Runtime Brokers warn when the per-user inotify limits are too low for the Hub's default capacity of 100 agents. `scion doctor` gains an inotify-limits check, and a new docs section explains the sysctl fix.
* **Admin test identities on test Hubs** (#3758, #3722, #3749, #3717): On test-designated Hubs, `scion hub test-identity issue --admin-role` issues admin and super-admin test identities. Their grants are stamped to expire with the issuance, and purge and residue reporting cover them. Workload issuer delegation and admin web sessions require startup-only configuration.
* **Groundwork** (#3738, #3737, #3736, #3760, #3710): A Kubernetes placement-defaults library (node selector, tolerations, affinity, runtime class) that merges global, runtime-entry and project levels. Store support for async start and restart launches. Row-unique blob storage with staged uploads for harness configs. GCP passthrough mode now follows the runtime profile the agent resolves to, and the Hub records that profile.

## 🔒 Security
* **Attachment and skill error hardening** (#3767, #3756, #3748): The legacy chat attachment download path applies the message read rule to user access tokens. Skill resolve errors return a fixed reason phrase with a correlation ID, and the full error goes only to server logs. Declared skill references are logged in redacted form.

## 🐛 Fixes
* **Cloud Run agents stay up** (#3731, #3742, #3312): Cloud Run instances are no longer killed and restarted about every 4 minutes. A `sciontool` startup listener answers the platform's startup probe. An accepted Instances delete that outlasts the cleanup budget counts as done. The Runtime Broker image pull now covers the grok-build and muse-code harnesses.
* **Launch cleanup retries** (#3741): A launch whose cleanup fails keeps its journal record as `cleanup_pending`, and the next Runtime Broker start retries only the failed scope. Retries stop after 5 attempts or 24 h.
* **Conduit on single-node Hubs** (#3734): A single-node Hub starts its relay without relay-peer authentication, and the "relay not running" 503 names the failing setting.
* **Other** (#3773, #3765, #3718): Closing the Postgres event publisher no longer deadlocks with a concurrent unsubscribe. A transport auth file replaced during a read is re-read instead of briefly clearing the credential. In Cloud Logging, an empty `trace_id` no longer overrides an earlier value.

## 📖 Docs
* **Guides** (#3764, #3757, #3703): A new Agent Plugins page. Settings precedence and template authoring for the files-as-source model. The single-node VM deploy docs now run their image and settings checks with `sudo`.
