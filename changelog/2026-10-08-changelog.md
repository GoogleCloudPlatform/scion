# Release Notes (2026-10-08)

The Hub now suspends a removed member's agents and their descendants, and sharing a Runtime Broker with a project needs the owner's consent. Scheduled messages joined scheduled dispatches under recorded schedule authority. Artifacts (experimental) gained reviews, grants, retention and share links. Conduit can now carry agent PTY sessions, with re-checked authorization on user streams.

## ⚠️ BREAKING CHANGES
* **Removed members' agents are suspended** (#2801): When a user's access to a project ends, the Hub suspends the agents they started there and every agent those agents created. Suspended agents keep their workspace but are refused on start, messaging, token minting, secrets, schedules and agent creation until a Hub admin lifts the hold (`POST /api/v1/agents/{id}/hold/lift`).
* **Runtime Broker sharing needs owner consent** (#2665): Linking a Runtime Broker to a project (providers API, project register, `hub link`, auto-link on agent create) requires `broker.update` on the Runtime Broker. Members dispatch to a provider only when it was linked by its owner or an active super-admin, or is auto-provide. Only super-admins set `broker.auto_provide` or link the embedded Runtime Broker.
* **Scheduled messages need re-authoring** (#2816): Scheduled messages are sent under the authority recorded at the last create, change or resume. Message schedules written before that do not fire until paused and resumed, or the event is recreated. Writes with a credential whose authority cannot be recorded (such as a federated sign-in) get 403.
* **More starts answer 409 after a lost delete** (#2844): Managed start and restart, and `POST /agents` on an existing agent, answer 409 `delete_in_progress` instead of 200 (or 404) when a delete wins before the final read.
* **Agent create without a git remote on non-GCS Hub storage** (#2852): On a remote Runtime Broker with no local path, creating an agent in a project with no git remote now fails up front with 412. Use GCS Hub storage, add a git remote, or link the project at a local path.
* **Conduit user stream deadline** (#2913): User streams are re-checked at `server.hub.conduit.stream_authz_max.user` (default 8h) and closed with 4401 if access was withdrawn.
* **Token schedule authoring refused** (#2848): User access tokens can read schedules with `scheduled_event` selectors, but creating events or schedules with a token is refused.

## 🚀 Features
* **Artifacts, experimental** (#2858, #2871, #2879, #2854): Behind `hub.artifacts`: CriticMarkup reviews (`scion artifact publish --review`, `get --clean`/`--accept`) with a web Review mode. Also added: a grants API, moving artifacts between projects, per-artifact expiry with blob garbage collection, and revocable, time-limited share links.
* **Conduit PTY path and stream re-checks** (#2910, #2841, #2832): The Hub picks the Runtime Broker or the agent's Conduit session for PTY attach per agent. User streams are re-checked when access changes, on reconnect and on a periodic sweep (`authz_recheck_interval`). Legacy control-channel attach streams have bounded buffers, so a slow reader no longer stalls the shared channel.
* **Health status and attention policy** (#2862, #2918): One policy derives overall status and a ranked attention list from Hub checks, Runtime Broker health, NFS, dispatch, integrations and agent error rates. The dashboard reports when the Hub's own identity lacks the IAM permission for service account assignment checks, with the remedy.
* **Scheduled send phase 2, experimental** (#2901, #2845): Behind `web.chat_scheduled_send`: scheduling in direct messages, a 60 min late cutoff, and Send now, Dismiss and Copy to composer for failed messages.
* **Refresh templates from their source** (#2891, #2903): `POST /api/v1/templates/{id}/reimport` refreshes a Hub template imported from a GitHub folder URL. `scion harness-config install` records its source URL.
* **Several Runtime Brokers per host** (#2878): `scion runtime-broker register --broker-name <name>` sets the name the Hub knows the Runtime Broker by, so a second Runtime Broker no longer merges into the first.
* **Ephemeral workspace warning** (#2893): Stopping, suspending or restarting a Kubernetes agent with an EmptyDir workspace warns about unpushed commits or changes first. Nothing is blocked.
* **Private repos on NFS workspaces** (#2825): The NFS provision init container clones with the project's git credential. `GITHUB_TOKEN` moves from plain pod env into the per-agent Kubernetes Secret and is never written to disk.
* **Message IDs** (#2912): Delivered agent envelopes carry `message_id`, and `scion message` prints the created ID.
* **Reloadable GCP permission-check settings** (#2840): Hub admins can change them at runtime from Server Config, with validation and audit.
* **CLI** (#2843, #2916, #2907): `hub token create --expires` accepts `2h` and `90m` and parses all units strictly. `hub projects info`/`delete` accept project IDs. Local `scion list` marks provisioned-but-not-started agents and hints at `scion start`.
* **Web** (#2905, #2908, #2896, #2851): Browser Back steps through chat panels on phones. The project list icon shows the workspace mode. Detail pages share a header that wraps long names. "Remove all inactive" covers every unconnected terminal, and status dots explain their colors.
* **Image and deploy** (#2902, #2881, #2839, #2885): Agent base images include pinned Helm v3.17.3. Cloud Run and single-node VM deploys grant the Hub service account admin, so it can mint service accounts. Starter-hub sets the telemetry project for the metrics dashboard.
* **Web performance** (#2861, #2821, #2897): Vite-fingerprinted assets are cached for a day, unused list prefetches are dropped, and readiness marks are available behind a profiling setting.

## 🔒 Security
* **User access token scoping** (#2900, #2848): Hub integration, GitHub App and observability operations admit only tokens with the matching Hub selector. Integration secrets, installs and GitHub App configuration need an interactive session.
* **Delegation and provenance** (#2813, #2829, #2894): An agent's execution source is resolved from its recorded provenance root, and relationship-derived execution authority requires project admission. Reincarnation by another principal keeps the agent's delegation edge. Agent, project and schedule create carry atomic grant-authority audit obligations.
* **Hardening** (#2909, #2831, #2823, #2837): Staged secret values are masked line by line in provisioner errors. The CLI loopback callback page escapes inserted values. The SPA shell is served `no-store`, and `cdn.webawesome.com` is dropped from the CSP.

## 🐛 Fixes
* **Agent lifecycle** (#2917, #2919, #2906, #2890, #2847, #2887): A slow synchronous create no longer reaches the user as an empty 502 after 60 s. A managed create retries its post-create write once on a version conflict. A superseded queued-stop clear no longer writes over a delete-won row. Preempted or evicted Kubernetes pods leave `running`. Restarting a stopped agent no longer fails in the `20-harness-provision` hook. Runtime Broker 4xx refusals are relayed instead of 502.
* **Settings saves keep fields** (#2921, #2904): An agent config PATCH keeps inline keys it does not mention. A `github_app` save on DB-backed Hubs keeps the App key path.
* **Empty env values are unset** (#2836, #2895): Exported but empty `SCION_*`, `SCION_SERVER_*` and `SCION_SEED_*` variables no longer override configured values.
* **Workspace sync** (#2809, #2889): Hub-native creates that download their workspace from the bucket no longer collide on the root `.scion` entry, and workspace uploads skip it.
* **Runtimes** (#2924, #2886, #2928): Cloud Run Sandbox reconcile keeps entries on ambiguous probes and serializes same-name starts. Replaced Kubernetes pods' per-run objects are removed once the pod is gone. Cloud Run Sandbox keeps entrypoint output when an agent dies early.
* **Scheduler** (#2846, #2865): A recurring dispatch blocked by an errored agent records an actionable error and notifies the owner. Scheduled sends are released when the runtime has already aborted.
* **Chat** (#2868, #2855, #2850): A DM to a deleted agent is reported undelivered, and post-wake sends are bounded. Fixes for Telegram reply routing and multi-replica schema creation, Teams reply context and Discord attachment-only messages. Cloned projects get a #general thread, and project templates no longer appear as spaces.
* **sciontool** (#2830, #2884, #2866): Hook Hub calls share one 3 s budget. Still-open session metrics are reported at daemon shutdown. Loopback OTLP exporters ignore `OTEL_EXPORTER_OTLP_*` env.
* **CLI** (#2827, #2838, #2835): `hub auth login`/`logout` resolve the Hub in the same order as other commands. `suspend --all` with JSON exits non-zero on partial failure. Hub sync reports an empty response instead of the local-only hint.
* **GCP service account actions** (#2899): Project service account lists offer the actions the handlers actually allow.
* **Web** (#2853, #2819, #2856, #2872, #2826, #2873, #2892, #2834): Agent lists and forms no longer show stale or partial data. Tooltips are readable in the dark theme. The admin groups table stays inside its container and shows full descriptions. Multi-pane terminal layouts show empty-slot drop targets and exactly one empty-state message, and placeholders follow the light theme.

## 🔧 CI & Infrastructure
* **Test timeouts and suites** (#2842, #2898, #2877): `pkg/hub` SQLite and Launch Store PostgreSQL timeouts are raised to 60 min. The chat-mobile Playwright suite runs on web PRs.

## 📖 Docs
* **Docs corrections** (#2880, #2860): API limit, GCP setup and messaging-skill corrections, plus recovering deleted built-in harness configs by re-importing from URL.
