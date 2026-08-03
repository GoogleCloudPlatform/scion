# Release Notes (2026-08-02)

An exceptionally productive day with 47 upstream PRs spanning metrics infrastructure (M3+M4), A2A bridge admin integration, auth pipeline decoupling, and a broad sweep of hardening fixes across the hub, CLI, and chat integrations.

## 🚀 Features
* **[Metrics]:** Session metrics reporting and storage (M3) — agent-side telemetry aggregation engine in sciontool, new `MetricsPayload` in StatusUpdate protocol, `agent_session_metrics` Ent schema, and hub ingestion endpoint with auth gating (#972). Session metrics API endpoints and web UI (M4) — `GET /agents/{id}/metrics/summary`, `GET /metrics/session/{id}`, `GET /projects/{id}/metrics/summary` with agent detail metrics tab, agents list stats columns, and project summary view. SQL-level aggregation, IDOR-safe authorization (#989).
* **[A2A]:** A2A bridge added to Hub admin integrations (Phases 1+2) — `KnownPlugin` catalog replacing flat `knownPlugins` list, self-managed install flow, frontend platform support (#992). Phase 3: real config management via admin UI — `Configure()` with atomic snapshot swap, `AdminOverlay` hot-swap, enriched `HealthCheck()` (#995).
* **[Auth]:** Config-driven auth migration (Phases 3-4) — removes all hardcoded per-provider auth fields (`AnthropicAPIKey`, `GeminiAPIKey`, `CodexAPIKey`, etc.) and switch-case tables. All built-in harnesses now use `FromConfig` variants exclusively (#954).
* **[Harness]:** Auto-detect and expose listening ports in agent containers — new `pkg/sciontool/autoexpose/` package scans `/proc/net/tcp{,6}` with diff-based reconciliation on configurable ticker (#953).
* **[Messages]:** Introduce `type:system` message category for hub-generated operational notices (`delivery-failed`, `scheduler`, `port-forward`), with rendering support across all chat integrations (#986). Add `@mention` parsing and `--cc` flag for multi-recipient notification fan-out (#996).
* **[Observability]:** GCP Error Reporting integration — `serviceContext`, `stack_trace` on ERROR+, and `@type` annotation for automatic error detection (#970). Tier 3 subsystem logging tags added to remaining CRUD handlers (#971). Retry with exponential backoff for telemetry pipeline exports (#987).
* **[Web]:** Replace all native `alert()`/`confirm()` with shared Shoelace-based `showToast()`/`showConfirm()` — 39 alerts and 23 confirms migrated across 22 files (#952). Add GCP Project ID and Cloud Logging settings to admin telemetry UI (#981).
* **[Discord]:** Container-to-host path translation in `/scion send` so agent container paths resolve correctly (#955). Add `downloads_path` config for inbound attachments in isolated workspace modes (#978).

## 🔒 Security
* **[Hub]:** Gate all 11 policy API handlers with `requireAdmin` — previously any authenticated user could create, modify, or delete authorization policies (#990).

## 🐛 Fixes
* **[Hub]:** Three-part `as_needed` injection fix chain — two-pass resolution for env-gather completeness so harness `required_env` can be satisfied by `as_needed` secrets (#967); resolve env vars and secrets in `DispatchAgentRestart` to fix missing `GOOGLE_CLOUD_PROJECT` on restart (#968); add second-pass resolution to `DispatchFinalizeEnv` to preserve auto-resolved keys across the gather flow (#994). Exempt file-type and variable-type secrets from `as_needed` filter — they pass through regardless of injection mode (#975).
* **[Hub]:** Decouple `provisionCredentials` from `NoAuth` guard — `no_auth.behavior: drop-to-shell` was starving `GitHubSkillResolver` of convention-based `GH_` tokens, breaking private repo skill resolution (#956).
* **[Hub]:** Derive telemetry project ID from SA credentials for metrics dashboard — hub never extracted GCP project ID from its existing SA key. New shared `pkg/util/gcp/` package (#969). Fix Cloud Monitoring queries using `ALIGN_SUM` instead of `ALIGN_DELTA` for single-point cumulative series (#982). Add missing log output for tier 3 subsystem loggers (#983).
* **[Hub]:** Add unique constraint on policy names per scope, fix NULL `scope_id` for hub-scoped policies (#993). Return 503 when verifying SA without token generator instead of false `Verified=true` (#991). Surface permission errors for secret creation as 403 with actionable guidance instead of generic 500 (#974).
* **[Hub]:** Return HTML error page for browser proxy requests to unexposed ports (#985). Wire `auto_expose_ports` through admin settings API (#984). Reject broker inbound messages to non-running agents with 409 instead of silently swallowing (#963).
* **[Init]:** Run git clone before pre-start hooks — provisioner-created files in `/workspace` caused `isWorkspaceEmpty` to skip the clone (#980). Chown root-owned files after provisioning to prevent undeletable agents (#958). Stop `scion init` from overriding global `default_harness_config` with embedded template defaults (#964).
* **[Runtime]:** Propagate errors from `ImageExists` instead of returning `(false, nil)` for daemon-unreachable (#961). Use detected runtime for broker heartbeat instead of hardcoding `container` (#965).
* **[Discord]:** Correct agent status icons for all phases and activities — stopped/errored/crashed agents previously showed generic play icon (#976). Show error for messages to non-existent agents instead of silently dropping (#959).
* **[Messages]:** Set group message type to `group-set` for recipients instead of `instruction` (#997).
* **[Web]:** Show last meaningful activity (`lastActivityEvent`) instead of heartbeat time in agent list (#979). Sort harness config dropdown alphabetically (#973). Add integrations page title to prevent "Page Not Found" display (#960). Add button spacing and Enter key support to confirm dialog (#977). Prevent text selection during drag-to-pan in agent graph view (#998).
* **[Config]:** Write `project_id` instead of legacy `grove_id` in settings (#951). Check `Content-Type` before falling back to legacy grove endpoint to fix spurious deprecation warnings (#950).
* **[CLI]:** Suppress ASCII banner in agent mode (#988).

## 🔧 Chores
* **[Harness]:** Add `whoami` shim in agent containers mapping to `scion whoami` (#957).
