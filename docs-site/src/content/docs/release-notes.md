---
title: Release Notes
---

Scion release notes are published weekly to highlight major features, bug fixes, and system updates across the platform.

## Week of July 27 -- August 2, 2026

This week delivered a full-stack session metrics subsystem from agent-side telemetry aggregation through API endpoints and web UI dashboards, completed the config-driven auth migration that eliminates all hardcoded per-provider credentials, and shipped pre-start customization hooks at both project and hub scope. The A2A protocol bridge gained admin UI integration with live config management, Discord added one-step thread+agent creation and file retrieval commands, and a sustained chain of auth and injection fixes resolved a class of Vertex AI restart failures.

---

### ⭐ Highlights

#### 1. Session Metrics Infrastructure (M3+M4)
An entirely new observability subsystem shipped end-to-end. M3 introduced an agent-side telemetry aggregation engine in sciontool, a `MetricsPayload` field in the StatusUpdate protocol, a new `agent_session_metrics` Ent schema, and a hub ingestion endpoint with auth gating. M4 followed immediately with three API endpoints (`/agents/{id}/metrics/summary`, `/metrics/session/{id}`, `/projects/{id}/metrics/summary`), SQL-level aggregation with IDOR-safe authorization, and a web UI layer including an agent detail metrics tab, stats columns in the agents list, and a project summary view.

#### 2. Config-Driven Auth Migration (Phases 3–4)
All hardcoded per-provider auth fields — `AnthropicAPIKey`, `GeminiAPIKey`, `CodexAPIKey`, and their switch-case dispatch tables — were removed. Every built-in harness now resolves credentials exclusively through `FromConfig` variants, decoupling the auth pipeline from provider-specific knowledge. This completes a multi-phase architectural cleanup that makes adding new model providers a configuration change rather than a code change.

#### 3. Pre-Start Customization Hooks
Project-level pre-start hooks shipped on July 27 with a new `ProjectPreStartHook` Ent entity, project-scoped shell scripts staged as `pre-start.d/30-project-custom` with abort-on-failure wiring. The next day, scope expanded to hub-level hooks with a `scope` enum (project/hub), web UI components in Hub Resources and Project Settings, a `scion hub hook` CLI subcommand, and project-overrides-hub precedence resolution. Operators can now inject environment setup, credential validation, or compliance checks that run before every agent start.

#### 4. A2A Bridge Admin Integration
The A2A protocol bridge was elevated from a standalone plugin to a first-class Hub admin integration across three phases. Phase 1 introduced a `KnownPlugin` catalog replacing the flat plugin list, with a self-managed install flow and frontend platform support. Phase 3 delivered real config management through the admin UI — `Configure()` with atomic snapshot swap, `AdminOverlay` hot-swap, and enriched `HealthCheck()`. Earlier in the week, the bridge also gained per-user access token auth with `hubUAT` and `hubJWT` schemes, SHA-256 keyed token cache, and `CallerIdentity` context propagation.

---

### 📊 Metrics & Observability

- **GCP Error Reporting integration:** `serviceContext`, `stack_trace` on ERROR+, and `@type` annotation for automatic error detection in GCP console.
- **Subsystem logging tags:** Tier 3 logging tags added to remaining CRUD handlers, completing structured log coverage across the hub.
- **Telemetry pipeline resilience:** Retry with exponential backoff for telemetry exports; GCP project ID derived from SA credentials for metrics dashboards instead of requiring manual configuration.
- **Cloud Monitoring query fix:** Corrected `ALIGN_SUM` to `ALIGN_DELTA` for single-point cumulative series, fixing inaccurate metric visualizations.

### 🔐 Auth & Security

- **Harness auth corruption fix chain:** Resolved a self-perpetuating corruption bug where harness implementation names (e.g. `container-script`) leaked into `opts.HarnessAuth` and `scion-agent.json`, causing Vertex AI agents to fail with "not logged in" on every restart. Added `GOOGLE_CLOUD_PROJECT` → `ANTHROPIC_VERTEX_PROJECT_ID` translation and a defensive guard rejecting known harness names from auth backfill.
- **`as_needed` injection enforcement:** The `as_needed` annotation was previously stored and shown in the UI but never filtered at dispatch time. Enforcement was added to all three injection paths in `httpdispatcher.go`, followed by a three-part fix chain: two-pass env-gather resolution, `DispatchAgentRestart` env/secret resolution, and second-pass resolution in `DispatchFinalizeEnv`. File-type and variable-type secrets are now exempt from the filter.
- **Policy API admin gating:** All 11 policy API handlers now require `requireAdmin` — previously any authenticated user could create, modify, or delete authorization policies.
- **Multi-GitHub credential support:** Convention-based secret key derivation (`GH_OWNER__REPO`, `GH_OWNER`) inserted into the `tokenForRef` precedence chain, enabling differently-scoped tokens for multiple repos. Purely additive and backward compatible.
- **Credential decoupling from NoAuth guard:** `provisionCredentials` no longer skips when `no_auth.behavior: drop-to-shell` is set, unblocking `GitHubSkillResolver` access to convention-based `GH_` tokens for private repo skill resolution.
- **Permission error surfacing:** Secret creation permission errors now return 403 with actionable guidance instead of generic 500.

### 🧩 Skills & Provisioning

- **Hub-side `gh://` skill cache activation (Phase 3):** `RegisterFallback` routing infrastructure with context-cancellation-safe fallback flips resolution to the hub-side cache, with broker-side fallback when the Hub is unreachable. Phase 2 defects (hash format, expiring URLs, cross-project authz) were fixed in the same cycle.
- **Batch skill import:** Paste a GitHub directory URL to discover skill subdirectories, select via checkbox interstitial, and add all checked skills as individual URI references in a single atomic PUT. URLs are stripped of credentials before logging.
- **Scope-based skill collision resolution:** Replaces the hard error in `installResolvedSkills` with a precedence dedup pass (project > template > user > hub > platform). `Scope` field annotated at all injection sites; collisions logged and recorded in `resolved-skills.json`.
- **Ref→SHA deduplication:** Duplicate `gh://` ref→SHA lookups within a single resolve request are now deduplicated with case-insensitive owner/repo memo keys.
- **Broker-singleton skill cache:** GitHub skill resolution cache promoted from per-request ephemeral to broker-level singleton with extended TTLs (5m → 30m default, 24h for SHA resolution).
- **Platform skill expansions:** Six skills added or updated — scheduler, git-operations, workspace orientation, messaging, agent management recovery, and shell safety.

### ⚙️ Settings & Project Management

- **Settings precedence overhaul:** Hub agent defaults and env scope ordering corrected, with thinking-level propagation fix and comprehensive design documentation. Project `default-harness-config` now correctly outranks template harness config.
- **Project clone:** Deep-copies settings, labels, env vars, skills, hooks, harness configs, and templates with defer-driven rollback. Frontend displays hub-default placeholders.
- **Resolved settings endpoint:** New `GET /projects/{id}/settings/resolved` shows hub defaults per-setting, enabling the UI to distinguish inherited from overridden values.
- **Hub agent defaults pipeline:** `DefaultModel` and `DefaultThinkingLevel` wired into the hub defaults pipeline. Canonical shared harness name list replaces three inconsistent hardcoded lists.
- **Init ordering:** Git clone now runs before pre-start hooks — provisioner-created files in `/workspace` previously caused `isWorkspaceEmpty` to skip the clone.

### 💬 Chat Integrations

- **`/scion thread` command:** Create a Discord thread and a Scion agent in one step, with `X-Scion-On-Behalf-Of` delegated identity middleware, template autocomplete, and in-thread progress feedback.
- **`/scion send` enhancements:** File retrieval from the shared scratchpad by absolute path or partial-name search with button picker and symlink traversal protection. Container-to-host path translation added so agent container paths resolve correctly, with configurable `send_search_root`.
- **System message category:** New `type:system` messages for hub-generated operational notices (`delivery-failed`, `scheduler`, `port-forward`), with rendering support across all chat integrations.
- **@mention parsing and --cc flag:** Multi-recipient notification fan-out via `@mention` parsing in message bodies and a new `--cc` flag.
- **Build version display:** `/help` command output now shows build version and git commit hash via build-time ldflags injection across Discord and Telegram.
- **Agent status icon corrections:** Stopped, errored, and crashed agents now display correct phase-specific icons instead of a generic play icon.
- **Thread parent cache fix:** Transient Discord API errors no longer permanently block thread parent lookups; `threadParentID()` now distinguishes confirmed results from failures.
- **IAP transport inheritance:** `longHTTPClient` inherits IAP transport, fixing 401 errors on IAP-protected deployments for long-running operations like `CreateAgent`.
- **Inbound attachment downloads:** Configurable `downloads_path` for inbound attachments in isolated workspace modes.

### 🖥️ Web UI

- **Shoelace dialog migration:** All 39 native `alert()` calls and 23 `confirm()` calls across 22 files replaced with shared Shoelace-based `showToast()` and `showConfirm()` components.
- **Quick-message buttons:** One-click message modal for agents in detail, list, and graph views — Enter sends, Shift+Enter for newline — capability-gated via existing message check.
- **Restructured server-config:** General tab split into three cards (General, Agent Defaults with sub-tabs, Project Default Settings). Message Broker moved to Hub Server tab; Telemetry toggle moved to Agent Defaults.
- **Terminal shortcut on graph cards:** Icon-only connect-to-terminal button in graph view, gated by attach capability and disabled for offline agents.
- **Agent list improvements:** `lastActivityEvent` shown instead of heartbeat time; harness config dropdown sorted alphabetically; label-filter control added to project-level agent list.
- **Skill URI handling:** Web skill picker now generates canonical `skill://scion/<slug>` URIs; long URIs middle-truncated to preserve the identifying skill name, with full URI on hover.
- **GCP telemetry settings:** GCP Project ID and Cloud Logging configuration added to admin telemetry UI.
- **Graph view polish:** Text selection prevented during drag-to-pan; graph-view toggle removed from the projects list page (where it was meaningless).

### 🚀 Agent Lifecycle & Runtime

- **Agent port forwarding:** Agents can expose local HTTP ports through the Hub as authenticated, reverse-proxied URLs. Auto-expose package scans `/proc/net/tcp{,6}` with diff-based reconciliation on configurable ticker; `auto_expose_ports` wired through admin settings API. HTML error pages returned for browser requests to unexposed ports.
- **`scion resume --force`:** Recover crashed agents from the error phase with in-place restart and harness resume flag, so interrupted sessions continue rather than starting fresh.
- **Message attachments through scratchpad:** Attachment delivery now routes through the scratchpad shared volume, fixing silent delivery failure in isolated workspace modes.
- **Scheduler concurrency:** Configurable scheduler interval and per-task concurrency (`max_concurrency`, default 2) with jitter before semaphore acquire to avoid thundering-herd. Fixes DB connection pool saturation on small deployments.
- **Scratchpad auto-provisioning:** New projects automatically receive a default scratchpad shared directory via `project_defaults.default_scratchpad` toggle (default: ON).
- **Claude harness model alias resolution:** `provision.py` now resolves model aliases using harness config `model_aliases` with `SCION_MODEL` env var fallback.

### 🐛 Bug Fixes & Stability

- **Chown root-owned files:** Provisioner-created root-owned files chowned after provisioning to prevent undeletable agents.
- **Runtime error propagation:** `ImageExists` now propagates errors from daemon-unreachable instead of returning `(false, nil)`. Detected runtime used for broker heartbeat instead of hardcoding `container`.
- **Image registry validation:** `image_registry` validated before starting runtime broker — fails fast with an actionable error instead of causing opaque image-pull 404s.
- **Broker message rejection:** Inbound messages to non-running agents rejected with 409 instead of being silently swallowed. Non-existent agent messages on Discord now show an error.
- **Mention metadata preservation:** CC'd agents correctly see primary recipients in delivery messages; group message type set to `group-set` instead of `instruction`.
- **SA verification honesty:** Returns 503 when verifying service account without token generator instead of false `Verified=true`.
- **Config cleanup:** `project_id` written instead of legacy `grove_id`; `Content-Type` checked before falling back to legacy grove endpoint to fix spurious deprecation warnings; `schema_version` advisory warning for settings files.
- **Hub maintenance guard:** Concurrent maintenance operations rejected with 409 Conflict, preventing `go build` pile-up that caused the July 28 outage.

### 📖 Docs

- **A2A Protocol Bridge documentation:** Consolidated and revamped A2A bridge documentation.
- **Multi-GitHub credential guide:** Naming convention, normalization rules, setup examples, credential resolution order, and injection mode semantics.
- **Shared directory patterns:** Common patterns for project shared directories — build caches, producer/consumer artifacts, shared knowledge base, file-based coordination.
- **Agent lifecycle corrections:** Fixed false safety guarantee ("committed" is not "pushed"), clarified `--preserve-branch` behavior, replaced "broker slots" with "system resources."
- **Skill documentation updates:** Messaging skill (2000-rune limit, inbound type discrimination, `--notify` deprecation), agent management (troubleshooting triage table, recovery sections), scheduler (whoami recipe for self-scheduling agents).

---

## Week of July 19 -- 25, 2026

This week brought substantial improvements to Discord's multi-server capabilities, a complete skill system expansion with private-repo support and multi-scope auto-injection, and critical correctness fixes across the dispatch pipeline. The agent lineage graph visualization debuted for complex project debugging, and relative workspace paths now resolve correctly through the entire orchestration stack.

---

### ⭐ Highlights

#### 1. Discord Multi-Server Support
Discord evolved from single-guild to full multi-server deployment with end-to-end changes spanning command registration, admin UI, and lifecycle management. The new `Config.GuildIDs` array replaced the singular `GuildID`, enabling concurrent slash command registration across multiple servers. Guild-removal cleanup now tracks `guild_name` and deactivates links when the bot is removed from a server. The admin UI exposes comma-separated `guild_ids` input with a "Global — all servers" placeholder and generates OAuth2 bot invite links with correct permission bitmasks. The agent cache TTL was reduced from 5 minutes to 30 seconds to eliminate stale `/default` listings, and `/default` gained autocomplete for large projects with case-insensitive slug validation. Multi-server setup documentation covers the trust model, invite flow, and operational considerations.

#### 2. Skills Expansion: Private Repos, Multi-Scope Injection, and URI Validation
The skill system matured into a production-ready subsystem with three major pillars. First, private-repo resolution via `gh://` URIs: skill resolver now injects GitHub tokens from project git credentials, supports per-URI credential selection through `?token=SECRET_NAME` query params, validates cache authorization, and eliminates the unauthenticated double-download bug that caused 404s on private repos. Second, multi-scope auto-injection: new `project_skill` and `user_skill` Ent schemas enable skill management at project and user scopes with CLI commands and automatic injection into agent provisioning based on scope resolution. Third, skill URI input validation and auto-transform: `NormalizeSkillURI` converts GitHub tree/blob URLs to canonical `gh://` form, rejects `scion://` with clear errors, and validates `gh://` shorthand structure — applied at hub (422 on invalid) and CLI (stderr notice on auto-convert).

#### 3. Agent Lineage Graph Visualization
A new graph view mode shipped for the web UI to visualize complex agent parent/child relationships. The initial implementation renders agents as a zoomable, pannable forest with HTML card nodes, SVG cubic-curve edges, spawn-direction arrowheads, pan/zoom/fit-to-view controls, and collapse pruning. A same-week refactor extracted the graph into a shared `<scion-agent-tree-view>` component rendered inline with grid/list modes so status and label filters apply automatically. The view is now available on both agents and project-detail pages, providing a third visualization mode alongside the existing grid and list layouts.

#### 4. GKE & IAP Hardening for Hosted Brokers
GKE-hosted broker dispatch received multiple rounds of targeted fixes. Hub endpoint derivation now pulls from IAP audience URLs instead of hardcoded values, broker auth token flow was corrected, and transport `oidc_audience` was decoupled from hub endpoint for independent resolution. Transport auth is now resolved before app-token gates in `attach` to unblock IAP mode, and GFE proxy health check interception is handled gracefully — detecting non-JSON 2xx responses from reverse proxies and falling back to `/health` with descriptive error messages. K8s runtime improvements, plugin hub client adjustments, and Discord/Telegram broker registration updates completed the hardening work. End-to-end GCP setup documentation now covers Cloud Run Hub + Discord + GKE Autopilot deployments with infrastructure, IAM, dispatch verification, and maintenance.

---

### 📡 Discord & Chat Integrations

- **Multi-guild command registration:** `Config.GuildIDs` replaces singular `GuildID`, with concurrent registration across guilds and backward-compat fallback; `handleGuildDelete` deactivates links when bot is removed from a server, with `guild_name` tracking populated from session cache.
- **Admin UI multi-server controls:** `guild_ids` config exposed with comma-separated input and "Global — all servers" placeholder; bot invite link button constructs OAuth2 authorize URL with permissions bitmask when Application ID is populated.
- **Agent cache TTL reduction:** Reduced from 5 minutes to 30 seconds to prevent new agents from being invisible in `/default` listings and mention resolution.
- **Autocomplete on `/default`:** Agent parameter gains autocomplete for large projects with case-insensitive slug validation.
- **State notifications default to off:** New channel links no longer spam by default.
- **@Mention routing correctness:** Body @mentions now route as `TypeMention` messages instead of injecting mentioned agents as group recipients, fixing incorrect multi-agent dispatch; default agent target restored when body-mention filtering empties the target list, with guards against human-mention and slash-command messages.
- **Observed message identity:** Messages now display under the actual sender's webhook identity and avatar instead of the topic agent's, with gray-sidebar embed styling to distinguish relayed messages and duplicate text content removed.
- **Observe mode filter fail-closed:** Rewritten using `resolveChannelLink` with proper thread-to-parent channel link resolution — previous implementation looked up thread IDs directly against the store but channel links are only persisted against parent channels.

### 🧩 Skills & Provisioning

- **Private-repo `gh://` skill resolution:** Injects GitHub token from project git credentials, supports `?token=SECRET_NAME` query param for per-URI credential selection, validates cache authorization, and eliminates unauthenticated double-download causing 404s on private repos.
- **Multi-scope skill auto-injection:** New `project_skill` and `user_skill` Ent schemas with CLI commands for managing project and user skills, automatic injection into agent provisioning based on scope resolution.
- **Skill URI validation and auto-transform:** `NormalizeSkillURI` converts GitHub tree/blob URLs to canonical `gh://` form, rejects `scion://` with clear errors, validates `gh://` shorthand structure; applied at hub (422 on invalid) and CLI (stderr notice on auto-convert).
- **Mandatory instruction preamble:** Embedded `mandatory_boilerplate/` FS prepended to every provisioned agent's instructions regardless of template.

### 🧠 Model & Harness

- **Model alias resolution timing fix:** Aliases now resolved before storing in `AppliedConfig` and `SCION_MODEL` — previously stored raw, causing harness provision scripts to receive unresolved tier names.
- **Harness model resolution fallback:** Fixed in OpenCode, Hermes, and Gemini CLI — `ctx.model_resolution` is always empty because `ProvisionManifest` has no `model_resolution` field; now falls back to `SCION_MODEL` env var.
- **Gemini CLI improvements:** Injected `GEMINI_SYSTEM_MD` env var for system prompt pickup, added single-letter model alias mappings (S/M/L), performs fallback alias resolution in `provision.py`; medium model alias updated to gemini-3.6-flash.
- **Claude harness config:** Added deny list and disable flags to `settings.json`.
- **Fresh claude-code install:** Forces `@latest` tag with npm cache clear to prevent stale packument from resolving to month-old versions.

### 🚀 Agent Lifecycle & Workspace

- **Relative workspace paths:** Resolves subdirectory paths against project logical root with containment checks (traversal, symlink escape), preserves relative paths through the hub/dispatcher pipeline.
- **Workspace sharing mode env vars:** `SCION_WORKSPACE_MODE` and `SCION_WORKSPACE_GIT` injected at both hub dispatch and broker start layers so agents can adapt behavior to exclusive/shared/git workspace configurations.
- **Agent/project list pagination:** Raised limit from 50 to 500, fixed agent cursor pagination with CLI/hub/store tests and web UI pagination support.
- **Enhanced `whoami`:** Typed `WhoamiResult` struct with Tier 1 env-var fields (project, template, harness, model, creator, etc.) and `--full` flag for Hub-enriched Tier 2 output (phase, ancestry, labels, taskSummary) with graceful degradation.

### 🏗️ Broker & Dispatch

- **Shared-reference mutation bug fix:** Clone `ResolvedEnv` map before mutation in `buildCreateRequest` — shared reference allowed secret injection, storage env merge, and GitHub token writes to silently modify agent's canonical config; also injects `SCION_MODEL` from `AppliedConfig.Model` in all three dispatch functions.
- **Plugin message broker routing:** Plugin is now added to `message_broker.types` on web UI install — `handleInstallIntegration()` loaded the plugin but never added it to the types list, excluding it from message routing.
- **FanOut spoke wiring:** Installed/restarted broker plugins now wired as FanOut spokes — without spoke wiring, plugins never received `Subscribe()` calls so `startGateway()` never fired until full hub restart.
- **Notification dispatch guard:** Don't silently mark notification dispatched when agent has no `RuntimeBrokerID` — was permanently losing notifications; now leaves them undelivered for future retry.

### 🌐 GKE & IAP

- **Hub endpoint derivation:** Derive from IAP audience URL instead of hardcoding, with transport `oidc_audience` decoupled from hub endpoint for independent resolution.
- **Transport auth ordering:** Resolve transport auth before app-token gate in `attach` to unblock IAP mode — auth was checked after the token gate, preventing IAP-authenticated connections.
- **GFE proxy health check handling:** Detect non-JSON 2xx responses from reverse proxies and fall back to `/health` (hub) or return descriptive error naming likely cause (broker).
- **GKE hosted broker dispatch fixes:** K8s runtime improvements, plugin hub client adjustments, Discord/Telegram broker registration updates.

### 🖥️ Web UI

- **Agent lineage graph view:** Renders agents as parent/child forest with HTML card nodes, SVG cubic-curve edges, spawn-direction arrowheads, pan/zoom/fit-to-view, and collapse pruning; refactored into shared `<scion-agent-tree-view>` component used on both agents and project-detail pages.
- **Help button:** Wired to open docs site in new tab.

### 🐛 Bug Fixes & Stability

- **Hermes arm64 build:** Removed nodesource apt repo after nodejs install to fix "Cannot allocate memory" in QEMU emulated builds caused by stale InRelease file.
- **CLI help text:** Updated `scion message --attach` help text with accurate path roots and failure mode; changed `scion://` to `skill://` in examples.
- **Skill resolver CI fix:** Repaired `ParseSkillURI` grammar to handle edge cases and updated stale test fixtures.
- **Discord `gofmt` cleanup:** Unblocked main CI.
- **Test cleanup:** Removed stale transport-audience-mismatch test case decoupled by earlier work.

### 📖 Docs

- **Discord multi-server setup:** `guild_ids` config, trust model, invite flow, guild removal behavior, and operational notes.
- **GCP setup tutorial:** End-to-end Cloud Run Hub + Discord + GKE Autopilot deployment guide covering infrastructure, IAM, dispatch verification, and maintenance.

---

## Week of July 12 -- 19, 2026

This week's work centered on two major arcs: a comprehensive overhaul of the chat messaging stack — with position-aware @mention routing, native mention rendering across Discord and Slack, and Telegram V2 becoming the default broker — and a sustained push on high-availability reliability, covering stateless HA env-gather, broker ID recovery on restart, and full OIDC transport auth for IAP-protected GKE deployments. Alongside these, a new hub admin settings system shipped with seed/managed config layering, and thinking-level control gained a first-class `--thinking-level` CLI flag with per-harness env-var injection across the agent stack.

---

### ⭐ Highlights

#### 1. Messaging Platform Overhaul: @Mentions, Thread Defaults, and Telegram V2
The chat integration layer received its deepest redesign to date. A new `TypeMention` message type provides position-aware @mention routing across Discord, Slack, and Telegram — agents are now dispatched via explicit mention rather than being injected as group recipients, eliminating an entire class of incorrect multi-agent dispatch bugs that also received follow-up routing and fallback-to-default fixes on July 19. Native @mention rendering landed in Discord and Slack outbound messages, replacing raw email strings with platform-native `<@id>` syntax (with duplicate-email handling and cached lookups). Discord gained per-thread default agent resolution via a new `thread_defaults` table with two-tier resolution, mirroring Telegram's existing topic_defaults pattern. Telegram V2 became the default broker (V1 remains available via `SCION_TELEGRAM_V1=1`), gained audio/video attachment downloads, and workstation onboarding now includes a client-side QR code for bot verification.

#### 2. HA & GKE Hosted Broker Reliability
Multiple days of targeted fixes hardened Scion's high-availability story for production deployments. The most consequential: `DispatchFinalizeEnv` was rewritten as a stateless, replay-based operation — previously, finalize-env calls routed to a different broker replica caused 404 errors; the fix rebuilds the full create request via `buildCreateRequest` and dispatches it cleanly. Brokers now recover their ID from the database on restart and re-assign orphaned agents, preventing UUID regeneration from silently losing live agent state. Three GKE hosted broker dispatch fixes unblocked deployments: skipping local `ImageExists` checks for non-local runtimes, correctly scanning all profiles for `cloudrun` type, and deriving hub endpoints from IAP audience URLs rather than hardcoding. OIDC transport auth shipped for IAP-protected hubs via the new `pkg/transportauth` package, completing a multi-phase workstream — with a follow-up fix decoupling the transport `oidc_audience` from the hub endpoint for proper independent resolution.

#### 3. Hub Admin Settings: Seed/Managed Config Layering
A new hub admin settings subsystem introduces `SCION_SEED_*` environment variable providers with a three-layer bootstrap merge (SEED → yaml → SERVER). Settings can be seeded for managed deployments, and the admin UI now shows which values are seeded versus actively managed — with deprecation detection to flag stale config. This is the foundational infrastructure for fleet-managed Scion deployments where operators need to inject immutable defaults without forking config files, and it shipped alongside a related fix for the plugin config deadlock where fresh installs couldn't save the config needed to activate a plugin.

#### 4. Thinking-Level Control Across the Agent Stack
First-class thinking-level support landed end-to-end. `scion start` now accepts a `--thinking-level` flag (0–100) that is injected into agent containers as `SCION_THINKING_LEVEL`, following the same pattern as `SCION_MODEL`. Codex maps this to its `reasoning_effort` parameter; Antigravity harness reads it with a 4-tier mapping and backward-compat fallback from the legacy `AGY_THINKING_LEVEL` variable. The default OOB harness was also migrated from Gemini to Antigravity, consolidating the thinking-level path.

---

### 📡 Chat Integrations

- **Telegram V2 default:** V2 broker is now the default broker; V1 remains accessible via the `SCION_TELEGRAM_V1=1` env var.
- **Position-aware @mention routing:** New `TypeMention` message type enables mention-triggered agent dispatch with positional metadata, replacing the old approach of injecting mentioned agents as group recipients.
- **Discord per-thread default agents:** New `thread_defaults` table with two-tier resolution and thread-aware `/setup`, `/default`, `/status` slash commands, mirroring Telegram's topic_defaults pattern.
- **Discord inbound attachments:** Downloads Discord message attachments to the agent workspace with path-traversal sanitization and resource-leak guards.
- **Telegram audio/video downloads:** Agents now receive audio and video files in addition to photos and documents; configurable `downloads_path` with graceful sticker/animation skipping.
- **Workstation Telegram QR code:** Client-side QR generated via the `qrcode` package on the onboarding verification step, updating dynamically as the code is entered.
- **Native @mention rendering (Discord & Slack):** Outbound agent messages replace raw email references with platform-native mention syntax, with duplicate-email handling and cached user lookups.
- **Discord observed message identity:** Relayed messages now display under the actual sender's webhook identity and avatar, with gray-sidebar embed styling to distinguish them visually.
- **Auto-enable message broker:** Server automatically enables the message broker when broker plugins are configured, preventing the silent "bot configured but never starts" failure mode.
- **Telegram admin UI & delivery errors:** Config fields and bot token UX improvements; 5xx delivery errors now propagated back to the hub and CLI caller instead of being silently swallowed.
- **Mention routing fallback guard:** Default agent target is restored when body-mention filtering empties the target list, with guards against human-mention and slash-command messages.

### 🏗️ HA & Multi-Node

- **Stateless env-gather finalize:** `DispatchFinalizeEnv` rewritten to be replay-safe — rebuilds the full create request from scratch via `buildCreateRequest`, eliminating 404s when finalize-env hits a different broker replica in an HA cluster.
- **Broker ID recovery on restart:** Brokers recover their ID from the database and re-assign orphaned agents on startup, preventing UUID regeneration from orphaning live workloads when settings are lost.
- **OIDC transport auth for IAP hubs:** New `pkg/transportauth` package provides shared OIDC transport infrastructure; broker OIDC transport auth for IAP-protected hubs completes Phase 3 of the HA Cloud Run OIDC workstream.
- **GKE hosted broker dispatch fixes:** Skips local `ImageExists` checks for non-local runtimes (resolves docker.io lookup errors on GKE), scans all profiles for cloudrun type rather than relying on `active_profile`, and derives hub endpoints from IAP audience URLs; follow-up decouples `oidc_audience` from hub endpoint for independent resolution.
- **Broker runtime hot-swap:** `SwapRuntime` swaps the runtime client and agent manager in-place when the container engine changes, fixing CLI/broker mismatch after the onboarding wizard changes the engine.
- **WebSocket payload limit:** `MaxMessageSize` raised from 64KB to 1MB with a pre-send size guard, unblocking large `RemoteCreateAgentRequest` payloads carrying inline config and base64-encoded bodies.
- **Control channel async dispatch:** Control channel dispatch made async to prevent head-of-line blocking — a slow dispatch no longer stalls all subsequent agent operations.
- **Broker fanout on plugin activation:** Hub replays fanout subscriptions onto spokes added via `AddSpoke`, fixing dead inbound messages after `PUT /config` activates a plugin.
- **Attachment path resolution:** Broker resolves attachment paths to host-side paths for host-process brokers, fixing container-internal `/scion-volumes/` paths being passed to Telegram/Discord plugins where they don't exist.

### 🧠 Model & Thinking-Level

- **`--thinking-level` CLI flag:** `scion start` accepts a 0–100 thinking level injected into containers as `SCION_THINKING_LEVEL`, following the same pattern as `SCION_MODEL`.
- **Codex `reasoning_effort` mapping:** Codex harness maps the 0–100 thinking level to its `reasoning_effort` parameter.
- **Antigravity thinking-level support:** Reads `SCION_THINKING_LEVEL` with a 4-tier mapping and backward-compat fallback from `AGY_THINKING_LEVEL`; default OOB harness migrated from Gemini to Antigravity.
- **Fable model alias:** Claude extra-large model alias changed from `opus` to `fable`.
- **Hermes Vertex AI auth:** Vertex AI auth support added with region fallback chain and cached `_build_vertex_env` result.

### ⚙️ Harness & Provisioning

- **Skills as individual files:** Claude harness now installs skills to `.claude/skills/` as individual files, with `include_skills=False` passed to `project_instructions()` so Claude Code discovers them natively instead of ingesting a large concatenated CLAUDE.md. The `include_skills` default was inverted globally and all harnesses audited.
- **Workspace skill overlay removed:** The `injectWorkspaceSkills()` provisioning step, root `skills/` directory, and associated tests deleted — superseded by embedded platform skills.
- **Copilot improvements:** Auth.json credential file capture, auth type naming fix (`auth-file` instead of `config-file`), instructions now write to `~/.github/` instead of workspace, hook support via `~/.copilot/hooks/scion.json`, and relative path fixes for `config_dir`/`skills_dir`/`instructions_file`.
- **OpenCode improvements:** `auth.json` file secret capture, no-auth login command, `--model` flag passthrough when model is configured, and `capture_auth` ordering fix.
- **gcloud ADC user opt-in:** Auto-injection of gcloud Application Default Credentials is now gated behind an explicit user setting and onboarding wizard toggle, preventing unexpected credential exposure. ADC is auto-detected in workstation mode.
- **Cloud Run Docker skip:** Broker skips Docker runtime initialization when docker binary is unavailable, falling back to cloudrun runtime when `K_SERVICE` is set; Docker heartbeat also skipped in this state.

### 🛠️ Hub & Plugin Lifecycle

- **Plugin config deadlock fix:** `PUT /config` falls back to `settings.yaml` for installed-but-unloaded plugins, resolving the chicken-and-egg problem on fresh installs where config couldn't be saved without loading a plugin that required that config. After save, the plugin activates via `LoadOne` with full resolved config; `config_file` path stored immutably so `PUT /config` can always find it.
- **Plugin lifecycle robustness:** Plugin install/reconfigure errors surface in HTTP responses with sanitized messages; integrations show as Available when plugin binary is on `$PATH` (fixes Homebrew installs where `SCION_MAINTENANCE_REPO_PATH` is unset); registered-but-not-active plugins accepted in `HasPlugin` and `ListPlugins`.
- **Harness config UI improvements:** Validation errors surfaced in harness config import UI (previously silent HTTP 200); harness configs sorted alphabetically by `displayName`; dropdown used on server config page; image re-parsed from `config.yaml` on save/upload; local image state shown in workstation/podman mode.
- **Image registry routing:** `image_registry` now applied when dispatching agents to remote brokers.
- **Dispatch failure surfacing:** `dispatch_failure_reason` exposed in CLI and API responses for failed messages, with participant-privacy integration tests.
- **Agent name conflict errors:** User-friendly 409 response returned at runtime, broker, and hub layers when an agent name is already in use (Docker, Podman, and Apple Container).
- **Hub credential reconstruction:** `getPluginHubCreds()` rebuilds `hub_url`, `broker_id`, `hmac_key`, and `project_slug_map` from authoritative live sources instead of relying on an empty plugin manager cache.

### 🐛 Bug Fixes & Stability

- **Postgres idempotence:** `Schema.Create` now skips `42P07` (duplicate table) errors; `skipExistingRelations` hook gated to Postgres only.
- **Web UX polish:** Password-style font in secret textareas; multi-line support for file-type secret intake; syntax highlighting for JSON and YAML in workspace file viewer; project templates sorted before global templates in the template list; QR code favicon added.
- **Broker profile fixes:** Profiles filtered by detected runtime (not hosted flag), with a fallback default profile when all configured profiles are filtered out; configured profiles enumerated in the broker info endpoint.
- **Config compatibility:** camelCase koanf field support for `SCION_SERVER_` env vars; v1-shaped runtime field detection when `schema_version` is missing; `cloudrun` added to V1 schema runtime type validation.
- **Provision cleanup:** Root-owned `__pycache__` directories no longer persist after agent delete.
- **Build compatibility:** Empty array expansions guarded under `set -u` for bash < 4.4 compatibility across three image-build scripts.

---

Looking for older release notes? See the [Prior Release Notes Archive](/scion/release-notes-archive/) for daily entries from the early development period (Feb--Jul 2026).
