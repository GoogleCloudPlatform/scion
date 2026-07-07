---
title: Release Notes
---

## Jul 5, 2026

The largest single commit in recent history landed: Phase 5 HA (Mode 3) support — a complete high-availability architecture for chat integrations with gRPC broker protocol, advisory lock failover, standalone Discord deployment, and an integration runtime library. Platform skills also became embedded in the binary.

### 🚀 Features
* **[HA]:** Phase 5 HA (Mode 3) support — a sweeping ~26,600-line change across 92 files delivering:
  - **gRPC broker protocol** (`proto/broker/v1/`) for cross-process integration communication with adapter, factory, and server
  - **Integration runtime library** (`pkg/integration/runtime/`) with config resolution (DB > env > YAML layering), admin signal listener, update signal handling with status write-back, and schema retry with backoff
  - **Standalone Discord entry point** with `--standalone` flag, Postgres-backed `discord_pending_links`, advisory lock loop with takeover delay and lock-loss detection, gRPC health service, and graceful shutdown ordering
  - **Transactional NOTIFY** for admin signals, cross-integration ID leakage guards, `updated_by` tracking
  - **PostgresConfigProvider** for HA config persistence replacing YAML-only storage
  - Multi-stage Dockerfile and comprehensive standalone deployment documentation (#608)
* **[Agent]:** Platform skills embedded in binary via `go:embed` and injected into all agents at provisioning time — `scion`, `scion-cli-operations`, `scion-messaging`, `agent-status-signals`, `team-creation`, and `git-sandbox` (conditional on `isGit=true`). Runs after template skills but before workspace skills (#610).

### 🐛 Fixes
* **[Hub]:** Check hub-scoped env vars in `hasAnyKey` credential check — child agents whose owner is a parent agent (not a human user) can now find credentials at hub scope (#611).
* **[Claude]:** Fixed env overlay variable resolution in `provision.py` — values now read from `auth_candidates.json` or staged secrets instead of `os.environ`, with `_resolve()` fallback to shell references (#607).
* **[Claude]:** Expanded env var references in Vertex AI env overlay — replaced literal `'${VAR_NAME}'` strings with actual `os.environ.get()` calls across all auth branches (#606).

## Jul 4, 2026

A quieter holiday day focused on auth correctness: the hub's credential detection was fixed to evaluate all config-driven auth types, GCP service account assignments are now honored for file-based credential skipping, and several regressions from the builtin harness removal were patched.

### 🐛 Fixes
* **[Hub]:** Fixed `hasRequiredAuthCredentials` to auto-detect auth type before checking requirements — when `HarnessAuth` is empty (auto mode), the hub now iterates all auth types from `authMeta.Types` instead of short-circuiting on the compiled api-key default. Correctly detects file-based credentials like `CODEX_AUTH` and `gcloud-adc` (#605).
* **[Hub]:** Honor `skipped_when_gcp_service_account_assigned` — when a project has a verified GCP service account, file requirements marked with this flag are treated as satisfied, preventing false no-auth fallback (#604).
* **[Hub]:** Accept `projectPath`/`projectSlug` keys in broker `startAgent` handler for backwards compatibility.
* **[Harness]:** Restored missing `StageCaptureAuthAssets` call in `provision.go` — the builtin harness removal (#600) accidentally deleted this line, causing a build error.
* **[Shell]:** Improved bash shebang lines to use `#!/usr/bin/env bash` consistently, avoiding stale bash versions.

## Jul 3, 2026

A milestone cleanup and expansion day: the entire builtin harness system was deleted (-3486 lines), Slack landed as a full chat integration, the hub gained graceful SSE reconnect for Cloud Run, and Claude's provisioner received OAuth capture and Vertex AI fixes.

### 🚀 Features
* **[Slack]:** Full Slack chat integration as a standalone plugin module — Events API and Socket Mode support, Block Kit formatting, slash command tree (`/scion setup, register, msg, agents, status`), ask-user modal flow, SQLite state store with WAL, hub API client with HMAC signing, per-user registration with code flow (#591).
* **[Slack]:** Slack integration management added to the chat admin UI (#599).
* **[Hub]:** Auto no-auth fallback in pre-dispatch validation — when auth is `auto` and the harness supports `drop-to-shell`, the hub now accepts the agent without credentials instead of rejecting it for missing env vars (#595).
* **[Hub]:** `SCION_IMAGE_REGISTRY` env var takes precedence over `settings.yaml` for image registry resolution (#594).

### 🐛 Fixes
* **[Hub]:** Graceful SSE reconnect before Cloud Run's 3600s hard timeout — server sends a `reconnect` event at 3500s and closes cleanly so the client auto-reconnects per the SSE spec (#590).
* **[Hub]:** Fixed `BootstrapBundledResources` to update existing configs when content changes — split skip logic into `SkipCreate` (respects user deletions) and an always-run update path with content hash comparison (#592, #596).
* **[Claude]:** Fixed Vertex AI auth detection in `provision.py` (#598).
* **[Claude]:** Capture OAuth token (`sk-ant`) from `setup-token` as `CLAUDE_CODE_OAUTH_TOKEN`, restricted to `oat` prefix and most-recent token (#587).
* **[Claude]:** Added `fable` as XL model alias and updated no-auth login command.
* **[Agent]:** Fixed no-auth auto-resolve UI — `HarnessAuth="none"` now propagated back through broker response so the web UI shows the Capture Auth button correctly. Auth method display shows resolved method instead of "container-script" (#586).
* **[Message]:** Closed remaining silent-drop gaps for `--plain`/`--notify`/`--channel` flag combinations with `--in`/`--at` and local mode (#584).
* **[Harness]:** Image status follow-up fixes — error propagation, nil checks, file permissions, atomic write, and `ValidateStorage` hash error handling (#585, #593).
* **[Gemini]:** Set Gemini CLI default model to 3.5 Flash.
* **[Hub]:** Address review comments on `handlers_integrations.go` (#588).
* **[CI]:** Fixed `gofmt` struct field alignment in `types.go` and `models.go` (#589).

### 🗑️ Removals
* **[Harness]:** Removed the entire dead builtin harness system — **-3486 lines**. All harnesses now use container-script provisioning (#600).

### 🔧 Chores
* **[Deps]:** Bumped `golang.org/x/net` in scion-broker-log (#597).

## Jul 2, 2026

A day of polish and depth: harness-config images gained build status tracking with registry probing, the plugin system got hub-mediated install/update flows, skills and templates were factored across repos, and no-auth provisioning learned to auto-fallback gracefully.

### 🚀 Features
* **[Harness]:** Track and display container image status per harness-config — new `image_status` column with local and remote registry checks (including anonymous Docker Hub auth), async refresh on detail page, startup recheck with errgroup concurrency, and list/filter/badge UI (#583).
* **[Harness]:** No-auth auto-fallback and auto-run suggested command — when no credentials are available, provisioning falls back to no-auth mode automatically and surfaces the suggested auth command (#582).
* **[Harness]:** Auto-inject workspace skills during provisioning — skills in the workspace `skills/` directory are automatically made available to agents at provision time (#573).
* **[Chat Admin]:** Hub-mediated plugin updates and first-time install (Phase 4) — `UpdatePlugin` rebuilds from source and restarts, `InstallPlugin` handles first-time build+load, `GET /integrations/available` lists installable plugins, and `FanOutEventBus` gains mutex-protected spoke management for thread-safe dynamic plugin lifecycle (#570).
* **[Web]:** Labels card on agent detail Configuration tab — displays agent labels as `sl-tag` pills, hidden when no labels are set (#581).

### 🐛 Fixes
* **[Capture Auth]:** Propagate exec exit codes and standardize conflict handling — broker now unwraps `exec.ExitError` for real exit codes instead of hardcoding 0. All `capture_auth.py` scripts detect "already exists" and exit with code 3 (`EXIT_CONFLICT`), parsed by a frontend dialog offering Force Update or Cancel (#577).
* **[Build]:** Slimmed Cloud Build source upload from ~2365 files / 38.5 MiB to ~942 files / 12.5 MiB via `.gcloudignore`, with anchored patterns to preserve `image-build/scripts/` and template embeds (#579).
* **[Build]:** Included Dockerfiles in cloud-build context and refocused build pipeline on harness catalog with gemini-cli build step (#576).
* **[Build]:** Run `go mod tidy` before `go build` in plugin install/update to prevent stale module errors (#574).
* **[Hermes]:** Added `--break-system-packages` to pip install for PEP 668 compatibility (#580).
* **[Hermes]:** Installed `python3-pip` in Hermes harness Dockerfile (#578).
* **[Antigravity]:** Bumped AGY_VERSION to 1.0.16 in Dockerfile.
* **[Base]:** Added Playwright CLI to base image.

### 🔄 Refactor
* **[Skills/Templates]:** Factored skills and templates across scion, teamv1, and contrib repos — created `scion-cli-operations` and `git-sandbox` workspace skills, promoted `scion-messaging` and `agent-status-signals` from teamv1, moved fork templates to correct destinations, retired status boilerplate from default `agents.md` (#575).

### 📖 Docs
* **[Glossary]:** Reworked Modes section with availability tiers (single-node hosted vs HA hosted) and tenancy as an orthogonal dimension (#571, #572).

## Jul 1, 2026

A massive infrastructure day: the harness system was refactored from compiled builtins to a bundled resource catalog with directory-based provisioning, a Gemini CLI harness shipped, chat integration admin landed across API and UI phases, the A2A bridge adopted the official SDK, and HA reliability received targeted fixes.

### 🚀 Features
* **[Harness]:** Normalized Claude to directory-based provisioning — moves Claude's harness config from compiled Go code to a standalone `harnesses/claude/` directory, completing the pattern established by PR #279's `provision.py` migration (#548).
* **[Resources]:** Introduced bundled resource catalog for Templates and Harness-configs — embedded resources are now declared in a catalog and promoted through a `ResourceSource` interface with `BootstrapSource` for the hosted startup path and `MaterializeBundledResources` for workstation local seeding (#549, #550, #551, #552).
* **[Harness]:** Gemini CLI container-script harness bundle — full provisioning model with API key/OAuth/Vertex AI auth detection, model aliases, `capture_auth.py`, Dockerfile, and Cloud Build config. Migrates Gemini from the builtin harness to the same pattern as Claude (#563).
* **[Build]:** Refocused image builds on base image and harness catalog — build pipeline now produces a single `scion-base` image plus per-harness images from the catalog (#561).
* **[Chat Admin]:** Chat integration admin API endpoints (Phase 2) — CRUD operations for managing integration plugins via the Hub API (#543).
* **[Chat Admin]:** Chat integration admin UI (Phase 3) — new `/admin/integrations` page with list/detail views, config forms, secrets management, and restart controls (#556).
* **[A2A Bridge]:** Adopted the official `a2a-go` SDK for protocol handling — replaces hand-rolled JSON-RPC with spec-compliant server, `ScionExecutor` bridges SDK events to Scion Hub routing. Preserves auth, metrics, and multi-project routing (#362).
* **[Hub]:** HA robustness improvements — added scheduler jitter (0-30s) and increased non-critical task intervals from 1 to 5 minutes to prevent DB connection thundering herd. Idempotent broker secret for co-located mode survives Cloud Run restarts (#555).
* **[Hub]:** Storage validation, repair, and CLI `validate` commands for diagnosing and fixing storage inconsistencies (#553).

### 🐛 Fixes
* **[Agent]:** Exclude soft-deleted agents from slug lookup and clean stale directories (#547).
* **[Message]:** Reject `--attach` in local (no-Hub) mode instead of silently dropping attachments — also rejects `--attach` combined with `--in`/`--at` since scheduled sends don't carry attachments.
* **[Build]:** Skip image registry rewrite for fully qualified references — prevents double-prefixing when images already include a registry hostname (#566).
* **[Web]:** Dir-browser UX improvements and safety fixes (#559).
* **[Server]:** Improved server lifecycle reliability (#558).
* **[Config]:** Updated `allow_container_script_harnesses` default to `true` in schema (#557).
* **[Config]:** Misc correctness fixes — Makefile, `InitProject`, GitHub URL import (#560).
* **[Web]:** Added plug icon to bundle and hidden `bot_id` from config UI (#562).
* **[Harness]:** Removed legacy seeding dead code and fixed logger subsystem (#554).

### 📖 Docs
* **[Build]:** Warned that `go install` produces a blank web UI and fixed build-from-clone steps (#565).

### 🔧 Chores
* **[Deps]:** Bumped `golang.org/x/net` in A2A bridge (#569).

## Jun 30, 2026

A landmark feature landed: the managed agent backend, enabling Scion to orchestrate cloud-hosted agents (starting with Google's Gemini API) alongside its existing container-based runtime. The glossary was also ported to the repo root.

### 🚀 Features
* **[Managed Agent]:** Added the `ManagedAgentBackend` interface and Google API client — introduces a new execution path where `ManagedAgentManager` implements the existing `Manager` interface but delegates to a cloud API instead of a local Runtime+Harness pair. The first backend targets `generativelanguage.googleapis.com` (Gemini API). Includes SSE stream parser, hub handlers for managed agent CRUD, design document, and ~2800 lines of new code across 21 files (#541).

### 📖 Docs
* **[Glossary]:** Ported runtime broker taxonomy to root `GLOSSARY.md` — updated Runtime Broker definition and added Node-Bound Broker, Proxy Broker, Embedded Broker, Hosted Broker, and Managed Agent entries from the docs-site glossary (#546).

## Jun 29, 2026

The biggest day in weeks: the Claude harness was migrated from builtin to container-script provisioning, a full Cloud Run HA deployment stack landed (Dockerfile, IAP auth, stateless broker routing, Postgres locking), agent labels shipped as a core feature, and chat integrations began a config refactor with secrets migration.

### 🚀 Features
* **[Harness]:** Migrated Claude harness from builtin to container-script provisioning — `provision.py` handles Claude's 4-way auth precedence (API key → OAuth → auth-file → Vertex AI), API key pre-approval, MCP server translation, and env var overlay. Includes parity tests verifying identical output to the compiled harness. Compiled fallback preserved for existing installations (#279).
* **[Hub]:** Chat integration config refactor + secrets migration (Phase 1) — added `IntegrationConfigProvider` with YAML-based per-integration config files, well-known secret key constants for Telegram/Discord/Google Chat, and a `LoadPluginConfigFile` helper that merges file-based config with inline config while filtering secrets (#537).
* **[Agent]:** Agent-specific key-value labels added to the core data model — labels can be set at creation time, displayed on agent detail pages, and filtered/sorted in the agent list view (#531).
* **[Hub]:** Multi-stage `Dockerfile.hub` for Scion Hub — builds frontend assets, embeds them in the Go binary, uses `CGO_ENABLED=0` for a static binary compatible with Debian runtime. Iteratively refined through several commits to fix npm scripts, web asset embedding, and root Dockerfile sync.
* **[Hub]:** IAP proxy auth middleware for Cloud Run — creates hub sessions from IAP identity headers, re-evaluates admin role on every request (not just login), and reduces DB connection pool size for hosted deployments (#530 follow-up).
* **[Deployment]:** Reworked Cloud Run HA deployment config — overhauled `deploy.sh` and `hub-settings-template.yaml` with fail-closed HA preflight checks, stateless broker lifecycle routing, IAP audience normalization, and Postgres advisory locking for safe concurrent migrations.

### 🐛 Fixes
* **[Hub]:** Expire stuck pending messages in broker-message-sweep — messages that remain in `pending` status beyond a threshold are now cleaned up automatically (#545).
* **[Hub]:** Narrowed hosted HA preflight to actual HA deployments — previously the preflight blocked single-instance startups that happened to have Postgres configured (#544).
* **[Runtime]:** Made `SCION_FORCE_HOST_NETWORK` escape hatch runtime-agnostic — works correctly with Docker, Podman, and Apple Container instead of assuming Docker-only semantics.
* **[Hub]:** Stateless Cloud Run broker routing — `deriveCloudRunLogicalBrokerID` now returns errors when project/region is unavailable instead of proceeding with empty values.
* **[Hub]:** IAP audience trailing-slash trimming and `render_settings` escaping fix for deploy scripts.
* **[Hub]:** Postgres migration advisory lock with proper error handling on deferred unlock, preventing concurrent migration races.
* **[CI]:** Resolved `gofmt` and `golangci-lint` failures on main (#538).

### 📖 Docs
* **[Glossary]:** Revised runtime broker glossary with broker taxonomy — node-bound vs proxy, standalone vs embedded — with entries for Hosted Broker, Managed Agent, and cross-references (#539, #540).

### 🔧 Chores
* **[Changelog]:** Merged June 28 entry (#529 follow-up).

## Jun 28, 2026

A large batch of platform improvements landed: Apple Container gained build support and DNS connectivity, a new Hermes Agent harness shipped, Cloud Build became an alternative image builder, and the auth pipeline was decoupled from harness-specific Go code. Chat integrations received several reliability fixes.

### 🚀 Features
* **[Runtime]:** Apple Container added as a build-capable runtime on macOS — `DetectContainerRuntime()` now discovers the `container` binary (Apple Virtualization framework), and tag/push commands use the cross-runtime `image` subcommand form (#509).
* **[Runtime]:** Apple Container DNS support for Hub connectivity — when using the `container` runtime, `EnsureAppleDNS` creates a DNS rule so agent containers can resolve the Hub API endpoint. Onboarding UI triggers setup automatically when Apple Container is selected (#528).
* **[Harness]:** Hermes Agent harness bundle — complete scaffold with API key auth (Anthropic > OpenAI > Google AI Studio precedence), instruction projection into `AGENTS.md`, MCP server config, model alias resolution, capture-auth for no-auth flow, and 16 provision tests (#519).
* **[Build]:** Google Cloud Build support for harness-config image builds — new `CloudBuildHarnessConfigExecutor` uploads build context to GCS, submits multi-arch builds (`linux/amd64,linux/arm64`), and streams logs. CLI gains `--builder cloud-build` flag. System status API exposes Cloud Build availability for the frontend (#521).
* **[Auth]:** Decoupled auth pipeline from harness-specific Go code (Phases 1-2) — harness-config hydration now runs during the env-gather pre-check so config-driven auth metadata is available before the broker requests env vars. Generic `EnvVars` map on `AuthConfig` replaces hardcoded paths; the Copilot harness's GitHub tokens now flow through config alone (#516).
* **[Agent]:** Retry with exponential backoff and two-layer TTL cache for the GitHub skill resolver (#525).
* **[Web]:** Moved Capture Auth button to the terminal page for easier access (#520).

### 🐛 Fixes
* **[Hub]:** Admin role re-evaluated on every login and token refresh — previously only set at user creation, so config changes to admin emails had no effect on existing users (#530).
* **[Discord]:** Require thread ID for forum channels to prevent broadcast — messages to forum-type channels without a thread ID are now rejected with an actionable error instead of broadcasting to all threads (#522).
* **[Telegram]:** Restore backtick code spans stripped by Telegram's entity parser — the broker now reconstructs inline code and code blocks from `code`/`pre` entities, and `stripMentions` preserves whitespace and indentation (#518).
* **[Chat]:** Propagate hub errors (403, 404, 500) back to chat channels with user-facing messages instead of swallowing them silently. Discord broker pre-validates target agents against the cached agent list to catch deleted-agent routing immediately (#517).
* **[Runtime]:** Replace bind-mount secret staging with env-var pipeline for stateless brokers, fixing credential delivery when the filesystem is read-only (#523).
* **[Message]:** Enforce 2000-character limit on messages with an actionable error for oversized payloads (#524).
* **[Web]:** Detect incomplete embedded assets and serve a helpful error page instead of a blank screen (#526).

### 🔧 Chores
* **[CI]:** Added `handlers_projects_core.go` and `handlers_runtime_brokers.go` to the compat-literals allowlist for legitimate legacy grove literals (#527).

## Jun 27, 2026

A single targeted fix: GitHub URL parsing now correctly handles branch names containing slashes when resolving remote template references.

### 🐛 Fixes
* **[Config]:** Handle branch names with slashes in GitHub URL parsing — `resolveGitHubRef` now uses `git ls-remote --heads` to disambiguate branch vs path segments, picking the longest matching ref. Previously, branches like `feature/foo` were incorrectly split at the first slash, misidentifying part of the branch name as a file path (#503).

## Jun 26, 2026

A harness-heavy day: the Codex harness received critical auth fixes for file-based credentials, a new GitHub Copilot CLI harness shipped end-to-end tested, OpenCode gained Vertex AI auth support, and several provisioning issues were resolved.

### 🚀 Features
* **[Harness]:** GitHub Copilot CLI harness bundle — complete harness with build integration, provisioner with resilient auth fallback to no-auth mode when hub-registered configs haven't staged auth keys yet, and env var fallback for tokens in container environment (#506).
* **[OpenCode]:** Vertex AI auth support — autodetects GCP project + location env vars and writes `VERTEXAI_PROJECT`/`VERTEX_LOCATION` to `outputs/env.json`. Lowest priority fallback after api-key and auth-file.
* **[Harness]:** Stage `required_files` as secrets instead of bind-mounting — reads file content on host and stages as 0600 secret files under `agent_home/.scion/harness/secrets/`, fixing read-only filesystem crashes when harnesses try to write to credential files (#498).

### 🐛 Fixes
* **[Codex]:** Write fresh writable `auth.json` from staged secret in auth-file mode, fixing `lchown: read-only file system` crash at Codex startup (#501).
* **[Codex]:** Validate `CODEX_AUTH` secret is valid JSON before writing to `~/.codex/auth.json`, surfacing clear errors at provisioning time instead of opaque startup failures (#499).
* **[Codex]:** Updated no-auth hint to suggest `codex login --device-auth` instead of generic message (#504).
* **[Hub]:** Registered missing `/api/v1/message-channels` route that was causing 404s for `--channel` flag in the CLI (#502).
* **[Sciontool]:** Prevent `__pycache__` creation during harness provision by setting `PYTHONDONTWRITEBYTECODE=1`, fixing `scion delete` permission errors from root-owned bytecache on bind-mounted agent home (#505).
* **[Web]:** Move Name field above Workspace Type in new project form (#507).
* **[Web]:** Clean `dist` directory before build to prevent stale chunk accumulation (#508).

### 🔧 Chores
* **[CI]:** Fixed TypeScript errors in metrics-dashboard causing CI failure (#500).

## Jun 25, 2026

A major push on harness development — the Codex harness gained notification hooks, dialect YAML configuration, OTEL telemetry support, and template instructions. Antigravity was pinned to a specific release, briefly switched to ADC auth, then reverted. The Hub handlers were split by resource for maintainability.

### 🚀 Features
* **[Codex Harness]:** Notification hooks enabled — harness now fires lifecycle events and OTEL-escaped telemetry configuration for agent observability (#482, #488).
* **[Codex Harness]:** Dialect YAML configuration — maps model aliases and API conventions for the Codex harness (#484, #486).
* **[Codex Harness]:** Project template instructions — added instruction projection with hardened output and dropped unused system prompt file (#483).
* **[Codex Harness]:** Extended OTEL config support for richer telemetry integration (#494).
* **[Codex Harness]:** Updated model aliases (#481).
* **[Antigravity]:** Pinned CLI binary to v1.0.11 from GitHub Releases for build reproducibility, with `TARGETARCH` mapping for multi-platform support (#487).
* **[Sciontool]:** Hook support for bundled dialect overrides, allowing harness-specific model mapping to be shipped with the harness config (#485, #489).

### 🐛 Fixes
* **[Antigravity]:** Added missing field extractions (`session_id` from `.conversationId`, `tool_input` from `.toolCall.args`) and removed false `tool_name` extraction from `PostToolUse`. Declared `max_model_calls` as supported capability (#490).
* **[Antigravity]:** Disabled ADC auth for vertex-ai (USE_ADC not yet functional in AGY CLI) and reverted to requiring `AGY_TOKEN` with keyring injection. GCP location fallback and v1.0.11 pin preserved (#497).
* **[Codex Harness]:** Write instructions under container home directory (#491).
* **[Codex Harness]:** Make notify hook executable (#492).

### 🔧 Chores
* **[Hub]:** Split monolithic handlers.go by resource type for maintainability (#480).
* **[Docs]:** Changelog updates merged (#changelog).

## Jun 23, 2026

A light day with fixes to metrics reporting and capture-auth error handling.

### 🐛 Fixes
* **[Metrics]:** Fixed 7 review findings — handle all `KeyValue` types in `attrSetKey` (bool, double, zero int, empty string), simplify duration alignment to use `Truncate`, remove raw error messages from `X-Metrics-Warning` HTTP headers, and move chart rendering side-effects from `render()` to `updated()` lifecycle.
* **[Capture Auth]:** Treat "already exists" as successful capture — previously returned `(False, None)` causing the main loop to trigger a keyring fallback that also failed on the same condition (#479).

## Jun 22, 2026

Skill publishing was simplified with a new multipart upload path replacing the 3-step signed-URL flow, and Antigravity auth received several fixes for token format handling and keyring capture.

### 🚀 Features
* **[Skill Bank]:** Replaced signed-URL upload with multipart POST for skill version publishing — server now handles hash computation, storage upload, and publishing atomically in a single request. Added `DeleteSkillVersion` store method for cleaning up failed draft uploads. Web UI simplified by removing client-side SHA-256, concurrent upload semaphore, per-file retry logic, and finalize step. Skill create page gained a "Publish first version" toggle for combined create+publish flow (#474).

### 🐛 Fixes
* **[Antigravity]:** Accept nested AGY token format with `auth_method` envelope — validation now handles both flat and nested `{"token": {...}, "auth_method": "..."}` layouts (#471).
* **[Antigravity]:** Capture token from gnome-keyring when AGY doesn't persist the file — `capture_auth.py` falls back to `secret-tool lookup` via saved DBUS session address (#477).
* **[Antigravity]:** Deduplicate capture-auth entries when the same secret key appears in multiple auth types, and treat "already exists" errors as silent skips (#478).
* **[Skill Bank — M5]:** Code review fixes — `cmd.Context()` for Ctrl+C support, case-insensitive URI scheme detection (RFC 3986), pointer fields for partial updates, store constants instead of hardcoded strings, fix for clearing pinned hashes (#470).

## Jun 21, 2026

Antigravity auth was iterated on significantly — gnome-keyring was restored after the previous removal broke flows, token paths were fixed for bind-mounted secrets, and GCP settings detection was patched. A lifecycle hooks integration with Google Cloud Agent Registry was demonstrated, and the build system learned to use config.yaml image names.

### 🚀 Features
* **[Antigravity]:** Restored gnome-keyring to the provision flow — AGY requires keyring initialization before writing the OAuth token file, so keyring packages, DBUS initialization, and `secret-tool` injection were added back while keeping the `AGY_TOKEN` rename (#461).
* **[Lifecycle Hooks]:** Added `PROJECT_SLUG` as a trusted lifecycle hook variable and demonstrated Google Cloud Agent Registry integration — hooks POST an A2A agent card on agent start and DELETE the registration on stop. Includes integration test and docs example. Also fixed `VerificationStatus` derivation in the ent adapter (#demo).
* **[Skills]:** Hand-tuned team-builder skill content.

### 🐛 Fixes
* **[Antigravity]:** Read `AGY_TOKEN` from bind-mounted target path (`~/.gemini/antigravity-cli/antigravity-oauth-token`) instead of the env secret staging directory, fixing token detection in `_select_auth_method`, `_provision`, and `agy-wrapper.sh` (#465, #469).
* **[Antigravity]:** Patch GCP settings block for oauth-token agents when `GOOGLE_CLOUD_PROJECT` is present in the container environment, fixing the gate that only triggered on the enterprise marker file or `AGY_USE_GCP` env var (#462).
* **[Build]:** Use `config.yaml` image field to determine output image name instead of always using the harness-config CLI argument, ensuring the built image matches the intended name from the config (#463, #464, #468).

## Jun 20, 2026

The Antigravity harness was simplified by dropping gnome-keyring in favor of file-based OAuth, the skill creation UX was overhauled with a combined create+publish flow, and the metrics pipeline gained critical fixes for GCP project resolution and hook metric routing.

### 🚀 Features
* **[Antigravity]:** Removed gnome-keyring dependency and switched to file-based OAuth token placement — the token is written directly to `~/.gemini/antigravity-cli/antigravity-oauth-token`, eliminating DBUS/keyring daemon complexity. Dockerfile drops 4 packages (#460).
* **[Antigravity]:** Use official install script for CLI installation (#453).
* **[Skills — Create UX P1]:** Combined create+publish flow on the skill creation page — SKILL.md textarea with auto-populated fields from YAML frontmatter (debounced parsing, manual edit tracking), drag-and-drop file upload with signed-URL pattern and SHA-256 hashing, inline multi-step progress view, per-file status indicators with retry support, and file validation (50 files max, 10MB/file, 50MB total) (#455).
* **[Config]:** Added `name` field to harness-config `config.yaml` so config authors can declare the intended name independently of the directory name. Resolution priority: CLI flag > config.yaml > harness field > URL-derived. Includes path traversal validation and JSON schema pattern constraint (#456).
* **[Web]:** "Capture Auth" button on agent detail page for no-auth running agents — calls `capture_auth.py` inside the container with exit code handling (#459).
* **[Metrics]:** Diagnostic logging when the telemetry pipeline receives data without a cloud exporter (sync.Once warning on first invocation). GCP project ID resolution from metadata server as fallback, fixing the pipeline on instances with working metadata but no explicit credentials. Hook metrics now route through the pipeline OTLP receiver instead of creating per-invocation exporters, preventing Cloud Monitoring sampling rate violations (#458).
* **[Metrics]:** Registered `ModelResponse` hook to enable token metrics from Claude Code (#458).

### 🐛 Fixes
* **[Antigravity]:** Use `TARGETARCH` Docker build arg instead of `uname -m` for correct multi-platform support under cross-compilation (#457).

## Jun 19, 2026

Harness config management gained delete and image status UI, agent logs got a broker-based fallback, and skill version publishing became idempotent for interrupted drafts.

### 🚀 Features
* **[Web]:** Harness-config detail page improvements — delete button with confirmation dialog, image status section showing image path (local vs remote) and last update time, and `HarnessConfigData` type to surface `config.image` from the API. Agent detail logs tab now always visible, falling back to broker-based `/api/v1/agents/{id}/logs` when Cloud Logging is not configured (#452).

### 🐛 Fixes
* **[Hub]:** Made skill version publish idempotent for draft versions — retrying after an interrupted upload/finalize now returns the existing draft with fresh upload URLs instead of a 409 Conflict. Published/deprecated/archived versions still reject duplicates (#451).
* **[Hub]:** Added missing user-scope authorization check in `deleteHarnessConfig` and defaulted `deleteFiles` checkbox to unchecked so users must opt in to file deletion (#452).

## Jun 18, 2026

A productive day spanning the harness config lifecycle, template import UX, agent visualization, and build infrastructure. The harness journey P1 landed with source URL tracking and reimport flows, while template imports gained a discovery/selection dialog.

### 🚀 Features
* **[Harness Config — Journey P1]:** Added `source_url` field to track import origin for harness configs and templates. New reimport endpoint (`POST /reimport`) re-imports from stored or overridden source URL. CLI `scion harness-config update` command with `--url` and `--all` flags. Web UI shows source URL as clickable link with a "Refresh from Source" button (#447).
* **[Templates]:** Template discovery and selection dialog for bulk imports — discover endpoints scan for available resources without importing, and when multiple templates are found, a checkbox dialog lets users choose which to import. Single-template sources import directly (#437).
* **[Agent Viz]:** Customizable agent colors via color picker (persisted in localStorage), replay button for seek-to-start, sender-colored comms cards with smarter collapsed summaries that understand structured JSON payloads (#436).
* **[Build]:** Sync built image reference back to Hub after `scion build` — auto-syncs the updated `config.yaml` to Hub with recalculated file manifest and content hash, so the locally-built image is actually used at agent start (#444).
* **[Server]:** Startup warning when the server binary is built without embedded web assets, with a self-contained HTML page served from the static asset handler (#445).

### 🐛 Fixes
* **[Harness]:** Pass Hub-hydrated harness-config path to `harness.Resolve` in both run and provisioning paths — previously Hub-managed configs hydrated into temp directories were invisible, causing fallback to `Generic{}` harness with an empty shell command (#450).
* **[Templates]:** Import progress streaming now works on per-project endpoints (NDJSON support), and single-template imports are correctly scoped to the discovered resource (#443).
* **[Messaging]:** Tightened web channel default to actual web clients only (not CLI or API callers) (#448).
* **[Build]:** Use default Docker builder for local builds instead of custom container builder, fixing intermediate image resolution; use `BUILDX_BUILDER` env var to avoid mutating global Docker config (#442).

### 🔧 Chores
* **[Deps]:** Bumped dompurify (→3.4.9, →3.4.11), js-yaml (→4.2.0), vite (→6.4.3), rclone (→1.74.3), astro (→6.4.8) (#430, #431, #432, #438, #439, #440, #441).

## Jun 17, 2026

A targeted fix for message display in the agent detail view, improving both data completeness and access control.

### 🐛 Fixes
* **[Messaging]:** Fixed message display in agent detail view — web UI messages now default to `channel: "web"` and persist `channel`/`threadID` fields on message records. Agent managers (owners, project admins, global admins) can now see all messages including those from chat integrations, while other users only see messages where they are a participant (#435).

## Jun 15, 2026

A settings loading fix resolved a split-brain bug for git projects, the Skill Bank web UI received QA polish, and agent-viz gained Markdown rendering for inter-agent messages.

### 🚀 Features
* **[Agent Viz]:** Render inter-agent comms transcript as Markdown with per-message collapse — messages expand from a one-line plain-text summary to full formatted Markdown on click. Includes security hardening (HTML escaping before `marked.parse()`), lazy parsing for performance, and 1,000-char summary truncation (#427).

### 🐛 Fixes
* **[Config]:** Fixed split-brain settings loading for git projects with `project-id` — in-repo `.scion/settings.yaml` was silently skipped when split storage was configured, causing global settings to override project-level settings. The merge chain is now: defaults → global → in-repo → external → env. Also fixed `scion config dir` to show the effective config directory and added warnings for profiles/runtimes in in-repo settings.
* **[Web — Skill Bank]:** QA fixes — added `storage.googleapis.com` to CSP `connect-src` for GCS uploads, fixed upload retry to use `/upload` endpoint, changed registry create default trust level from `trusted` to `pinned`, fixed case-sensitive `SKILL.md` validation, hid `scopeId` for user-scoped skills, and defaulted version to `1.0.0` (#429).
* **[Runtime]:** Fixed Apple container list JSON parsing for new status format.
* **[Build]:** Split `make all` and `make install` targets so `sudo` doesn't need `go`/`npm` in PATH — workflow is now `make all && sudo make install`.

### 🔧 Chores
* **[Docs]:** Changelog entries for June 10-14 merged to main (#433).

## Jun 14, 2026

The Skill Bank gained a full web UI, the Discord bot received significant fixes, and harness provisioning was hardened for no-auth mode.

### 🚀 Features
* **[Web — Skill Bank UI]:** Complete web interface for skill management — list page with search/scope filtering, detail page with version history and metadata, create page with scaffolding, and a publish dialog for uploading skills to the Hub. Admin pages for skill registry management (list, detail, CRUD). Added 4,200+ lines of Lit components across 7 new pages (#423).
* **[Web]:** Auto-link chat accounts when registration link includes `?code=` parameter — shows a clean "Linking... → Success!" flow instead of the manual code-entry form (#426).

### 🐛 Fixes
* **[Discord]:** Multiple fixes to the Discord bot — improved broker message handling, command routing, send queue reliability, and webhook delivery (422 lines changed across broker, commands, sendqueue, and webhooks) (#428).
* **[Harness]:** Handle no-auth mode in container-script provisioners, preventing auth-related failures when agents are configured without authentication (#424).
* **[Build]:** Added missing `!no_sqlite` build tags to test files depending on `messagebroker_test.go`, fixing `go vet -tags no_sqlite` failures (#264).

### 🔧 Chores
* **[Style]:** Minor `gofmt` formatting fixes (#264).

## Jun 13, 2026

A2A multi-turn conversations shipped as a series of commits, transforming the bridge from single-turn MVP to full multi-turn lifecycle support. Alongside that, secrets handling was fixed for double-encoding, the project detail agent list gained filtering, and several CI/build issues were resolved.

### 🚀 Features
* **[A2A Bridge — Multi-Turn Lifecycle]:** Tasks no longer auto-close on the first content message. Content messages are now broadcast with `state=working` and `Final=false`, keeping the task alive. Task lifecycle is driven solely by agent state-change messages: `working/thinking/executing` → working, `waiting_for_input` → input-required, `completed/error/stalled` → terminal states. This enables agents to ask clarifying questions, send progress updates, and emit interim artifacts before completing.
* **[A2A Bridge — Follow-Up Messages]:** `message/send` with a `taskID` routes to the existing agent, continuing the conversation. Verifies task ownership, rejects terminal-state tasks, and works with both blocking and non-blocking modes.
* **[A2A Bridge — Capability Advertisement]:** Agent cards now advertise `streaming=true` and `pushNotifications=true`, reflecting the implemented multi-turn support.
* **[Hub]:** Restored harness-config build UI and executor that was accidentally removed by PR #412 — includes the build image button, dialog, log streaming, and seeded operation (#420).
* **[UI]:** Added filter and sort controls to the project detail agent list (#414).

### 🐛 Fixes
* **[Secrets]:** Fixed secret API base64 handling — store decoded plaintext instead of base64-encoded values, preventing double-encoding when secrets are injected as environment variables. Added 128KB `MaxBytesReader` limits and frontend-side base64 encoding via `TextEncoder` (#418).
* **[A2A Bridge]:** Preserve `input-required` state on content messages instead of unconditionally resetting to `working`. Use `projectcompat` topic helpers instead of hardcoded patterns. Added `TouchTask` store method for timestamp refresh (#421).
* **[Ent]:** Regenerated ent client to remove stale `discordpendinglink` import (#419).
* **[Auth]:** Stricter email validation using `net/mail.ParseAddress` (#411).
* **[Hub]:** Fixed duplicate `no_auth` keys and missing field schema attribute (#412).
* **[Skill Bank]:** Fixed SQLite pin compatibility and skill name validation (#415).

### 🔒 Security
* **[Runtime]:** Protected metadata server shutdown endpoint from unauthorized access (#422).

### 🔧 Chores
* **[CI]:** Added esbuild as explicit dev dependency for Vite 7 compatibility (#416).
* **[Build]:** Bumped esbuild and vite (→ v8.0.16) in web frontend (#413).
* **[Style]:** Applied `gofmt` to all unformatted Go source files (#417).

## Jun 12, 2026

Two major feature PRs landed: Skill Bank M5 adds federated skill resolution across GitHub, GCP Vertex AI, and external registries, while the messaging overhaul hardens error contracts, delivery feedback, and agent wake semantics across all channels.

### 🚀 Features
* **[Skill Bank — M5a: Routing Resolver]:** `RoutingSkillResolver` dispatches skill references by URI scheme to registered resolvers (`skill://`, `gh://`, `gcp-skill://`, full GitHub URLs), with the hub resolver as fallback. Wired at CLI and broker call sites, wrapped by the caching resolver (#408).
* **[Skill Bank — M5b: GitHub Resolver]:** `GitHubSkillResolver` resolves `gh://` URIs and full GitHub URLs via the GitHub Contents API, with input sanitization and response size limits (#408).
* **[Skill Bank — M5c: Federation & Registries]:** External skill registry management with CRUD admin API, federation proxy for cross-registry resolution, and trust enforcement (trusted pass-through or pinned hash verification). CLI commands under `scion skills registries`. Security hardening: 10MB body size limit, redirect-following disabled to prevent credential leakage, reusable HTTP client (#408).
* **[Skill Bank — M5d: GCP Vertex AI Resolver]:** `gcp-skill://` URI resolution via Vertex AI Skills API using Application Default Credentials. Version validation, SSRF defense (HTTPS-only, same-host download URLs, no link-local/RFC1918 targets), and 1MB metadata response limit (#408).
* **[Messaging — Error Contracts]:** Non-existent agent targets now return proper errors instead of creating orphan message rows. Scheduled events targeting deleted agents are marked as failed. Hub API 404 responses include agent slug and project context (#409).
* **[Messaging — Delivery Feedback]:** Persistence failures return 500 (was silent 200), missing recipients return 400 (removed silent creator fallback), broker dispatch failures return 502. Successful sends include `message_id`, `status`, `recipient`, and `recipient_id` in the response (#409).
* **[Messaging — Agent Phase Pre-Check]:** `handleAgentMessage` now returns 409 Conflict for non-running agents with actionable guidance (suspended: use `--wake`, stopped/error: use `scion start`) (#409).
* **[Messaging — Wake Improvements]:** Wake timeout bumped from 15s to 30s matching broker retry deadline. Distinct error for wake-success-delivery-failure. Messages to suspended agents without `--wake` now rejected with clear error (#409).
* **[Messaging — Integration Feedback]:** Telegram plugin validates default agents before routing, reports Hub delivery errors back to originating chat with error cooldown (max 1 per 5min per chat+thread+error-type) and remediation suggestions (#409).

### 🐛 Fixes
* **[Build]:** Corrected `runId` JSON key mismatch in build polling that caused polling to silently fail (#410).

### 🔧 Chores
* **[Docs]:** Added Observability section to glossary clarifying infrastructure metrics (`scion.hub.*`, `scion.db.*`) vs agent metrics (`gen_ai.*`, `agent.*`) and the telemetry pipeline (#407).

## Jun 11, 2026

The skill bank feature landed in a single massive PR — a complete registry, provisioning, and caching system for reusable agent skills. This is the largest single change in the project's history at 18,000+ lines spanning milestones M1 through M6.

### 🚀 Features
* **[Skill Bank — M1: Agent-Side Provisioning]:** Added `SkillReference` type with URI parser supporting full, shorthand, alias, and bare name forms. Integrated skill resolution into the agent provisioning pipeline with fail-closed safety (S1): required skills without a resolver cause provisioning failure. Skills are installed via staging with per-file SHA-256 hash verification (S2), path safety validation (S3), and atomic rename (#399).
* **[Skill Bank — M2: Hub API & Storage]:** Full CRUD API for skills and versions — create, list, get, update, delete, publish with signed URL upload workflow. Batch resolve endpoint with scope search order (user > project > global > core) and semver constraint matching (exact, `^`, `~`, `>=`, `latest`, `sha256:` content-addressed). Ent schemas for `Skill` and `SkillVersion` entities with comprehensive store tests (#399).
* **[Skill Bank — M2: CLI]:** `scion skills` command group with `list`, `show`, `create` (scaffold), `publish` (upload + finalize), `delete`, `versions`, and `resolve` subcommands. Includes `scion skill` singular alias and `--format json` support (#399).
* **[Skill Bank — M3: Hub Resolver Wiring]:** `HubSkillResolver` adapter bridging `hubclient.SkillService` to the agent `SkillResolver` interface, injected at both CLI and broker call sites with identity propagation (#399).
* **[Skill Bank — M4: Broker-Side Caching]:** Content-hash-keyed caching for resolved skills using the existing `templatecache.Cache` infrastructure (500MB default, `~/.scion/cache/skills/`). Cache hits verified with SHA-256 on read; mismatches evict and re-download. Latest/range constraints always re-resolve against Hub but skip download on cache hit (#399).
* **[Skill Bank — M6: Discovery & Lifecycle]:** Tag filtering with AND semantics and case-insensitive matching. Deprecation workflow with version-level deprecation messages, optional replacement URIs, and warnings surfaced during resolution. Download counter with atomic per-version increment. Published versions preferred over deprecated in latest/constraint resolution (#399).

### 🔒 Security
* **[Skill Bank]:** ActionRead authorization checks added to all read/download/resolve endpoints. User scope authorization gap fixed in `createSkill`. Batch resolve item cap (max 50) to prevent abuse. Cache hit hash verification to detect corruption. HTTPS-only downloads (S5) with cross-host redirect rejection (#399).

## Jun 10, 2026

A sweeping day of project compatibility refactoring, security hardening, and a major new Discord integration. The codebase-wide rename from "grove" to "project" landed across endpoints, config, runtime labels, and CI guardrails. The test-login endpoint was secured with challenge tokens, harness config hydration was fixed end-to-end, and a standalone Discord bot with gRPC support shipped.

### 🚀 Features
* **[Discord]:** Standalone Discord bot with gRPC for HA deployment — the Discord plugin can now run as an independent process communicating with the hub via gRPC, enabling horizontal scaling and high-availability setups. Includes a new `DiscordPendingLink` entity, gRPC broker adapter, and a dedicated Dockerfile (#395).
* **[Scheduler]:** Enabled scheduler write commands (`create-recurring`, `pause`, `resume`, `delete`) in agent mode, removing the `dispatch_` prefix gate that previously blocked agents from managing schedules (#378).
* **[Auth]:** Added challenge token authentication to the test-login endpoint — callers must present a short-lived JWT (5min TTL) signed with the hub's user signing key and scoped to the `scion-test-login` audience. Includes case-insensitive Bearer parsing and explicit `exp` claim enforcement (#382, #392).
* **[Compatibility]:** Project/grove compatibility boundary — added a comprehensive migration layer with legacy `/groves` route wrappers, request body field adapters, and centralized compatibility key mapping to support clients using the old naming while the codebase transitions (#380, #387, #388).

### 🐛 Fixes
* **[Hub]:** Fixed harness config hydration pipeline — `populateAgentConfig()` now stamps `HarnessConfigID`/`Hash` on agent records, `RemoteAgentConfig` threads these fields to the broker, and database errors during slug lookup are now logged instead of silently discarded (#389).
* **[UI]:** Fixed file badge size inconsistency (computed from filtered list instead of unfiltered total) and added missing settings page titles for harness-configs and templates detail routes (#390).
* **[UI]:** Applied agent list UI improvements to the project detail view (#383).
* **[Runtime]:** Fixed recursive glob in sparse checkout to include `home/` subdirectory by switching from `/*` to `/**` pattern (#381).
* **[Compatibility]:** Used canonical project endpoints across integrations, test fixtures, and mocks — replacing remaining `/groves` references (#384, #385).
* **[Compatibility]:** Centralized runtime project label compatibility, replacing scattered literal strings (#386).
* **[CI]:** Fixed compat-literals CI step failing due to missing ripgrep — installs `rg` in the runner and degrades gracefully when unavailable (#397).

### 🔧 Chores
* **[CI]:** Enforced project compatibility literal guardrail in GitHub Actions with a documented contributor rule (#391, #393).
* **[Docs]:** Changelog backfill for June 8-9 entries merged to main (#396).

## Jun 9, 2026

A lighter day focused on messaging improvements and fixing a hub import routing gap. Broker messages now support an interrupt prefix, Telegram formatting was fixed, and missing harness-config import routes were wired up.

### 🚀 Features
* **[Messaging]:** Support `!` prefix in broker messages as inline interrupt — messages from Telegram, webhooks, or direct channels that start with `!` are now delivered with urgent/interrupt semantics, equivalent to `--interrupt` on the CLI. Handles whitespace edge cases and defaults to "interrupt" for bare `!` messages (#375).

### 🐛 Fixes
* **[Messaging]:** Fixed literal `\n` sequences appearing in Telegram message formatting instead of actual newlines (#377).
* **[Hub]:** Registered missing harness-config import routes — the unified `/api/v1/resources/import` endpoint and the per-project `/api/v1/projects/{id}/import-harness-configs` endpoint were never wired up, causing 404 errors on the hub import screen. Added handlers, URL normalization, and proper error code constants (#376).

## Jun 8, 2026

This release strengthens the agent state and container lifecycle: agents can now be suspended and resumed with their harness session intact, crashes are surfaced as a restartable `error` state, and stalled agents are auto-suspended to reclaim resources.

### 🚀 Features
* **Suspend & Resume with Session Continuation:** `scion suspend <agent>` (and `--all`) now tears down an agent's container while preserving the intent to resume. Resuming — or simply running `scion start` on a suspended agent — *continues* the prior harness conversation (Claude Code via `--continue`, Gemini CLI via `--resume`) instead of starting fresh. Suspend is available for harnesses that support session resume and is also exposed in the Web Dashboard's lifecycle controls. See [Agent Lifecycle](/scion/local/agent-lifecycle/).
* **Auto-Suspend of Stalled Agents:** The Hub now automatically suspends agents that remain `stalled` past a grace period (~10 minutes of inactivity), reclaiming their containers. Such agents resume automatically on the next message, as long as their harness supports resume and the container is still alive.

### 🐛 Fixes
* **Crash → Restartable `error` State:** Agents that exit non-zero (a genuine crash, OOM, or `SIGKILL`) now transition to the `error` phase with a descriptive message like `Agent crashed with exit code N`, distinct from a clean `stopped` exit or a `limits_exceeded` stop. The `error` phase is restartable — `scion start` clears it and launches a fresh session. (A graceful `stop` sends `SIGTERM`, which harnesses handle cleanly, so stopping never leaves an agent in `error`.)

## Mar 17, 2026

This release introduces a major new GCP Identity implementation allowing agents to authenticate via metadata server emulation, alongside comprehensive new Project Settings and Agent Limits configurations in the UI.

### 🚀 Features
* **GCP Identity & Metadata Emulation:** Implemented end-to-end GCP identity assignment for agents using metadata server emulation and token brokering. This includes a new Web UI for Service Account management, iptables interception, per-agent rate limiting, audit logging, and telemetry metrics (consolidated from commits 2ac33bb, 961653a, d37a79c, d11318f, a5f457a, d187838, 8df2a04, 34c7056, 401a178, 52f6838).
* **Project Settings & Agent Limits:** Introduced a comprehensive Project Settings UI organized into General, Limits, and Resources tabs. Administrators can now configure default agent limits at both the hub and project levels, which automatically pre-populate when creating new agents (consolidated from commits c7d9585, aa5c2ff, 2ffdff8, 8f0263f, 0d87a17, 07714a1, 906a88d).
* **Workspace Content Previews:** Added content preview capabilities for workspace files directly within the UI (commit 53cea7c).
* **CLI Enhancements:** Added a `-r`/`--running` flag to the `scion list` command to easily filter for active agents (commit 7001035).

### 🐛 Fixes
* **Project & Membership Synchronization:** Resolved multiple issues with project linking and membership backfills, including fixing unique constraints on project IDs, ensuring proper legacy owner role assignments, and correctly including auto-provide brokers (consolidated from commits 4af2662, 307fb85, cb22a18, 79cc591, 1f6f16f, e14ec95).
* **Storage & ID Consistency:** Fixed global project ID bleed-through issues and unified agent split storage paths under `.scion/` for deterministic behavior across hub-managed and external projects. Ensured cascading cleanups of templates and configs when a project is deleted (consolidated from commits fea4588, 6bb2348, a97ebd7, 023a089, 6eaf8dc, 221c736, 75bfcc0, c9d8ddf).
* **GCP Validation & Logging:** Improved debug logging for 4xx errors and enhanced GCP Service Account validation messages, including returning capabilities in the list API response (consolidated from commits e060664, d65dc09).
* **Container Lifecycle Management:** Ensured agent containers are gracefully stopped before removal to prevent shared-directory mount errors (commit 8a0fabc).
* **Template Synchronization:** Fixed an issue where template synchronization was blocked by setting a default image for the generic harness config (commit 816c960).
* **Web UI Consistency:** Fixed layout issues such as status column widths in agent tables and exposed Scion version information on the admin config page (consolidated from commits 53f55b5, 7536c59).

## Mar 16, 2026

This release focuses on a major overhaul of user group and membership management with new authorization rules and UI enhancements, alongside significant improvements to the OpenTelemetry metrics pipeline.

### 🚀 Features
* **Group & Membership Management:** Overhauled the group management system by introducing human-friendly member editing, user search autocomplete in the add dialog, and strict enforcement of group ownership and authorization rules (consolidated from commits 1ae6d03, 454c80e, 5e32c9e, c2fa624).
* **Telemetry & Metrics Pipeline:** Enhanced the observability pipeline by exporting OTLP metrics through GCP, restoring Gemini token metric hooks, and covering Gemini native OTEL metrics (consolidated from commits 721da2b, 5e752f8, 28a9877, 4321775).

### 🐛 Fixes
* **Group Constraint Fixes:** Resolved multiple backend issues related to group creation and loading, including fixing dev-user UUID mapping for workstation mode, backfilling project member groups, and ensuring SQLite constraint compatibility (consolidated from commits 1993892, 4628e5f, e5c1eba, 6a4f843, 1a779c8, cb7c932).
* **Agent Lifecycle:** Implemented proper agent resume and restart dispatch logic on the hub (commit 30a1b74).
* **Project Synchronization:** Fixed an issue with re-linking stale hub projects by ensuring the project ID is regenerated from the marker file, and updated the UI to conditionally show the branch field only for git-based projects (consolidated from commits 39e0025, 2bab781).
* **Container Prune Operations:** Fixed the container runtime image pruning by removing the unsupported `-f` flag (commit ad9f486).
* **Environment & Security:** Improved GCE certificate checks (commit 8904e76).

## Mar 15, 2026

This release significantly hardens the agent workspace provisioning process for Hub-linked environments, introduces interactive terminal toolbar toggles for the web UI, and improves overall deterministic Project ID generation and management.

### 🚀 Features
* **Hub-Linked Workspace Provisioning:** Transitioned hub-linked projects to strictly use a robust `git init` + `git fetch` strategy instead of standard cloning or local worktrees. This allows provisioning into workspaces that already contain `.scion` metadata or `.scion-volumes` directories while properly clearing out stale artifacts before initialization (consolidated from commits 3852e6a, 51be1e6, 118b518, 2f6b877, 2f59410, 4497a86, 6cc4487).
* **Terminal Toolbar Enhancements:** Added web toolbar toggles for managing tmux windows (seamlessly switching between agent and shell) and controlling mouse/clipboard behavior. Also fixed mouse-drag text selection and improved the robustness of the window controls using direct tmux key bindings (consolidated from commits 8c07a48, b83ba8c, 8027407, 6140111).
* **Deterministic Project ID & Synchronization:** Enhanced deterministic Project ID generation during hub-link sync. Git URL user info is now cleanly normalized to ensure UUID v5 project IDs match regardless of the protocol (e.g., `https://` vs `git@`), and stale project links are automatically detected and synchronized (consolidated from commits 6a52952, 1bfa95d, ed2b2c5).
* **Template Sync Improvements:** Enabled the template sync command to update existing templates without requiring the `--force` flag. Local templates now automatically sync on hub startup and intelligently bypass cache when running in co-located (hub-broker combo) mode (consolidated from commits ff14bd9, e0cf52d).
* **Web UI Flow & Performance:** Introduced route-based code splitting to significantly reduce the web bundle size. Additionally, refined the project settings page by adding a "Done" button, hiding unnecessary registration options for git-backed projects, and introducing a confirmation dialog when creating a project for an existing git repository (consolidated from commits 62e3e36, bd9f40e, b6d2afe, f715bf0).

### 🐛 Fixes
* **Project Deletion Cleanup:** Fixed an issue where environment variables, secrets, and harness configs were left orphaned; the system now performs a proper cascade delete when a project is removed (commit 834bae9).
* **Agent Path & Directory Routing:** Corrected the routing of agent directories for git-backed projects to correctly use the project-specific path instead of the global directory. Also properly resolved shared directory mount paths for git-based workspaces (consolidated from commits e38cf92, fbb9056).
* **Hub Unlinking & Local State:** Fixed a bug where the hub status erroneously showed "linked" after unlinking by verifying the local enabled state instead of mutating the global `project_id`. The provider's `localPath` is also now properly preserved when re-registering an existing project (consolidated from commits 0857553, 54fe4b8, 4db9253, d4828f1).
* **Agent Container Lifecycle:** Ensured that the agent container stops correctly and cleanly when the underlying agent process exits within the tmux session (commit 535ebbd).
* **Configuration & Path Resolution:** Fixed resolution logic for split-storage paths when writing project settings, and ensured `git check-ignore` runs from the repository root so that broker `.gitignore` checks function correctly (consolidated from commits 7a4cd3c, f934b9a).
* **Web Assets & UI Styling:** Restored dark mode logic for Shoelace form components after implementing code splitting, and fixed the serving of root-level public assets (like the notification icon) from the Go backend (consolidated from commits ca1343f, b3d9484).
* **Network & Harness Config:** Preserved the hub port when applying container bridge endpoint overrides, and returned a synthetic harness config for "generic" agents to unblock template synchronization (consolidated from commits ff1635d, 713c3ab).

## Mar 14, 2026

This release introduces the foundational infrastructure for the Scion plugin system, adds comprehensive support for syncing project-level templates, and unifies all Project IDs to a standard UUID format.

:::danger[BREAKING CHANGES]
* **Project ID Format Unification:** All Project IDs have been standardized to a unified UUID format. Git-backed projects now use a deterministic UUID v5 (based on the namespace and normalized URL) instead of a 16-character hex hash, while non-git and hub-managed projects continue using UUID v4. Existing git-backed projects may need to be re-linked, and any integrations relying on the old hex format must be updated (commit e896693).

:::

### 🚀 Features
* **Plugin System Infrastructure:** Introduced the core architecture for a new Scion plugin system using `hashicorp/go-plugin`, complete with reference implementations for message broker and agent harness plugins (consolidated from commits 6c543d0, b1a5ae1, 22991ec).
* **Project Template Sync & Management:** Implemented capabilities for syncing project-level templates with the Hub. This includes new API endpoints (`POST /api/v1/projects/{projectId}/sync-templates`), CLI commands (`scion templates sync --all`, `scion templates status`), and a dedicated Web UI for managing synced templates. Additionally, machine-specific settings for git-backed projects are now externalized, while templates remain in-repo to support version control (consolidated from commits d0507b1, 3c9cb4b, 0cf62d7, ef4f208, 56df5b4).
* **CLI Navigation Commands:** Added `config dir`, `cd-config`, and `cd-project` commands to simplify locating and navigating to configuration and workspace directories (commit 596295d).

### 🐛 Fixes
* **Agent Git Cloning:** Resolved an issue where git clones would hang indefinitely when authentication was required but no token was present. Added proper error state reporting upon clone failures, and corrected the `agent-info.json` path to correctly use the `scion` user's home directory (consolidated from commits 93dfdcd, 7ec5eb2).
* **Image Builds:** Fixed the Google Cloud SDK installation in the build environment by explicitly using `apt-get` (commit d76197c).

## Mar 13, 2026

This release focuses on improving agent specialization with harness skills, resolving critical routing and identification issues in multi-hub and linked git environments, and adding a new satellite service for documentation agents.

:::danger[BREAKING CHANGES]
* **Linked Git Project IDs:** Linked projects backed by a git remote now use deterministic 16-character hex hash IDs instead of the raw, normalized git URL. This resolves severe web routing and API path parsing issues caused by slashes in the URL. If you had existing linked projects, they may need to be re-linked, and any scripts relying on the raw git URL as the Project ID will need to be updated (commit 05e0c7a).

:::

### 🚀 Features
* **Harness Skills for Templates:** Implemented robust support for harness skills within agent templates. Skills defined in `harness-configs` and templates are now automatically merged and mounted into the appropriate harness-specific directory (e.g., `.claude/skills`, `.gemini/skills`) during agent provisioning (consolidated from commits efefc44, 2a086ac, 5b54c66).
* **Docs-Agent Satellite Service:** Introduced a new `docs-agent` satellite service to provide dedicated documentation capabilities alongside agent workflows (consolidated from commits 092ffde, 58f21c2, fd1b1e2).
* **Shared Directory Management UI:** Added web UI support for managing and viewing project shared directories (commit 7d7acfb).
* **Terminal & UX Enhancements:** Enabled tmux mouse mode by default for better terminal interactivity and introduced a custom Scion bell icon for browser notifications (commits c915da9, 343382e).

### 🐛 Fixes
* **Multi-Hub Routing & Dispatch:**
    * Resolved an issue where brokers connected to multiple hubs would route agents to the wrong local hub endpoint by correctly resolving the endpoint from the control channel connection header.
    * Enabled control-channel-only brokers to successfully dispatch agent operations (consolidated from commits dd5581f, 1bdc31d).
* **Agent Creation Context:**
    * Ensured project shared directories are properly passed from the hub to the broker during agent creation.
    * Fixed an issue where `agentDir` was omitted during harness provisioning and setting overlays (consolidated from commits a5cac3b, c550865).
* **Documentation & Web Hosting:** Corrected site base URLs, configured Astro for GitHub Pages deployment, fixed markdown links to use relative paths, and updated the README to point to the rendered site (consolidated from commits 35eee03, 8ca4a96, a7dc580, e133647, 2467d89).
* **Maintenance:** Internal refactoring analysis for `server.go` and documentation updates for recent feature releases (commits d3484d4, 33ee10e).

## Mar 12, 2026

This release focuses on enhancing persistent storage and system observability. It introduces **Project Shared Directories**, enabling agents within a project to share and persist mutable state via the filesystem (with native Kubernetes support). Additionally, the metrics pipeline has been significantly enriched with labels for harness type, model, and project ID, providing deeper insights into agent performance and costs.

### 🚀 Features
* **Project Shared Directories (Phase 1 & 2):** Introduced a persistent, mutable storage layer shared between agents within a single project.
    * Added support for both local filesystem storage and Kubernetes PersistentVolumeClaims (PVCs) with project-scoped lifecycle management.
    * New CLI commands added: `scion shared-dir list`, `create`, `remove`, and `info` for managing shared volumes.
    * Shared volumes can be mounted at standard paths (`/scion-volumes/<name>`) or within the workspace (`/workspace/.scion-volumes/<name>`) (consolidated from commits 838b1b9, a8d50f8, 8b860c0).
* **Enhanced Telemetry & Metrics Pipeline:** Major overhaul of the metrics pipeline for improved observability and aggregation.
    * Enriched OTel resource attributes with `scion.harness`, `scion.model`, `scion.broker`, and `project_id`.
    * Expanded Codex-specific telemetry to capture tool usage, tool input/output, and detailed token counts (input, output, cached).
    * Injected `SCION_HARNESS` and `SCION_MODEL` environment variables into agent containers to enable harness-aware telemetry (consolidated from commit 8246a76).

### 🐛 Fixes
* **Metrics & Telemetry Reliability:**
    * Resolved an issue where tool and API metrics were not recorded from unpaired end events.
    * Corrected the wiring of token and model metrics in the hook-to-OTel pipeline (consolidated from commits 2a64f02, 43f1bf0).
* **Agent Lifecycle & Configuration:**
    * Corrected an issue where custom branch names were not properly passed during the final environment setup path of agent creation (commit 46eee6d).
    * Updated the default model configuration for the Codex harness to `gpt-5.4` (commit fbfc950).
* **Maintenance:** Fixed broken documentation links in the repository README (commit 0f55876).

## Mar 11, 2026

This release focuses on improving agent lifecycle flexibility and enhancing the web-based terminal experience. It introduces support for targeting specific git branches during agent creation and provides better visibility into template versions, alongside critical fixes for runtime stability and authentication.

### 🚀 Features
* **Custom Branch Targeting:** Added a branch name field to the agent creation flow and enabled cloning of agent branches from origin. This allows users to direct agents to specific branches immediately upon creation, improving workflow flexibility (consolidated from commits 182c323, 2d50def, 11c36a8).
* **Web Terminal & Tmux Interactivity:** Introduced a tmux mouse toggle (via `C-b m`) and a toolbar button in the web terminal. This release also resolves persistent copy-paste issues in the web interface and adds comprehensive documentation for terminal options (consolidated from commits 9a41138, 9371859, 616250a).
* **Enhanced Template Traceability:** Updated the CLI and Web UI to display template IDs and hashes, providing clear visibility into the exact configuration version associated with each agent.

### 🐛 Fixes
* **Runtime & Broker Stability:**
    * **Podman Reliability:** Resolved an issue where Podman containers would fail to restart correctly from the Hub or Broker.
    * **Double-Daemonization:** Prevented the broker from double-daemonizing during start or restart operations.
* **Agent Attachment Reliability:** Added a readiness check for tmux sessions before attachment, ensuring more reliable connections when attaching to running agents.
* **Authentication & Secret Injection:** Corrected a bug where environment-type secrets were not properly injected into the execution environment during authentication resolution.
* **Project & Workspace Management:**
    * **Multi-Hub Compatibility:** Fixed a regression where git-based projects were incorrectly rejected in multi-hub environments.
    * **Cleanup & Resolution:** Improved hub-managed project path resolution during agent deletion and enhanced detection of orphaned project configurations.
* **Configuration & Compatibility:**
    * **Legacy Key Support:** Updated `config get` to support legacy v1 settings keys like `image_registry`.
    * **Fallback Logic:** Improved `env-gather` and harness configuration to correctly fall back to global settings when local context is missing.
* **Documentation & Polish:** Performed final pre-launch polish on philosophical documentation and refined the agent creation UX by defaulting runtime profiles to "Use broker default."

## Mar 10, 2026

This release focuses on streamlining system administration and enhancing visibility into agent operations. It introduces a comprehensive Web-based server configuration editor and a native runtime profile selector for agent creation, alongside critical improvements to telemetry reliability and Hub connectivity.

### 🚀 Features
* **Web Admin Server Configuration Editor:** Launched a full-featured settings editor at `/admin/server-config` (admin-only). This allows administrators to view and modify the global `settings.yaml` through the Web UI with support for tabbed navigation, sensitive field masking, and hot-reloading of key settings like log levels, telemetry defaults, and admin emails.
* **Runtime Profile Selector:** Added a dynamic profile selector to the agent creation form. After selecting a broker, users can now choose from the available runtime profiles defined on that broker, simplifying execution environment selection.
* **Standardized Issue & Feedback Templates:** Introduced official bug report and feature request templates to the repository to improve the quality and consistency of community contributions.

### 🐛 Fixes
* **Telemetry Configuration Reliability:** Corrected an issue where the telemetry opt-in checkbox on the agent configuration page wouldn't correctly reflect the global settings defaults.
* **Hub Connectivity Precision:** Enhanced agent startup logic to prioritize Hub-dispatched endpoints over local broker configuration, ensuring correct Hub communication in distributed and multi-hub environments.
* **Logging Observability & Traceability:**
    * **Agent Lifecycle Traceability:** Added `agent_id` to all broker-side agent lifecycle log events to improve cross-traceability and audit capabilities.
    * **Connectivity Debugging:** Stopped redacting `SCION_HUB_ENDPOINT` and `SCION_HUB_URL` in agent environment logs to facilitate easier debugging of connectivity issues.
* **Documentation & Licensing:** Restructured internal documentation for improved clarity, updated the installation guide, and completed the application of standard license headers across all source files.

## Mar 9, 2026

This release marks a significant milestone with the official transition of the project to the Google Cloud Platform organization, including a full module rename. It also introduces critical enhancements for agent autonomy with the enablement of the Scion CLI inside agent containers, alongside major improvements to administrative observability and real-time event reliability.

:::danger[BREAKING CHANGES]
* **Project Rebranding & Module Rename:** The Go module has been renamed from `github.com/ptone/scion-agent` to `github.com/GoogleCloudPlatform/scion`. All internal package imports and external references have been updated to reflect the transition to the Google Cloud Platform organization.

:::

### 🚀 Features
* **Autonomous In-Container CLI:** Enabled the Scion CLI within agent containers, providing agents with the ability to interact with the Hub API natively using their provisioned authenticated service context.
* **Admin User Activity Tracking:** Introduced "Last Seen" timestamps and sortable columns to the Admin Users dashboard to improve system administration and audit capabilities.
* **Enhanced Event Integrity:** Refined the Server-Sent Event (SSE) pipeline to ensure full agent snapshots are sent in `created` events, preventing incomplete UI states during high-concurrency creation.

### 🐛 Fixes
* **Log Query Precision:** Optimized agent log retrieval by filtering out internal HTTP request logs from the primary agent cloud logging view.
* **Infrastructure & Connectivity:**
    * Prioritized public Hub endpoints for production dispatches, reducing reliance on local network bridges.
    * Implemented defensive fallbacks for Hub environment variables within agent containers.
    * Resolved IAM role assignment issues for Hub service accounts.
* **UI/UX Consistency:**
    * Enforced name slugification across all CLI and Web input boundaries to prevent routing collisions.
    * Eliminated "white-flash" artifacts during OAuth redirects for users in dark mode.
    * Implemented automatic scrolling to error banners on form submissions.
    * Switched to SPA-native navigation for terminal back-links, improving navigation responsiveness.
* **System Stability:**
    * Resolved directory creation and path resolution bugs in split-storage (git-project) configurations.
    * Fixed `lstat` errors for non-existent project configuration files in containerized environments.
    * Corrected image registry resolution logic to prevent redundant prompts when already configured.
    * Resolved test failures across four critical categories on the main branch.
* **Harness Improvements:** Refined the Codex harness with improved configuration formatting and support for sandbox/bypass-approval flags.

## Mar 8, 2026

This release delivers a complete maturation of the Kubernetes runtime, introduces significant architectural enhancements for agent isolation and security, and drastically improves Web UI performance with optimistic updates and connection pooling.

:::danger[BREAKING CHANGES]
* **Kubernetes Mutagen Sync Removal:** Mutagen synchronization support has been entirely removed from the Kubernetes runtime in favor of native implementations as part of the Stage 1 Parity rollout.

:::

### 🚀 Features
* **Kubernetes Runtime Maturation (Stages 1-3):** Successfully implemented Parity, Production Hardening, and Launch Readiness for the Kubernetes runtime, establishing it as a fully-supported, robust platform for agent execution.
* **Agent Isolation & Project Security:** Enhanced agent security by externalizing non-git project data and agent home directories. Introduced tmpfs shadow mounts to definitively prevent agents from cross-accessing `.scion` configuration data or other agents' workspaces within the same project.
* **Web UI Performance & Responsiveness:** Drastically improved the frontend experience by implementing optimistic UI updates and background data refreshes. Re-architected the application shell to reuse components on navigation and consolidated Server-Sent Event (SSE) connections to prevent browser connection pool exhaustion.
* **Contextual Agent Instructions:** Added support for conditional instruction extensions (`agents-git.md` and `agents-hub.md`), allowing agents to receive tailored operational context based on their specific workspace type.
* **Hub API & Infrastructure:** Completed Phase 5 of the Hub API consolidation with full mode awareness and isolation. Enabled HTTP/2 cleartext (h2c) support on the web server, and introduced new project management CLI commands (`list`, `prune`, `reconnect`).
* **Agent Configuration & Execution:** Enabled `max_duration` limits universally across all harnesses, added a `--notify` flag to the CLI message command, and introduced a required `image_registry` prompt during workstation initialization.
* **Codex Harness Enhancements:** Stabilized the Codex integration with telemetry reconciliation, proper `auth.json` generation for API key workflows, and unified flag formatting.
* **UI Quality of Life:** Added a card/list view toggle to the project detail agent list and introduced a power-user shortcut (Alt/Option-click) to bypass delete confirmation dialogs globally.

### 🐛 Fixes
* **Hub/Broker Synchronization:** Resolved critical sync issues by tracking synced agents to correctly detect hub-side deletions, preventing deleted agents from being incorrectly re-proposed for registration.
* **Agent Lifecycle Cleanup:** Fixed cleanup routines to correctly stop agent containers before removing orphaned configs, and ensured broker-side files are meticulously cleaned if a hub dispatch fails.
* **Configuration & Auth Propagation:** Corrected the application order of `--harness-auth` before provisioning to prevent stale environment warnings, and ensured template telemetry configs are properly merged into the applied agent config.
* **Messaging Integrity:** Fixed a bug in `handleAgentMessage` to ensure structured messages are correctly constructed from plain text, and updated the messages tab query to include agent-sent communications.
* **Health & Security:** Exempted health check endpoints from broker auth middleware during strict mode enforcement to prevent false-positive failures in distributed deployments.

## Mar 7, 2026

This release marks a major leap in agent observability with the launch of the Cloud Log Viewer and structured messaging pipeline. It also introduces significant UI overhauls for agent management, enhanced GCP integration, and a new workstation-class daemon mode for the Scion server.

### 🚀 Features
* **Cloud Log Viewer & Structured Messaging (Phases 1-5):** Completed the end-to-end implementation of the Cloud Log Viewer and structured message pipeline. This includes a new Hub API for log retrieval, a dedicated "Messages" tab in the Web UI, and a multi-stage message broker adapter for reliable delivery and external notifications.
* **Agent Detail UI Overhaul:** Re-architected the agent detail page into a high-density tabbed layout featuring dedicated "Status", "Configuration", and "Messages" tabs. Added a new telemetry configuration card, breadcrumb navigation improvements, and a back button for the configuration flow.
* **Workstation & Daemon Management:** Introduced a workstation-optimized "daemon" mode for `scion server`. This allows the server to run as a persistent background process with integrated lifecycle management, simplified configuration, and automated combined-server detection for local brokers.
* **GCP & Metrics Integration:** Enhanced Google Cloud visibility with a native Cloud Monitoring exporter, trace-log correlation across logging pipelines, and automated injection of `SCION_PROJECT_ID` and GCP labels (agent/project) into all log streams.
* **Image Management & Build Automation:** Consolidated image build scripts and introduced support for custom `image_registry` settings. Added GitHub Actions workflows for automated building and delivery of Scion harness images.
* **Security & Authorization Hardening:** Strengthened the security posture by enforcing per-agent authorization for workspace routes, mandatory read authorization for all resource endpoints, and nonce-based HMAC validation for broker communication.
* **First-Run Experience:** Added a new `scion install` command and a streamlined first-run experience to simplify initial project setup and dependency verification.
* **Bulk Operations:** Added a "Stop All" button to the Web UI for efficient bulk shutdown of all agents within a project.
* **Harness Capability Gating:** Introduced capability-based gating for advanced agent configuration, ensuring only supported features are exposed based on the selected harness.

### 🐛 Fixes
* **UI Performance & Reliability:** Optimized the agent detail page by parallelizing API fetches and eliminating redundant data loads. Resolved rendering issues in the messages tab and added handling for null entries in message logs.
* **Auth & Environment Injection:** Fixed multiple issues with environment variable and profile injection, specifically resolving signing errors in combined-server mode and ensuring profile variables are applied before auth overlays.
* **Runtime & Broker Stability:** Improved Podman error handling and force-deletion reliability. Fixed a bug where `agent-limits.json` lacked correct permissions after creation and ensured `InlineConfig` is correctly propagated during agent restarts.
* **Logging Precision:** Established a dedicated HTTP request log stream using the standard `HttpRequest` format and removed misleading debug logs when running in GCP-native mode.
* **Build System:** Fixed a race condition in `make all` by ensuring web assets are fully built before the Go binary compilation begins.

## Mar 6, 2026

This release introduces Just-In-Time (JIT) agent configuration, an advanced agent creation interface, and native GCP telemetry integration, while centralizing profile management at the global level.

:::danger[BREAKING CHANGES]
* **Global Profile Management:** Runtime `profiles` and `runtimes` are no longer supported in project-level `settings.yaml`. These must now be managed exclusively at the global/broker level (`~/.scion/settings.yaml`). Existing project-specific profiles must be migrated to the global configuration.

:::

### 🚀 Features
* **Just-In-Time (JIT) Agent Configuration:** Completed Phases 1 & 2 of the inline agent configuration refactor. Agents now support dynamic, late-bound configuration overrides at runtime, enabling more flexible and adaptive agent behavior.
* **Advanced Agent Creation Form:** Launched a comprehensive advanced configuration interface in the Web UI. This allows for granular control over agent parameters, including model selection, resource limits, and specific harness settings during creation.
* **GCP-Native Telemetry Integration:** Introduced native support for Google Cloud Trace and Cloud Logging telemetry exporters. The system now automatically detects GCP credentials and configures the appropriate exporter mode, facilitating seamless observability in Google Cloud environments.
* **Enhanced Developer Workflow:** Improved the developer experience with automated mounting of the `sciontool` binary and a dedicated `SCION_DEV_BINARIES` directory, enabling rapid iteration and testing of local changes within agent containers.
* **Branding & UI Refresh:** Updated the application branding with a new seedling logo and favicon, and added detailed visibility of the resolved harness authentication method in the agent detail view.
* **Local Networking Automation:** Automated the computation of the `ContainerHubEndpoint` for Podman and Docker when running in combined hub-broker mode, simplifying local setup and networking.

### 🐛 Fixes
* **Telemetry & Auth Propagation:** Resolved several issues where telemetry settings, harness authentication, and configuration overrides were not consistently propagated through all broker and agent startup paths.
* **Agent Lifecycle Stability:** Fixed a bug where provisioning agents were not correctly cleaned up after an aborted environment-gathering session.
* **Claude Harness Authentication:** Corrected Vertex AI authentication detection for the Claude harness when using file-based secrets.
* **Data Integrity:** Fixed a bug in the advanced agent creation form where the applied configuration was not correctly populated with resolved values.

## Mar 5, 2026

This release introduces a major overhaul of the agent authentication pipeline, automated token refresh, and critical stability fixes for container removal and terminal reliability.

:::danger[BREAKING CHANGES]
* **Credential Key Migration:** The internal secret key `OAUTH_CREDS` has been renamed to `GEMINI_OAUTH_CREDS`. Users must migrate existing secrets to this new key to maintain Gemini harness functionality.
* **Harness Auth Refactor:** Legacy harness-specific authentication methods have been retired in favor of a unified `ResolvedAuth` pipeline. Custom harness implementations or manual environment overrides may require updates to align with the new late-binding logic.

:::

### 🚀 Features
* **Unified Harness Authentication:** Completed a multi-phase refactor of the agent authentication pipeline. Agents now support a variety of resolved auth types (API Key, Vertex AI, ADC, OAuth) with late-binding overrides available via the CLI (`--harness-auth`) and the agent creation form.
* **Agent Token Refresh:** Implemented an automated token refresh mechanism to ensure long-running agents maintain valid authorization throughout extended tasks.

### 🐛 Fixes
* **Apple-Container Stability:** Resolved critical hangs during container removal on macOS by implementing automated cleanup and blocking of problematic debug symlinks (e.g., `.claude_debug`).
* **Terminal UX & Reliability:** Improved error visibility by skipping terminal reset sequences on attachment failures.
* **Workspace & Git Integrity:** Hardened workspace file collection by skipping symlinks and ensured `git clone` operations correctly use the `scion` user when the broker runs as root.
* **Auth Precision & Validation:** Fixed several authentication regressions, including incorrect Vertex AI region projections, false API key requirements during environment gathering, and improper leakage of host settings into agent containers.

## Mar 4, 2026

This period focuses on the foundational implementation of the unified harness authentication pipeline and enhances infrastructure visibility within the Web UI.

:::danger[BREAKING CHANGES]
* **Harness Authentication Pipeline:** The implementation of the unified `ResolvedAuth` model (Phases 1-7) replaces legacy harness-specific authentication methods. While finalized in the Mar 5 release, the core architectural shift and retirement of legacy methods occurred in this period.

:::

### 🚀 Features
* **Unified Harness Authentication:** Completed a multi-phase refactor (Phases 1-7) of the agent authentication pipeline. Introduced centralized `AuthConfig` gathering, per-harness `ResolveAuth` logic, and a unified `ValidateAuth` phase, enabling more robust credential resolution across all harnesses.
* **Broker Visibility & Infrastructure Metadata:** Enhanced the Web UI to display runtime broker information on agent cards, project detail pages, and agent detail headers, providing clearer insight into distributed execution.
* **Default Notification Triggers:** Expanded the notification system to include `stalled` and `error` as default trigger states, improving proactive monitoring of agent health.

### 🐛 Fixes
* **Workspace Permissions:** Hardened the workspace provisioning flow by ensuring `git clone` operations run as the `scion` user when the broker is executing as root.
* **UI Navigation & UX:** Fixed back-link routing for agent creation and detail pages to consistently return users to the parent project. Improved terminal accessibility by disabling the terminal button for offline agents.
* **Config & Environment Propagation:** Resolved issues with `harnessConfig` propagation during the environment-gathering finalization flow and refined Hub endpoint bridging to only target `localhost` endpoints.
* **Server Reliability:** Applied default `StalledThreshold` values for agent health monitoring and improved status badge readability.

## Mar 3, 2026

This release introduces hierarchical subsystem logging, an integrated browser push notification system, and native support for GKE runtimes and OTLP telemetry.

### 🚀 Features
* **Structured Subsystem Logging:** Introduced a hierarchical, subsystem-based structured logging framework across the Hub and Runtime Broker. This enables more granular observability and easier troubleshooting by isolating logs for specific components like the scheduler, dispatcher, and runtimes.
* **Agent Notifications & Browser Push:** Launched an integrated notification system with real-time SSE delivery and agent-scoped filtering. Features include a new notification tray in the Web UI, opt-in checkboxes for agent creation, and native browser push notification support.
* **Telemetry & OTLP Pipeline:** Added native support for OTLP log receiving and forwarding. The system now supports automated telemetry export with GCP credential injection, manageable via new CLI flags (`--enable-telemetry`) and UI toggles.
* **Stalled Agent Detection:** Implemented a new monitoring system to detect agents that have stopped responding (heartbeat timeout). Stalled agents are now flagged in the UI and can trigger automated notification events.
* **GKE Runtime Support:** Added native support for Google Kubernetes Engine (GKE) runtimes, including cluster provisioning scripts and Workload Identity integration for secure, distributed agent execution.
* **Layout & View Toggles:** Enhanced the Web UI with card/list view toggles for Projects, Agents, and Brokers pages, improving resource visibility for both small and large deployments.
* **Broker Access Control:** Strengthened security by enforcing dispatch authorization checks and resolving creator identities for all registered runtime brokers.

### 🐛 Fixes
* **Terminal UX:** Fixed double-paste and selection-copy bugs in the web terminal.
* **UI Responsiveness:** Resolved an issue where the agent list could incorrectly clear during real-time SSE updates and improved status badge readability.
* **Agent Provisioning:** Prevented root-owned directories in agent home by pre-creating secret and gcloud mount-point directories.
* **Administrative Security:** Hardened the Hub by restricting access to global settings and sensitive resource management (env/secrets) to administrative users.
* **Server Stability:** Fixed scheduler startup in combined mode and resolved heartbeats from defeating stalled agent detection.
* **CLI UX:** Standardized CLI scope flags and corrected secret set syntax for hub-scoped resources.

## Mar 2, 2026

This release focuses on refining the agent lifecycle experience with an overhauled status and activity tracking system, enhanced project-level configuration, and improved CLI flexibility for remote operations.

### 🚀 Features
* **Status & Activity Tracking Overhaul:** Replaced the generic `STATUS` with a more precise `PHASE` column across the CLI and Web UI. Introduced "sticky" activity logic to ensure significant agent actions remain visible during transitions, and enabled real-time status broadcasting via SSE for broker heartbeats.
* **Project Environment & Secret Management:** Launched a dedicated configuration interface for managing project-scoped environment variables and secrets. Includes a new "Injection Mode" selector (Always vs. As-Needed) for granular control over agent environment population.
* **Remote Project Targeting:** Enhanced the `--project` flag to natively accept project slugs and git URLs in Hub mode, streamlining operations on remote workspaces without requiring local configuration.
* **Unified Configuration UX:** Consolidated project-specific configuration into a centralized settings page in the Web UI, utilizing shared components for environment and secret management.

### 🐛 Fixes
* **Container Runtime Compliance:** Fixed an issue where secret volume mounts were incorrectly ordered in container run commands, ensuring reliable mounting across different runtimes.
* **Agent Identity Reliability:** Resolved bugs preventing the consistent propagation of `SCION_AGENT_ID` during restarts and specific dispatch paths, fixing broken notification subscriptions.
* **Linked-Project Pathing:** Corrected workspace resolution for linked projects without git remotes by ensuring fallback to the provider's local filesystem path.
* **UI State Resolution:** Fixed a bug where hub agents would occasionally show an "unknown" phase by ensuring the UI correctly reads the unified Phase and Activity fields.
* **UX Refinements:** Improved the `scion list` output to use human-friendly template names and fixed dynamic label mapping in secret configuration forms.
* **Stability:** Suppressed spurious errors during graceful server shutdown and resolved potential issues with higher-priority environment variable leakage in tests.

## Mar 1, 2026

This release introduces strict runtime enforcement for agent resource limits and includes several critical stability and performance improvements across the server and build pipeline.

### 🚀 Features
* **Agent Resource Limits Enforcement:** Implemented strict runtime enforcement for agent constraints, including `max_turns`, `max_model_calls`, and `max_duration`. Agents exceeding these limits are now automatically transitioned to a `LIMITS_EXCEEDED` state and terminated.

### 🐛 Fixes
* **Bundle Size Optimization:** Implemented vendor chunk splitting in the Vite build process to resolve bundle size warnings and improve frontend load performance.
* **Server Stability:** Resolved a critical panic that occurred during double-close operations in the combined Hub+Web server shutdown sequence.
* **Secret Mapping:** Corrected the mapping of secret type fields and standardized dynamic key/name labels to ensure consistency with backend providers.

## Feb 28, 2026

This release marks a major milestone with the completion of the canonical agent state refactor and the launch of the Hub scheduler system, alongside significant enhancements to real-time observability and broker security.

:::danger[BREAKING CHANGES]
* **Unified State Model:** The legacy `Status` and `SessionStatus` fields have been fully retired in favor of a canonical, layered agent state model. Downstream consumers of the Hub API or `sciontool` status outputs must update to the new schema.
* **Notification Triggers:** In alignment with the state refactor, notification `TriggerStatuses` have been renamed to `TriggerActivities`.

:::

### 🚀 Features
* **Canonical Agent State Refactor:** Completed a comprehensive, multi-phase overhaul of the agent state system across the Hub, Store, Runtime Broker, CLI, and Web UI. This ensures a consistent, high-fidelity representation of agent activity throughout the entire lifecycle.
* **Hub Scheduler & Timer Infrastructure:** Launched a unified scheduling system for recurring Hub tasks and one-shot timers. This includes automated heartbeat timeout detection for "zombie" agents and a new CLI/API for managing scheduled maintenance and lifecycle events.
* **Real-time Debug Observability:** Introduced a full-height debug panel in the Web UI, providing a real-time stream of SSE events and internal state transitions for advanced troubleshooting and observability.
* **Enhanced Web UI Feedback:** Added emoji-based status badges to agent cards and list views, providing more intuitive visual indicators of agent health and activity.
* **Broker Authorization & Identity:** Strengthened security by enforcing dispatch authorization checks and resolving creator identities for all registered runtime brokers.
* **Automated Project Cleanup:** Hardened the hub-managed project lifecycle by implementing cascaded directory cleanup on remote brokers whenever a project is deleted via the Hub.
* **CLI Enhancements:** Added a new `-n/--num-lines` flag to the `scion look` command, enabling tailored views of agent terminal output.

### 🐛 Fixes
* **Notification Dispatcher:** Fixed a bug where the notification dispatcher failed to start when the Hub was running in combined mode with the Web server.
* **Environment Variable Standardization:** Renamed `SCION_SERVER_AUTH_DEV_TOKEN` to `SCION_AUTH_TOKEN` and introduced `SCION_BROKER_ID` and `SCION_TEMPLATE` variables for better debugging and interoperability.
* **Local Secret Storage:** Resolved issues with local secret storage and added diagnostics for environment-gathering resolution.

## Feb 27, 2026

This release focuses on refining the hub-managed project experience, enhancing the web terminal's usability, and introducing new workspace management capabilities via the Hub API.

### 🚀 Features
* **Workspace Management:** Added new Hub API endpoints for downloading individual workspace files and generating ZIP archives of entire projects, facilitating easier data export and backup.
* **Broker Detail View:** Launched a comprehensive broker detail page in the Web UI, providing a grouped view of all active agents by their respective projects for improved operational visibility.
* **Deployment Automation:** Enhanced GCE deployment scripts with new `fast` and `full` modes, streamlining the process of updating Hub and Broker instances in production environments.
* **Iconography Standardization:** Established a centralized icon reference system and updated the web interface to use consistent iconography for resources like projects, templates, and brokers.

### 🐛 Fixes
* **Hub-Managed Path Resolution:** Resolved several critical issues where hub-managed projects incorrectly inherited local filesystem paths from the Hub server. Broker-side initialization of `.scion` directories and explicit path mapping now ensure consistent workspace behavior across distributed brokers.
* **Terminal & Clipboard UX:** Enabled native clipboard copy/paste support in the web terminal and relaxed availability checks to allow terminal access during agent startup and transition states.
* **Real-time Data Integrity:** Fixed a bug in the frontend state manager where SSE delta updates could merge incorrectly; the manager is now reliably seeded with full REST data upon page load.
* **Slug & Case Sensitivity:** Normalized agent slug lookups to lowercase and implemented stricter name validation to prevent routing collisions and inconsistent dispatcher behavior.
* **Environment & Harness Config:** Improved the reliable propagation of harness configurations and environment variables from Hub storage to the runtime broker during both initial agent start and subsequent restarts.
* **UI Refinement:** Replaced text-based labels with intuitive iconography on agent cards to optimize space and improved contrast for neutral status badges.

## Feb 26, 2026

This release introduces a robust capability-based access control system, a dedicated administrative management suite, and critical session management upgrades to support larger authentication payloads.

### 🚀 Features
* **Capability-Based Access Control:** Implemented a comprehensive capability gating system across the Hub API and Web UI. Resource responses now include `_capabilities` annotations, enabling granular UI controls and API-level enforcement for resource operations.
* **Administrative Management Suite:** Launched a new Admin section in the Web UI, providing centralized views for managing users, groups, and brokers. Includes a maintenance mode toggle for Hub and Web servers to facilitate safe infrastructure updates.
* **Advanced Environment & Secret Management:** Introduced a profile-based settings section for managing user-scoped environment variables and secrets. Secrets are now automatically promoted to the configured backend (e.g., GCP Secret Manager) with standardized metadata labels.
* **SSR Data Prefetching:** Improved initial page load performance and eliminated "flash of unauthenticated content" by prefetching critical user and configuration data into the HTML payload via `__SCION_DATA__`.
* **Hub Scheduler Design:** Completed the technical specification for a new Hub scheduler and timer system to manage long-running background tasks and lifecycle events.
* **Enhanced Real-time Monitoring:** Expanded Server-Sent Events (SSE) support to the Brokers list view, ensuring infrastructure status is reflected in real-time without manual refreshes.

### 🐛 Fixes
* **Filesystem Session Store:** Replaced cookie-based session storage with a filesystem-backed store to resolve "400 Bad Request" errors caused by cookie size limits (4096 bytes) during large JWT/OAuth exchanges.
* **Hub-Managed Project Reliability:** Fixed critical 503 errors and path resolution issues during agent creation in hub-managed projects by correctly propagating project slugs to runtime brokers.
* **Agent Deletion Cleanup:** Hardened the agent deletion flow to ensure that stopping and removing an agent in the Hub correctly dispatches cleanup commands to the associated runtime broker and removes local workspace files.
* **Environment Validation:** Improved agent startup safety by treating missing required environment variables as fatal errors (422), preventing agents from starting in incomplete states.
* **Terminal Responsiveness:** Resolved several layout bugs in the web terminal, ensuring it correctly resizes with the viewport and fits within the application shell.
* **Group Persistence:** Fixed synchronization issues between the Hub's primary database and the Ent-backed authorization store, ensuring project-scoped groups and policies are preserved during recreation.

## Feb 25, 2026

This release focuses on hardening the agent provisioning pipeline, streamlining template management through automatic bootstrapping, and enhancing the web authentication experience.

### 🚀 Features
* **Template Bootstrapping:** Local agent templates are now automatically bootstrapped into the Hub database during server startup, ensuring all defined templates are consistently available across the system.
* **Custom ADK Runner Entrypoint:** Introduced a specialized runner entrypoint for Agent Development Kit (ADK) agents with native support for the `--input` flag, facilitating more robust automated execution.
* **Wildcard Subdomain Authorization:** Expanded security configuration to support wildcard subdomain matching in `authorized-domains`, allowing for more flexible deployment architectures.

### 🐛 Fixes
* **Agent Provisioning & Creation:** Resolved multiple issues in the Hub-dispatched agent creation flow, including a 403 authorization fix, rejection of duplicate agent names, and a critical fix for container image resolution.
* **Instruction Injection Logic:** Improved the reliability of agent instructions by implementing auto-detection for `agents.md` and ensuring stale instruction files (e.g., lowercase `claude.md`) are removed during provisioning.
* **Web UI & Auth Persistence:** Fixed a bug where the authenticated user wasn't correctly fetched on page load, ensuring the profile and sign-out options are always visible in the header.
* **Pathing & Scoping:** Corrected path resolution logic to prevent local-path projects from incorrectly using hub-managed paths, and refined the `scion delete --stopped` command to strictly scope to the active project.
* **Environment Gathering:** Fixed a regression in the `env-gather` finalize-env flow to ensure the template slug is correctly preserved throughout the entire provisioning pipeline.
* **Configuration Schema:** Added `task_flag` support to the settings schema and Hub configuration, improving the tracking and validation of agent task states.

## Feb 24, 2026

This release introduces a robust policy-based authorization system, a comprehensive agent notification framework, and significant enhancements to hub-managed projects and schema validation.

:::danger[BREAKING CHANGES]
* **Policy-Based Authorization:** Strictly enforced authorization for agent operations. Agent creation now requires project membership, while interaction (PTY, messaging) and deletion are restricted to the agent's owner (creator) or system administrators.

:::

### 🚀 Features
* **Agent Notifications System:** Launched a multi-phase notification framework enabling real-time subscriptions to agent status events. This includes a new notification dispatcher, Hub API endpoints, and a `--notify` flag in the CLI for status tracking.
* **Harness-Agnostic Templates:** Introduced support for role-based, harness-agnostic agent templates. New fields for `agent_instructions`, `system_prompt`, and `default_harness_config` allow templates to be defined by their role rather than specific LLM implementations.
* **GKE Security Enhancements:** Added a dedicated `gke` runtime configuration option to enable GKE-specific features like Workload Identity, streamlining secure deployments on Google Kubernetes Engine.
* **Hub-Managed Workspace Management:** Advanced hub-managed project capabilities (Phase 3) with new support for direct workspace file management via the Hub API, reducing reliance on external Git repositories.
* **ADK Agent Integration:** Added a specialized example and Docker template for Agent Development Kit (ADK) agents, facilitating the development of custom autonomous agents within the Scion ecosystem.
* **Infrastructure & Models:** Upgraded the default agent model to `gemini-3-flash-preview` and introduced Cloud Build configurations for automated image delivery.

### 🐛 Fixes
* **Schema & Config Synchronization:** Conducted a comprehensive audit and sync between Go configuration structs and JSON schemas. This fixes field naming inconsistencies (e.g., camelCase for `runtimeClassName`) and improves cross-platform validation.
* **Environment Variable Passthrough:** Corrected environment handling to treat empty variable values as implicit host environment passthroughs.
* **Per-Agent Hub Overrides:** Enabled agents to specify custom Hub endpoints directly in their configuration, providing flexibility for agents to report to different Hubs than their parent project.
* **Soft-Delete Configuration:** Added explicit server-side settings for soft-delete retention periods and workspace file preservation.

## Feb 23, 2026

This period focused on major architectural expansions, introducing multi-hub connectivity for runtime brokers and "hub-managed" projects that decouple workspace management from external Git repositories.

### 🚀 Features
* **Multi-Hub Broker Architecture:** Completed a major refactor of the Runtime Broker to support simultaneous connections to multiple Hubs. This includes a new multi-credential store, per-connection heartbeat management, and a "combo mode" that allows a broker to be co-located with one Hub while serving others remotely.
* **Hub-Managed Projects:** Launched "Hub-Managed" projects, enabling the creation of project workspaces directly through the Hub API and Web UI without an external Git repository. These projects are automatically initialized with a seeded `.scion` structure and managed locally by the Hub.
* **Streamlined Workspace Creation:** Introduced a new project creation interface in the Web UI that supports both Git-based repositories and Hub-managed workspaces, including direct Git URL support for quick onboarding.
* **Improved Agent Configuration:** Enhanced the agent creation form with optimized dropdowns and more intuitive labeling, including renaming "Harness" to "Type" for better clarity.

### 🐛 Fixes
* **Web UI Asset Reliability:** Resolved several issues with Shoelace icon rendering by correctly synchronizing the icon manifest, fixing asset serving paths in the Go server, and updating CSP headers to allow data-URI system icons.
* **Template Flexibility:** Updated the template push logic to make the harness type optional, facilitating the use of more generic or agnostic agent templates.
* **Codex Harness Refinement:** Improved the Codex integration by isolating harness documentation into a dedicated `.codex/` subdirectory and removing unnecessary system prompt prepending.

## Feb 22, 2026

This period introduced significant data management features, including agent soft-delete and centralized harness configuration storage, while advancing the secrets management and execution limits infrastructure.

### 🚀 Features
* **Agent Soft-Delete & Restore:** Implemented a complete soft-delete lifecycle for agents. This includes Hub-side archiving, a new `scion restore` command, list filtering for deleted agents, and an automated background purge loop for expired records.
* **Secrets-Gather & Interactive Input:** Enhanced the environment gathering pipeline to support "secrets-gather." Templates can now define required secrets, and the CLI provides interactive prompts to collect missing values, which are then securely backed by the configured secret provider.
* **K8s Native Secret Mounting:** Completed Phase 4 of the secrets strategy, enabling native secret mounting for agents running in Kubernetes. This includes support for GKE CSI drivers and robust fallback paths.
* **Harness Config Hub Storage:** Added Hub-resident storage for harness configurations. This enables centralized management (CRUD), CLI synchronization, and ensures configurations are consistently propagated to brokers during agent creation.
* **Agent Execution Limits:** Introduced Phase 1 of the agent limits infrastructure, including support for `max_turns` and `max_duration` constraints and a new `LIMITS_EXCEEDED` agent state.
* **CLI UX Improvements:** Added a `--all` flag to `scion stop` for bulk agent termination, introduced Hub auth verification with version reporting, and enhanced `scion look` with better visual padding and borders.
* **Web UI & Real-time Updates:** Launched a new "Create Agent" UI, optimized frontend performance by moving to explicit component imports, and enabled real-time project list updates via Server-Sent Events (SSE).

### 🐛 Fixes
* **Provisioning Robustness:** Improved cleanup of provisioning agents during failed or cancelled environment gathering sessions to prevent stale container accumulation.
* **Sync & State Consistency:** Fixed a race condition where Hub synchronization could remove freshly created agents and ensured harness types are correctly propagated during agent sync.
* **Deployment Pipeline:** Corrected the build sequence in GCE deployment scripts to ensure web assets are fully compiled before the Go binary is built.
* **Config Resolution:** Fixed several configuration issues, including profile runtime application, project flag resolution in subdirectories, and Hub environment variable suppression when the Hub is disabled.

## Feb 21, 2026

This period heavily focused on implementing the end-to-end "env-gather" flow to manage environment variables safely, alongside several CLI improvements and runtime fixes.

### 🚀 Features
* **Env-Gather Flow Pipeline:** Implemented a comprehensive environment variable gathering system across the CLI, Hub, and Broker. This includes harness-aware env key extraction, Hub 202 handling with submission endpoints, and broker-side evaluation to finalize the environment prior to agent creation.
* **Agent Context Threading:** Threaded the CLI hub endpoint directly to agent containers and added support for environment variable overrides.
* **Agent Dashboard Enhancements:** The agent details page now displays the `lastSeen` heartbeat as a relative time format.
* **Template Pathing:** Added support for `SCION_EXTRA_PATH` to optionally include template bin directories in the system `PATH`.
* **Build System Upgrades:** Overhauled the Makefile with new standard targets for build, install, test, lint, and web compilation.

### 🐛 Fixes
* **Env-Gather Safety & UX:** Added strict rejection of env-gather in non-interactive modes to prevent unsanctioned variable forwarding. Improved confirmation messaging and added dispatch support for project-scoped agent creation.
* **CLI Output Formatting:** Redirected informational CLI output to `stderr` to ensure `stdout` can be piped cleanly as JSON.
* **Podman Performance:** Fixed slow container provisioning on Podman by directly editing `/etc/passwd` instead of using `usermod`.
* **Profile Parameter Routing:** Corrected the threading of the profile parameter from the CLI through the Hub to the runtime broker.
* **Hub API Accuracy:** The Hub API now correctly surfaces the `harness` type in responses for agent listings.
* **Docker Build Context:** Fixed an issue where the `scion-base` Docker image build was missing the web package context.

## Feb 20, 2026

This period focused heavily on unifying the Hub API and Web Server architectures, refactoring the agent status model, and enhancing the web frontend experience with new routing and pages.

:::danger[BREAKING CHANGES]
* **Status Model:** Consolidated the `SessionStatus` field into the primary `Status` field across the codebase (API, Database, UI). The `WAITING_FOR_INPUT` and `COMPLETED` states are now treated as "sticky" statuses.
* **Server Architecture:** Combined the Hub API and Web server to serve on a single port (`8080`) when both are enabled. API traffic is now routed to `/api/v1/`, resolving CORS issues and simplifying local deployment.

:::

### 🚀 Features
* **Web Frontend Enhancements:** Added a new Brokers list page, implemented full client-side routing for the Vite dev server, and unified OAuth provider detection via a new `/auth/providers` endpoint.
* **Agent Environment:** Added support for injecting harness-specific telemetry and hub environment variables directly into agent containers based on project settings.
* **Git Operations:** Added cloning status indicators and improved git clone config parity during project-scoped agent creation.

### 🐛 Fixes
* **Real-time UI Updates:** Fixed the Server-Sent Events (SSE) format to ensure real-time UI updates correctly broadcast agent state changes.
* **Routing & Port Prioritization:** Fixed port prioritization to use the web port for broker hub endpoints in combined mode, and ensured unhandled `/api/` routes return proper JSON 404 responses.
* **OAuth & Login:** Fixed conditional rendering for the `/login` route and correctly populated OAuth provider attributes during client-side navigation.
* **Container Configuration:** Fixed container image resolution from on-disk harness configurations and normalized YAML key parsing.
* **Status Reporting:** Ensured Hub status reporting correctly respects and preserves the newly unified, sticky statuses.

## Feb 19, 2026

This period represented a major architectural shift, consolidating the web server into a single Go binary, removing dependencies like NATS and Koa, and introducing hub-first remote workspaces via Git.

:::danger[BREAKING CHANGES]
* **Secrets Management:** The system now strictly requires a configured production secret backend (e.g., `gcpsm`) for any secret Set operations across user, project, and runtime broker scopes. Plaintext fallbacks have been removed. Read, list, and delete operations remain functional locally to support data migration.
* **Server Architecture:** The Node.js Koa server and NATS message broker dependencies have been completely retired. The Scion Hub now natively handles web frontend serving, SPA routing, and Server-Sent Events (SSE) via a consolidated Go binary.

:::

### 🚀 Features
* **Hub-First Git Workspaces:** Implemented end-to-end support for creating remote workspaces directly from Git URLs. This integration enables git clone mode across `sciontool init` and the runtime broker pipeline.
* **Web Server & Auth Integration:** Introduced native session management and OAuth routing within the Go web server, alongside a new EventPublisher for real-time SSE streaming.
* **Telemetry & Settings:** Added telemetry injection to the `v1` settings schema. Telemetry configuration now supports hierarchical merging and is automatically bridged into the agent container's environment variables.
* **CLI Additions:** Introduced the `scion look` command for non-interactive terminal viewing. Project initialization now automatically sets up template directories and requires a global project.

### 🐛 Fixes
* **Lifecycle Hooks:** Relocated the cleanup handler to container lifecycle hooks to guarantee reliable execution upon container termination.
* **Settings Overrides:** Fixed configuration parsing to ensure environment variable overrides are correctly applied when loaded from `settings.yaml`.
* **CLI Defaults:** Ensured the `update-default` command consistently targets the global project, and introduced a new `--force` flag.
* **Frontend Assets:** Resolved static asset serving issues by removing an erroneous `StripPrefix` in the router, and fixed client entry point imports.
