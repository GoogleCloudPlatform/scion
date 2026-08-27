# Release Notes (2026-08-26)

A transformative day: the Permissions Foundation Phase 1 authorization refactor landed, Cloud Run Instances shipped as a new runtime type for dispatching agents as individual services, a single-node Cloud Run sandbox tier added one-command deploy, Helm chart Phases 2–3 added Cloud SQL proxy and secret placement guards, and a P0 security fix blocked dev-auth on non-loopback interfaces.

## 🔒 Security
* **[Hub]:** **P0:** Refuse dev-auth middleware on non-loopback interfaces — `devAuthMiddleware` auto-logged-in every cookieless request as admin; bound to `0.0.0.0` (the hosted-mode default), this was a publicly reachable unauthenticated admin UI. Startup validation now blocks the combination at two layers (#1307).
* **[Hub]:** Add caller verification to three broker-scoped handlers (`getRuntimeBroker`, `handleBrokerHeartbeat`, `getBrokerProjects`) — path parameter was trusted without verifying the authenticated caller is that broker, creating a cross-tenant authorization gap (#1296).

## 🚀 Features
* **[Auth]:** Permissions Foundation Phase 1 — complete authorization refactor with permissions registry, deterministic policy evaluation, declarative route guards via `UnifiedAuthMiddleware`, `RoleDefinition`/`RoleBinding`/project membership, `CanDelegate` admission gate, agent credential revocation, decision/mutation audit, and Explain API (#1312).
* **[Runtime]:** Cloud Run Instances runtime — dispatch and manage agents as individual Cloud Run services with create/start/stop/destroy lifecycle, NFS workspace and Secret Manager injection, IAP exec connector via WebSocket tunnels, Cloud Logging streams, IAM and service account management (#1302).
* **[Hosted]:** Single-node Cloud Run sandbox tier — one deploy command produces a `run.app` URL where every agent runs as a sandbox inside one Cloud Run Instance. Cheaper, faster to start, shared filesystem. Complements the per-service Cloud Run Instances runtime. 40 files, +6860 lines (#1310).
* **[Helm]:** Phase 2 — Cloud SQL Auth Proxy sidecar with IAM and password-based DSN construction, minimum connection pool floor enforcement (#1309). Phase 3 — session secret sourced exclusively from Kubernetes Secret (never argv/annotation), credential placement scanner, OAuth client credentials support with redacted-projection checksum hashing (#1313).
* **[Secret]:** PATCH endpoint for secret metadata-only updates — `UpdateMeta` backend method (local + GCP) without creating a new Secret Manager version, CLI `scion hub secret update` subcommand, web UI edit-settings dialog with scope validation and path traversal protection (#1303).
* **[Harness]:** API key auth support for antigravity harness — `GEMINI_API_KEY` as a new auth path alongside oauth-token and vertex-ai, with `modelProvider` written to AGY `settings.json` (#1297). Pre-fetch `models.dev` catalog in opencode provisioner for fresh model data before startup (#1308).

## 🐛 Fixes
* **[Hub]:** WebServer reads live operational settings via `AccessSettingsProvider` interface instead of holding a stale by-value copy. Also fixes data race in `handlers_auth.go` (#1300).
* **[Hub]:** Auth preflight recognizes passthrough GCP identity mode — adds passthrough `MetadataMode` to `gcpSAAssigned` condition (#1306). Resolve implicit "default" template when no template specified — adds fallback and fixes HC identity resolution guard (#1305). Skip env-gather block when `noAuth` is true (#1304).
* **[Web]:** Supply detail in SSE `connected` CustomEvent (was null, causing TypeError) and add identity guards to three `addEventListener`-registered handlers for connection safety (#1262).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 25 — Helm Phase 1, auth capture user-scope, DefaultHarnessAuth, ExitCode/ExitReason, sciontool --allow-progeny, grok-build Vertex AI (#1298).
